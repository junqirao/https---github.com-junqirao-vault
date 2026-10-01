package store

import (
	"context"
	"fmt"

	"vault/internal/domain"
)

// UsageDrift 是一次用量重算的结果：被修正的存储库与用户行数。
type UsageDrift struct {
	Repos int
	Users int
}

// accountedUsageSQL 是"单个差异盘计入配额的量"的 SQL 表达式。
//
// ⚠️ 必须与 app 包的 accountedUsage 完全一致：
// 优先实际物理占用，未测量（0）时退回逻辑大小。
// 两处口径一旦分叉，就会重新出现"账目与实际不一致 → 用量漂移甚至负数"。
const accountedUsageSQL = `CASE WHEN d.physical_bytes > 0 THEN d.physical_bytes ELSE d.size_bytes END`

// RecomputeUsage 以 disks 表为**唯一真源**重算存储库与用户的已用量，返回被修正的行数。
//
// 为什么需要：已用量是"分配时按逻辑大小预留 → 建盘后按实际物理占用校正 → 删除时回退"
// 的增量账。只要某一步的金额与账上不一致（历史缺陷：删除回退用的是逻辑大小、
// 账上记的却是物理大小），差额就会逐次累积，甚至把用量退成负数
// （真实工单：用户配额显示 -7.98 GB / 3.91 TB）。
//
// 正常情况下本方法是个**空操作**（WHERE 条件不匹配任何行），因此可以安全地周期性执行，
// 既修掉历史脏数据，也把任何新的漂移在下一轮对账收敛。
func (s *Store) RecomputeUsage(ctx context.Context) (UsageDrift, error) {
	var drift UsageDrift
	now := nowMillis()
	diffKind := string(domain.DiskKindDiff)

	const reposQuery = `
		UPDATE repositories
		   SET used_bytes = COALESCE((
		         SELECT SUM(` + accountedUsageSQL + `)
		           FROM disks d
		          WHERE d.repo_id = repositories.id AND d.kind = ?), 0),
		       updated_at = ?
		 WHERE used_bytes <> COALESCE((
		         SELECT SUM(` + accountedUsageSQL + `)
		           FROM disks d
		          WHERE d.repo_id = repositories.id AND d.kind = ?), 0)`

	repos, err := s.q.ExecContext(ctx, s.q.Rebind(reposQuery), diffKind, now, diffKind)
	if err != nil {
		return drift, fmt.Errorf("store: 重算存储库用量失败: %w", err)
	}
	if n, aErr := repos.RowsAffected(); aErr == nil {
		drift.Repos = int(n)
	}

	const usersQuery = `
		UPDATE users
		   SET used_bytes = COALESCE((
		         SELECT SUM(` + accountedUsageSQL + `)
		           FROM disks d
		           JOIN allocations a ON a.disk_id = d.id
		          WHERE a.user_id = users.id AND d.kind = ?), 0),
		       updated_at = ?
		 WHERE used_bytes <> COALESCE((
		         SELECT SUM(` + accountedUsageSQL + `)
		           FROM disks d
		           JOIN allocations a ON a.disk_id = d.id
		          WHERE a.user_id = users.id AND d.kind = ?), 0)`

	users, err := s.q.ExecContext(ctx, s.q.Rebind(usersQuery), diffKind, now, diffKind)
	if err != nil {
		return drift, fmt.Errorf("store: 重算用户用量失败: %w", err)
	}
	if n, aErr := users.RowsAffected(); aErr == nil {
		drift.Users = int(n)
	}
	return drift, nil
}

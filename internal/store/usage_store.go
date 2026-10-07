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

// diskStateErrorSQL 是"建盘终态失败"在 SQL 里的字面量。
const diskStateErrorSQL = string(domain.DiskStateError)

// diskStateUnbuiltSQL 是"盘还没建好 / 正在回收"在 SQL 里的状态字面量：
// 这些盘子还不存在（或即将不存在），用量按逻辑大小预留。
const diskStateUnbuiltSQL = `'` + string(domain.DiskStateCreating) + `', '` +
	string(domain.DiskStateDeleting) + `'`

// accountedUsageSQL 是"单个差异盘计入配额的量"的 SQL 表达式。
//
// 口径（唯一真源是 disks 表，见 RecomputeUsage / RecomputeRepoUsage）：
//
//  1. 有实测物理占用 → 用它，这才是"实际占了多少"；
//  2. 建盘终态失败（state=error）且没测到占用 → 计 0。这份盘根本不存在，
//     若按标称容量计费就是永久挂账（真实反馈："我一点空间都没用就占了 1G"）；
//  3. 尚未建好的盘（creating / deleting）→ 退回逻辑大小，与"分配时按逻辑预留"的口径一致。
//     否则会在建盘完成前把用量算成 0，并发分配直接超卖；
//  4. 已建好的盘（ready / published）→ 按实测值记账，**实测 0 就是 0**。
//
// 第 4 条是踩过坑的：thin 差异盘刚派生出来时独占块本来就是 0（所有块都与母盘共享），
// 这不是"没测到"。早先靠 physical_bytes > 0 判"测没测到"，于是这份 0 被当成"尚未采样"
// 退回标称容量，而库容量恰好等于那块盘的容量 —— 新建的库一上来就显示"已用 100%"。
const accountedUsageSQL = `CASE WHEN d.physical_bytes > 0 THEN d.physical_bytes
	       WHEN d.state = '` + diskStateErrorSQL + `' THEN 0
	       WHEN d.state IN (` + diskStateUnbuiltSQL + `) THEN d.size_bytes
	       ELSE d.physical_bytes END`

// repoUsageSQL 是"单个存储库已用量"的派生表达式（只有一个 ? ：存储库 ID）。
const repoUsageSQL = `SELECT COALESCE(SUM(` + accountedUsageSQL + `), 0)
	        FROM disks d
	       WHERE d.repo_id = ? AND d.kind = ?`

// userUsageSQL 是"单个用户已用量"的派生表达式（只有一个 ? ：用户 ID）。
//
// 按"分配↔磁盘"关联计费：少了分配行就等于这笔用量无人认领。
const userUsageSQL = `SELECT COALESCE(SUM(` + accountedUsageSQL + `), 0)
	        FROM disks d
	        JOIN allocations a ON a.disk_id = d.id
	       WHERE a.user_id = ? AND d.kind = ?`

// RecomputeRepoUsage 以 disks 表为唯一真源重算**单个**存储库的已用量并写回，返回重算值。
//
// 为什么需要"单个"的版本：删除/校正一个差异盘只会影响它所在的库（与它的用户），
// 调用方（app 层）在盘记录变更后必须按实际占用重算，而**不能**按记忆的金额做减法 ——
// 账上记的是建盘那一刻的物理占用，差异盘之后还会长大。
func (s *Store) RecomputeRepoUsage(ctx context.Context, repoID string) (int64, error) {
	var used int64
	if err := s.q.GetContext(ctx, &used, s.q.Rebind(repoUsageSQL), repoID, string(domain.DiskKindDiff)); err != nil {
		return 0, fmt.Errorf("store: 重算存储库用量失败: %w", err)
	}
	if _, err := s.q.ExecContext(ctx, s.q.Rebind(
		`UPDATE repositories SET used_bytes = ?, updated_at = ? WHERE id = ?`),
		used, nowMillis(), repoID); err != nil {
		return 0, fmt.Errorf("store: 写回存储库用量失败: %w", err)
	}
	return used, nil
}

// RecomputeUserUsage 以 disks 表为唯一真源重算**单个**用户的已用量并写回，返回重算值。
func (s *Store) RecomputeUserUsage(ctx context.Context, userID string) (int64, error) {
	var used int64
	if err := s.q.GetContext(ctx, &used, s.q.Rebind(userUsageSQL), userID, string(domain.DiskKindDiff)); err != nil {
		return 0, fmt.Errorf("store: 重算用户用量失败: %w", err)
	}
	if _, err := s.q.ExecContext(ctx, s.q.Rebind(
		`UPDATE users SET used_bytes = ?, updated_at = ? WHERE id = ?`),
		used, nowMillis(), userID); err != nil {
		return 0, fmt.Errorf("store: 写回用户用量失败: %w", err)
	}
	return used, nil
}

// RecomputeUsage 以 disks 表为**唯一真源**重算存储库与用户的已用量，返回被修正的行数。
//
// 为什么需要：已用量一旦靠"增量维护"，只要某一步的金额与账上不一致，差额就会逐次累积，
// 甚至把用量退成负数（真实工单：删除差异盘后存储库已用 -100M、用户配额 -7.98 GB / 3.91 TB）。
//
// 正常情况下本方法是个**空操作**（WHERE 条件不匹配任何行），因此可以安全地周期性执行，
// 既修掉历史脏数据，也把任何新的漂移在下一轮对账收敛。
func (s *Store) RecomputeUsage(ctx context.Context) (UsageDrift, error) {
	var drift UsageDrift
	now := nowMillis()
	diffKind := string(domain.DiskKindDiff)

	// 全量版必须写成**关联子查询**（按 repositories.id / users.id 逐行聚合），
	// 因此与 repoUsageSQL / userUsageSQL 相比只有"按哪一列过滤"不同，计费口径共用同一份表达式。
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

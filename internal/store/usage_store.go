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

// allocationReleasedSQL 是"分配已释放"在 SQL 里的字面量。
const allocationReleasedSQL = string(domain.AllocationStateReleased)

// accountedUsageSQL 是"单块盘计入配额的量"的 SQL 表达式（引用 disks 的别名 d）。
//
// 口径：**已用 = 已分配磁盘总量**（唯一真源仍是 disks 表，见 RecomputeUsage）。
//
//  1. 盘被分配出去（存在未释放的分配行，见 allocatedDiskSQL）就按它的**标称容量**
//     （size_bytes）计费，与盘里实际写了多少（physical_bytes）无关 —— 动态盘 / thin
//     差异盘的实测占用常常接近 0，按实测记账会让"已分配 2TB"的库显示成"用了 5MB"，
//     配额也就形同虚设。
//
//     共享库的差异盘同样按母盘容量整份计：100G 的库分给 5 个人，就是每人扣 100G。
//     曾经有一列 accounted_bytes（"共享磁盘限制"）用来把这个数调小，但它只是个记账
//     口径 —— 差异盘的虚拟容量必须等于母盘（客户端按容量匹配本地 iSCSI 盘，见
//     Lease.DiskSizeBytes），既改变不了盘的实际大小，也不能限制单卷的物理增长
//     （见 platform/linuxlvm 的 checkWatermark 注释），已经整体移除。
//
//  2. 建盘终态失败（state=error）计 0：这份盘根本不存在，按容量计费就是永久挂账
//     （真实反馈："我一点空间都没用就占了 1G"），见 DiskService.MarkCreateFailed；
//
//  3. 没被分配出去的盘（母盘、建库时预创建的池位盘）被 allocatedDiskSQL 挡在账外，
//     进不了这个表达式。
//
// 顺序要紧：error 判在最前面 —— 失败盘不占账。
const accountedUsageSQL = `CASE WHEN d.state = '` + diskStateErrorSQL + `' THEN 0
	       ELSE d.size_bytes END`

// allocatedDiskSQL 是"这块盘当前有未释放的分配"的过滤条件（引用 disks 的别名 d）。
//
// 唯一判据是 allocations 表：有未释放、且不属于 @temp 占位用户的分配行才算"已分配"。
//
// 为什么必须按分配行、而不是"库名下的盘点数"：池化共享库在建库那一刻就预创建了
// "共享数量"个池位盘（见 runPrepareRepo），它们还挂在池里没给任何人；按盘计费的话，
// 一个共享数量 5、容量 1TB 的库刚建好就会显示"已用 5TB"。
// 母盘的"临时共享"占位分配（user_id = @temp）同理不算分配：那是维护用的只读映射。
const allocatedDiskSQL = `EXISTS (SELECT 1 FROM allocations a
	         WHERE a.disk_id = d.id AND a.state <> '` + allocationReleasedSQL + `'
	           AND a.user_id <> '` + domain.TempAllocationUserID + `')`

// repoUsageSQL 是"单个存储库已用量"的派生表达式（只有一个 ? ：存储库 ID）。
const repoUsageSQL = `SELECT COALESCE(SUM(` + accountedUsageSQL + `), 0)
	        FROM disks d
	       WHERE d.repo_id = ? AND ` + allocatedDiskSQL

// userUsageSQL 是"单个用户已用量"的派生表达式（只有一个 ? ：用户 ID）。
//
// 按"分配↔磁盘"关联计费：少了分配行就等于这笔用量无人认领。
const userUsageSQL = `SELECT COALESCE(SUM(` + accountedUsageSQL + `), 0)
	        FROM disks d
	       WHERE EXISTS (SELECT 1 FROM allocations a
	                      WHERE a.disk_id = d.id AND a.user_id = ?
	                        AND a.state <> '` + allocationReleasedSQL + `')`

// RecomputeRepoUsage 以 disks 表为唯一真源重算**单个**存储库的已用量并写回，返回重算值。
//
// 为什么需要"单个"的版本：分配/释放一个盘只会影响它所在的库（与它的用户），
// 调用方（app 层）在盘记录变更后必须按盘记录重算，而**不能**按记忆的金额做加减 ——
// 账上记的是"已分配出去的盘子容量"，只有 disks 表知道当前还有哪些盘被分配着。
func (s *Store) RecomputeRepoUsage(ctx context.Context, repoID string) (int64, error) {
	var used int64
	if err := s.q.GetContext(ctx, &used, s.q.Rebind(repoUsageSQL), repoID); err != nil {
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
	if err := s.q.GetContext(ctx, &used, s.q.Rebind(userUsageSQL), userID); err != nil {
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

	// 全量版必须写成**关联子查询**（按 repositories.id / users.id 逐行聚合），
	// 因此与 repoUsageSQL / userUsageSQL 相比只有"按哪一列过滤"不同，计费口径共用同一份表达式。
	const reposQuery = `
		UPDATE repositories
		   SET used_bytes = COALESCE((
		         SELECT SUM(` + accountedUsageSQL + `)
		           FROM disks d
		          WHERE d.repo_id = repositories.id AND ` + allocatedDiskSQL + `), 0),
		       updated_at = ?
		 WHERE used_bytes <> COALESCE((
		         SELECT SUM(` + accountedUsageSQL + `)
		           FROM disks d
		          WHERE d.repo_id = repositories.id AND ` + allocatedDiskSQL + `), 0)`

	repos, err := s.q.ExecContext(ctx, s.q.Rebind(reposQuery), now)
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
		          WHERE EXISTS (SELECT 1 FROM allocations a
		                         WHERE a.disk_id = d.id AND a.user_id = users.id
		                           AND a.state <> '` + allocationReleasedSQL + `')), 0),
		       updated_at = ?
		 WHERE used_bytes <> COALESCE((
		         SELECT SUM(` + accountedUsageSQL + `)
		           FROM disks d
		          WHERE EXISTS (SELECT 1 FROM allocations a
		                         WHERE a.disk_id = d.id AND a.user_id = users.id
		                           AND a.state <> '` + allocationReleasedSQL + `')), 0)`

	users, err := s.q.ExecContext(ctx, s.q.Rebind(usersQuery), now)
	if err != nil {
		return drift, fmt.Errorf("store: 重算用户用量失败: %w", err)
	}
	if n, aErr := users.RowsAffected(); aErr == nil {
		drift.Users = int(n)
	}
	return drift, nil
}

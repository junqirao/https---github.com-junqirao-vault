package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"vault/internal/domain"
	"vault/internal/store"
)

// seedRepoWithOwner 建一个用户 + 存储库，返回 (存储库 ID, 用户 ID)。
func seedRepoWithOwner(t *testing.T, st *store.Store) (string, string) {
	t.Helper()
	ctx := context.Background()
	user := &domain.User{Username: "tester", Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("建用户失败：%v", err)
	}
	repo := &domain.Repository{
		Name:    "repo-1",
		Mode:    domain.RepoModeShared,
		OwnerID: user.ID,
		State:   domain.RepoStateActive,
	}
	if err := st.CreateRepository(ctx, repo); err != nil {
		t.Fatalf("建存储库失败：%v", err)
	}
	return repo.ID, user.ID
}

// createDiffDisk 建一个差异盘记录，并**同时建出它的分配记录**——
// 真实流程（repo.Allocate）就是"先建分配、再建磁盘"，用量重算按"分配↔磁盘"关联计费，
// 少了分配行就等于这笔用量无人认领。
func createDiffDisk(t *testing.T, st *store.Store, repoID, userID string, size, physical int64) *domain.Disk {
	t.Helper()
	ctx := context.Background()
	disk := &domain.Disk{
		RepoID:        repoID,
		Kind:          domain.DiskKindDiff,
		VHDXPath:      filepath.Join("C:", "vault", "disks", "diffs", "x.vhdx"),
		SizeBytes:     size,
		PhysicalBytes: physical,
		State:         domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}
	if err := st.CreateAllocation(ctx, &domain.Allocation{
		ID:     "alloc-" + disk.ID,
		RepoID: repoID,
		DiskID: disk.ID,
		UserID: userID,
		State:  domain.AllocationStateAllocated,
	}); err != nil {
		t.Fatalf("建分配记录失败：%v", err)
	}
	return disk
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// assertUsage 断言存储库与用户的已用量都等于 want，且**永不为负**。
func assertUsage(t *testing.T, st *store.Store, repoID, userID string, want int64) {
	t.Helper()
	ctx := context.Background()
	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	user, err := st.GetUserByID(ctx, userID)
	if err != nil {
		t.Fatalf("回查用户失败：%v", err)
	}
	if repo.UsedBytes != want {
		t.Fatalf("存储库已用 = %d，期望 %d（负数即为历史缺陷）", repo.UsedBytes, want)
	}
	if user.UsedBytes != want {
		t.Fatalf("用户已用 = %d，期望 %d（负数即为历史缺陷）", user.UsedBytes, want)
	}
}

// TestUsageReserveCorrectReleaseBalances 覆盖一次完整生命周期的账目闭环：
// 分配预留（逻辑大小）→ 建盘校正（实测物理）→ 采样（盘长大了）→ 删除（归零）。
//
// 关键回归（真实工单）：**删掉一个差异盘后存储库已用变成 -100M**。
// 根因是删除时按"记忆的金额"做减法，而账上记的是建盘那一刻的物理占用，
// 差异盘之后被写入撑大（采样只改 physical_bytes，不动账），于是减掉了从未计入的增量。
// 因此这里要求：账目在每一步都等于 disks 表的派生值，并且删除后必须是 0 而不是负数。
func TestUsageReserveCorrectReleaseBalances(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	const size int64 = 2 << 30      // 逻辑 2GB（分配时的预留）
	const atCreate int64 = 20 << 20 // 建盘那一刻的物理占用
	const grown int64 = 100 << 20   // 之后被写入撑到 100M（正是 -100M 的来路）

	// 1) 分配：按逻辑大小预留（对应 repo.go 的 Allocate）。
	disk := createPendingDiffDisk(t, st, repoID, userID, size)
	if _, err := st.AddRepoUsedBytes(ctx, repoID, size); err != nil {
		t.Fatalf("预留存储库用量失败：%v", err)
	}
	if _, err := st.AddUserUsedBytes(ctx, userID, size); err != nil {
		t.Fatalf("预留用户用量失败：%v", err)
	}

	svc := &DiskService{Deps: Deps{
		Store: st,
		Log:   testLogger(),
		Disk:  fakeDiskBackend{exists: true, physicalSize: grown},
	}}

	// 2) 建盘完成：实测物理占用落库 → 账目校正为实测值（对应 runCreateDiff）。
	disk.PhysicalBytes = atCreate
	disk.State = domain.DiskStateReady
	if err := st.UpdateDisk(ctx, disk); err != nil {
		t.Fatalf("写回磁盘失败：%v", err)
	}
	svc.resyncUsage(ctx, repoID, userID)
	assertUsage(t, st, repoID, userID, atCreate)

	// 3) 差异盘随写入变大：采样把 physical_bytes 改成 100M，账目必须跟着走到实际占用。
	if err := svc.SamplePhysicalUsage(ctx); err != nil {
		t.Fatalf("采样失败：%v", err)
	}
	assertUsage(t, st, repoID, userID, grown)

	// 4) 删除该差异盘：账目必须归零，不得出现 -100M。
	if err := svc.deleteDiskArtifacts(ctx, disk.ID, "alloc-"+disk.ID); err != nil {
		t.Fatalf("删除磁盘失败：%v", err)
	}
	assertUsage(t, st, repoID, userID, 0)
}

// TestRecomputeRepoUsageIsIdempotent 定向重算必须幂等，且以 disks 表为准。
func TestRecomputeRepoUsageIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	const physical int64 = 5 << 20
	disk := createDiffDisk(t, st, repoID, userID, 2<<30, physical)

	used, err := st.RecomputeRepoUsage(ctx, repoID)
	if err != nil {
		t.Fatalf("重算存储库用量失败：%v", err)
	}
	if used != physical {
		t.Fatalf("重算结果 = %d，期望实际占用 %d", used, physical)
	}
	used, err = st.RecomputeRepoUsage(ctx, repoID)
	if err != nil {
		t.Fatalf("二次重算失败：%v", err)
	}
	if used != physical {
		t.Fatalf("二次重算结果 = %d，期望 %d（重算必须幂等）", used, physical)
	}

	// 盘记录消失后重算必须归零 —— 删除路径正是靠这一点把占用退干净。
	// 顺序与真实删除路径一致：先删分配（外键指向磁盘），再删磁盘。
	if err := st.DeleteAllocation(ctx, "alloc-"+disk.ID); err != nil {
		t.Fatalf("删除分配失败：%v", err)
	}
	if err := st.DeleteDisk(ctx, disk.ID); err != nil {
		t.Fatalf("删除磁盘失败：%v", err)
	}
	if used, err = st.RecomputeRepoUsage(ctx, repoID); err != nil || used != 0 {
		t.Fatalf("盘已删除后重算 = %d（err=%v），期望 0", used, err)
	}
}

// TestRecomputeUsageRepairsNegative 覆盖"以磁盘实际占用为唯一真源"的用量重算：
// 历史脏数据（负数）必须被修回正确值，且重复执行是空操作。
func TestRecomputeUsageRepairsNegative(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	const size int64 = 2 << 30
	const physical int64 = 6 << 20
	createDiffDisk(t, st, repoID, userID, size, physical)

	// 母盘不计入用量（只按差异盘计费）：即便存在也不该被算进去。
	if err := st.CreateDisk(ctx, &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindParent,
		VHDXPath:  filepath.Join("C:", "vault", "disks", "parents", "p.vhdx"),
		SizeBytes: 10 << 30,
		State:     domain.DiskStateReady,
	}); err != nil {
		t.Fatalf("建母盘记录失败：%v", err)
	}

	// 造出历史脏数据：账上被退成负数。
	if _, err := st.AddUserUsedBytes(ctx, userID, -8<<30); err != nil {
		t.Fatalf("构造脏数据失败：%v", err)
	}
	if _, err := st.AddRepoUsedBytes(ctx, repoID, -8<<30); err != nil {
		t.Fatalf("构造脏数据失败：%v", err)
	}

	drift, err := st.RecomputeUsage(ctx)
	if err != nil {
		t.Fatalf("重算失败：%v", err)
	}
	if drift.Users != 1 || drift.Repos != 1 {
		t.Fatalf("应各修正 1 行，实际 repos=%d users=%d", drift.Repos, drift.Users)
	}

	user, err := st.GetUserByID(ctx, userID)
	if err != nil {
		t.Fatalf("回查用户失败：%v", err)
	}
	if user.UsedBytes != physical {
		t.Fatalf("用户用量应为物理占用 %d，实际=%d", physical, user.UsedBytes)
	}
	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	if repo.UsedBytes != physical {
		t.Fatalf("存储库用量应为物理占用 %d，实际=%d", physical, repo.UsedBytes)
	}

	// 幂等：账目已一致时不得再改动任何行。
	again, err := st.RecomputeUsage(ctx)
	if err != nil {
		t.Fatalf("二次重算失败：%v", err)
	}
	if again.Users != 0 || again.Repos != 0 {
		t.Fatalf("账目一致时重算应为空操作，实际 repos=%d users=%d", again.Repos, again.Users)
	}
}

// TestRecomputeUsageFallsBackToLogicalSize 物理占用尚未测出（0）时按逻辑大小计费，
// 与"分配时按逻辑预留"的口径保持一致（否则会在建盘完成前把用量算成 0）。
func TestRecomputeUsageFallsBackToLogicalSize(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	const size int64 = 3 << 30
	createDiffDisk(t, st, repoID, userID, size, 0) // 物理未知

	if _, err := st.RecomputeUsage(ctx); err != nil {
		t.Fatalf("重算失败：%v", err)
	}
	user, err := st.GetUserByID(ctx, userID)
	if err != nil {
		t.Fatalf("回查用户失败：%v", err)
	}
	if user.UsedBytes != size {
		t.Fatalf("物理未知时应按逻辑大小 %d 计费，实际=%d", size, user.UsedBytes)
	}
}

// TestRepoCapacityDerivedFromStartDisk 守住存储库卡片进度条的**分母**（库容量）。
//
// 库容量不落 repositories 表，是读时从"建库那块盘"派生的（见 store.repoCapacitySQL）。
// 一旦这个派生失效（例如只认 parent_disk_id、或误用 quota_bytes），卡片上的
// 「已用 / 容量」就会退化成"没有分母、进度条永远空着"——那正是用户报的问题。
func TestRepoCapacityDerivedFromStartDisk(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	// 还没建盘：派生不出容量，必须是 0（前端据此回退成不显示分母，而不是除以 0）。
	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	if repo.CapacityBytes != 0 {
		t.Fatalf("无建库盘时容量应为 0，实际=%d", repo.CapacityBytes)
	}

	// 建库时创建的那块盘（共享模式 = 母盘），容量 10GB。
	const size int64 = 10 << 30
	parent := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindParent,
		VHDXPath:  filepath.Join("C:", "vault", "disks", "parents", "base.vhdx"),
		SizeBytes: size,
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, parent); err != nil {
		t.Fatalf("建母盘记录失败：%v", err)
	}
	pid := parent.ID
	repo.ParentDiskID = &pid
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("回填母盘失败：%v", err)
	}

	// 单查与列表都要带上派生列（卡片走的是列表接口）。
	got, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	if got.CapacityBytes != size {
		t.Fatalf("库容量应派生自母盘 = %d，实际=%d", size, got.CapacityBytes)
	}
	list, err := st.ListRepositories(ctx, "", 10, 0)
	if err != nil {
		t.Fatalf("查询存储库列表失败：%v", err)
	}
	if len(list) != 1 || list[0].CapacityBytes != size {
		t.Fatalf("列表里的库容量 = %v，期望 1 条、容量 %d", list, size)
	}
}

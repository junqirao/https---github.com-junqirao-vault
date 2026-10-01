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

// TestUsageReserveCorrectReleaseBalances 覆盖一次完整生命周期的账目闭环：
// 分配预留（逻辑大小）→ 建盘校正（实测物理占用）→ 删除回退。
//
// 关键回归：回退必须与"账上实际记的金额"一致。历史实现在回退时用了逻辑大小，
// 而账上记的是物理大小，于是每删一个差异盘就多退差额，最终把用户配额退成负数
// （真实工单：-7.98 GB / 3.91 TB）。
func TestUsageReserveCorrectReleaseBalances(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	const size int64 = 2 << 30     // 逻辑 2GB（分配时的预留）
	const physical int64 = 8 << 20 // 实测物理 8MB（差异盘是稀疏的）

	// 1) 分配：按逻辑大小预留（对应 repo.go 的 Allocate）。
	if _, err := st.AddRepoUsedBytes(ctx, repoID, size); err != nil {
		t.Fatalf("预留存储库用量失败：%v", err)
	}
	if _, err := st.AddUserUsedBytes(ctx, userID, size); err != nil {
		t.Fatalf("预留用户用量失败：%v", err)
	}

	// 2) 建盘完成：按实测物理占用校正（对应 runCreateDiff）。
	disk := createDiffDisk(t, st, repoID, userID, size, physical)
	correction := physical - size
	if _, err := st.AddRepoUsedBytes(ctx, repoID, correction); err != nil {
		t.Fatalf("校正存储库用量失败：%v", err)
	}
	if _, err := st.AddUserUsedBytes(ctx, userID, correction); err != nil {
		t.Fatalf("校正用户用量失败：%v", err)
	}

	// 3) 删除差异盘：回退（对应 DiskService.releaseUsage）。
	svc := &DiskService{Deps: Deps{Store: st, Log: testLogger()}}
	svc.releaseUsage(ctx, disk, &domain.Allocation{UserID: userID, DiskID: disk.ID})

	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	user, err := st.GetUserByID(ctx, userID)
	if err != nil {
		t.Fatalf("回查用户失败：%v", err)
	}
	if repo.UsedBytes != 0 {
		t.Fatalf("存储库用量应回到 0，实际=%d", repo.UsedBytes)
	}
	if user.UsedBytes != 0 {
		t.Fatalf("用户用量应回到 0（负数即为历史缺陷），实际=%d", user.UsedBytes)
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

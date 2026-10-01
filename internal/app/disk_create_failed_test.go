package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"vault/internal/domain"
	"vault/internal/store"
)

// createPendingDiffDisk 建一个**仍在创建中**的差异盘及其分配（对应"分配后、建盘未完成"）。
func createPendingDiffDisk(t *testing.T, st *store.Store, repoID, userID string, size int64) *domain.Disk {
	t.Helper()
	ctx := context.Background()
	disk := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindDiff,
		VHDXPath:  filepath.Join("C:", "vault", "disks", "diffs", "pending.vhdx"),
		SizeBytes: size,
		State:     domain.DiskStateCreating,
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

// TestMarkCreateFailedReleasesReservedUsage 覆盖"建盘终态失败必须回退预留用量"。
//
// 真实反馈："我一点空间都没用就占了 1G" —— 分配时按**母盘标称容量**预留用量，差异盘
// 建完后才按实测物理占用校正（见 runCreateDiff）。建盘彻底失败时校正永远不会发生，
// 那份预留就成了永久挂账（每个失败的分配 1GB 起），而用户在界面上什么都没用。
//
// 幂等是硬要求：对账会反复调用 MarkCreateFailed（兜住历史数据与崩溃场景），
// 重复回退会把用量退成负数（历史工单：-7.98 GB）。
func TestMarkCreateFailedReleasesReservedUsage(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	const reserved int64 = 1 << 30 // 预留 1GiB（母盘标称容量）
	disk := createPendingDiffDisk(t, st, repoID, userID, reserved)

	// 1) 分配：按母盘标称容量预留。
	if _, err := st.AddRepoUsedBytes(ctx, repoID, reserved); err != nil {
		t.Fatalf("预留存储库用量失败：%v", err)
	}
	if _, err := st.AddUserUsedBytes(ctx, userID, reserved); err != nil {
		t.Fatalf("预留用户用量失败：%v", err)
	}

	svc := &DiskService{Deps: Deps{Store: st, Log: testLogger()}}

	// 2) 建盘彻底失败 → 预留必须回退，磁盘进入 error（不再永远停在 creating）。
	if err := svc.MarkCreateFailed(ctx, disk.ID, errors.New("create diff failed")); err != nil {
		t.Fatalf("MarkCreateFailed 失败：%v", err)
	}
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
		t.Fatalf("用户用量应回到 0，实际=%d", user.UsedBytes)
	}
	got, err := st.GetDisk(ctx, disk.ID)
	if err != nil {
		t.Fatalf("回查磁盘失败：%v", err)
	}
	if got.State != domain.DiskStateError {
		t.Fatalf("磁盘状态 = %q，期望 %q", got.State, domain.DiskStateError)
	}

	// 3) 再次调用（对账重复兜底）→ 不得重复回退（否则用量变负）。
	if err := svc.MarkCreateFailed(ctx, disk.ID, errors.New("again")); err != nil {
		t.Fatalf("重复 MarkCreateFailed 失败：%v", err)
	}
	repo, _ = st.GetRepository(ctx, repoID)
	user, _ = st.GetUserByID(ctx, userID)
	if repo.UsedBytes != 0 || user.UsedBytes != 0 {
		t.Fatalf("重复回滚不得再退用量：repo=%d user=%d", repo.UsedBytes, user.UsedBytes)
	}
}

// TestMarkCreateFailedSkipsParentDisk 母盘不计费（只按差异盘计费），失败时也不得退账 ——
// 否则会把没有预留过的量退成负数（历史工单：-7.98 GB）。
func TestMarkCreateFailedSkipsParentDisk(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	parent := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindParent,
		VHDXPath:  filepath.Join("C:", "vault", "disks", "parents", "p.vhdx"),
		SizeBytes: 10 << 30,
		State:     domain.DiskStateCreating,
	}
	if err := st.CreateDisk(ctx, parent); err != nil {
		t.Fatalf("建母盘失败：%v", err)
	}

	svc := &DiskService{Deps: Deps{Store: st, Log: testLogger()}}
	if err := svc.MarkCreateFailed(ctx, parent.ID, errors.New("boom")); err != nil {
		t.Fatalf("MarkCreateFailed 失败：%v", err)
	}
	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	if repo.UsedBytes != 0 {
		t.Fatalf("母盘失败不应产生负账：used=%d", repo.UsedBytes)
	}
}

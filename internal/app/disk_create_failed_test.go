package app

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"vault/internal/domain"
	"vault/internal/job"
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

// TestMarkCreateFailedReleasesReservedUsage 覆盖"建盘终态失败必须回退计费用量"。
//
// 真实反馈："我一点空间都没用就占了 1G" —— 配额口径是**已用 = 已分配磁盘总量**，
// 分配那一刻就按标称容量计费（见 repo.Allocate）。建盘彻底失败时这块盘根本不存在，
// 不能被它永远占着配额（每个失败的分配 1GB 起），而用户在界面上什么都没拿到。
//
// 幂等是硬要求：对账会反复调用 MarkCreateFailed（兜住历史数据与崩溃场景），
// 重复回退会把用量退成负数（历史工单：-7.98 GB）。
func TestMarkCreateFailedReleasesReservedUsage(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	const reserved int64 = 1 << 30 // 计费 1GiB（母盘标称容量）
	disk := createPendingDiffDisk(t, st, repoID, userID, reserved)

	// 1) 分配：按标称容量计费。
	if _, err := st.AddRepoUsedBytes(ctx, repoID, reserved); err != nil {
		t.Fatalf("计费存储库用量失败：%v", err)
	}
	if _, err := st.AddUserUsedBytes(ctx, userID, reserved); err != nil {
		t.Fatalf("计费用户用量失败：%v", err)
	}

	svc := &DiskService{Deps: Deps{Store: st, Log: testLogger()}}

	// 2) 建盘彻底失败 → 这笔用量必须回退，磁盘进入 error（不再永远停在 creating）。
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

// TestCopyProgressReporter 覆盖"写入母盘"的进度映射（消费方见 buildVHDXFromSource）。
//
// 拷入是建盘里唯一的长活：源目录多大就要写多大的盘（30G 的库 = 30G 的读 + 30G 的写），
// 此前任务进度只上报 15 与 85 两个端点，中间几十分钟条纹丝不动 ——
// 用户看到的正是"100% 卡在建库中"（真实反馈：30G 等了半小时，以为是校验卡住了）。
func TestCopyProgressReporter(t *testing.T) {
	var got []int
	rep := copyProgressReporter(job.ReporterFunc(func(p int) { got = append(got, p) }), 15, 85)

	rep(0, 100)   // 起步：落在区间起点
	rep(50, 100)  // 一半：15 + 70*0.5
	rep(50, 100)  // 采样重复（卷上可用空间没再变）→ 不得重复上报
	rep(100, 100) // 拷完：落在区间终点
	rep(100, 100)
	if want := []int{15, 50, 85}; !slices.Equal(got, want) {
		t.Fatalf("进度上报序列 = %v，期望 %v", got, want)
	}

	// 总量未知（源目录为空、统计失败）时不报：编不出百分比就不要去动进度条。
	var unknownCount int
	unknown := copyProgressReporter(job.ReporterFunc(func(int) { unknownCount++ }), 15, 85)
	unknown(1024, 0)
	if unknownCount != 0 {
		t.Fatalf("总量未知时不应上报，实际上报 %d 次", unknownCount)
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

package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
	"vault/internal/platform"
)

// TestCleanupDiffsRequiresMaintenance 清理差异盘只能在维护态发起；正常态必须报状态机违规。
func TestCleanupDiffsRequiresMaintenance(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	// 补一个母盘 ID：CleanupDiffs 需要据此列出差异盘。
	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	parentID := "parent-1"
	repo.ParentDiskID = &parentID
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("更新存储库失败：%v", err)
	}

	worker := job.NewWorker(st, lock.NewKeyed(), job.Options{Logger: testLogger()})
	svc := &RepoService{Deps: Deps{Store: st, Log: testLogger(), Jobs: worker}}

	// 正常态：不允许清理（差异盘可能正在被用户使用）。
	if _, err := svc.CleanupDiffs(ctx, repoID, "op"); err == nil {
		t.Fatal("正常态不应允许清理差异盘")
	} else if got := apperr.CodeOf(err); got != "repo.parent_condition_violation" {
		t.Fatalf("错误码 = %q，期望 repo.parent_condition_violation（err=%v）", got, err)
	}

	// 维护态：允许清理，并返回一个异步任务。
	if err := st.SetParentCondition(ctx, repoID, domain.ParentMaintenance, false); err != nil {
		t.Fatalf("置维护态失败：%v", err)
	}
	j, err := svc.CleanupDiffs(ctx, repoID, "op")
	if err != nil {
		t.Fatalf("维护态应允许清理：%v", err)
	}
	if j.Type != domain.JobCleanupDiffs || j.State != domain.JobStatePending {
		t.Fatalf("任务 = %s/%s，期望 cleanup_diffs/pending", j.Type, j.State)
	}
}

// TestMaintenanceNoLongerProvisionsParentTarget 锁定新的母盘管理方式：
// 进入维护**不再**建 @temp 占位分配与临时 iSCSI 目标（改由服务端本地挂载 + 文件 API，
// 见 ParentMount），退出维护直接回到 idle，不依赖 iSCSI。
func TestMaintenanceNoLongerProvisionsParentTarget(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	parent := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindParent,
		VHDXPath:  filepath.Join("C:", "vault", "disks", "parents", "base.vhdx"),
		SizeBytes: 1 << 30,
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, parent); err != nil {
		t.Fatalf("建母盘失败：%v", err)
	}
	repo, err := st.GetRepository(ctx, repoID)
	if err != nil {
		t.Fatalf("回查存储库失败：%v", err)
	}
	repo.ParentDiskID = &parent.ID
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("更新存储库失败：%v", err)
	}

	worker := job.NewWorker(st, lock.NewKeyed(), job.Options{Logger: testLogger()})
	svc := &RepoService{Deps: Deps{Store: st, Log: testLogger(), Jobs: worker}}

	if _, err := svc.TransitionParent(ctx, repoID, ParentActionMaintenance, "op"); err != nil {
		t.Fatalf("进入维护失败：%v", err)
	}
	repo, _ = st.GetRepository(ctx, repoID)
	if repo.Condition() != domain.ParentMaintenance {
		t.Fatalf("进入维护后条件 = %s", repo.Condition())
	}
	if _, err := st.GetIscsiTargetByName(ctx, tempTargetName(repoID)); !isNotFound(err) {
		t.Fatalf("维护态不应再建母盘临时目标，err=%v", err)
	}
	allocs, err := st.ListAllocationsByRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("列出分配失败：%v", err)
	}
	for i := range allocs {
		if allocs[i].UserID == domain.TempAllocationUserID {
			t.Fatal("维护态不应再建 @temp 占位分配")
		}
	}

	if _, err := svc.TransitionParent(ctx, repoID, ParentActionFinishMaint, "op"); err != nil {
		t.Fatalf("退出维护失败：%v", err)
	}
	repo, _ = st.GetRepository(ctx, repoID)
	if repo.Condition() != domain.ParentIdle {
		t.Fatalf("退出维护后条件 = %s，期望 idle", repo.Condition())
	}
}

// resizeDiskSpy 记录一次母盘扩容调用（其余磁盘能力由嵌入接口兜底）。
type resizeDiskSpy struct {
	platform.DiskBackend
	size   int64
	called bool
}

func (f *resizeDiskSpy) Exists(string) bool                 { return true }
func (f *resizeDiskSpy) Fingerprint(string) (string, error) { return "fp", nil }
func (f *resizeDiskSpy) PhysicalSize(string) (int64, error) { return 0, nil }
func (f *resizeDiskSpy) Resize(_ context.Context, _ string, size int64) error {
	f.called = true
	f.size = size
	return nil
}

// TestResizeParentDiskOnlyInMaintenance 锁定母盘扩容的前提：仅维护态、只扩不缩、
// 且必须没有差异盘（差异盘基于旧容量）。
func TestResizeParentDiskOnlyInMaintenance(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	parent := &domain.Disk{
		RepoID:    repoID,
		Kind:      domain.DiskKindParent,
		VHDXPath:  filepath.Join("C:", "vault", "disks", "parents", "base.vhdx"),
		SizeBytes: 1 << 30,
		State:     domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, parent); err != nil {
		t.Fatalf("建母盘失败：%v", err)
	}
	repo, _ := st.GetRepository(ctx, repoID)
	repo.ParentDiskID = &parent.ID
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("更新存储库失败：%v", err)
	}

	spy := &resizeDiskSpy{}
	svc := &RepoService{Deps: Deps{Store: st, Log: testLogger(), Disk: spy}}

	// 非维护态：拒绝。
	if _, err := svc.ResizeParentDisk(ctx, repoID, 2<<30, "op"); err == nil {
		t.Fatal("非维护态不应允许扩容母盘")
	}

	if err := st.SetParentCondition(ctx, repoID, domain.ParentMaintenance, false); err != nil {
		t.Fatalf("置维护态失败：%v", err)
	}
	// 有差异盘：拒绝（差异盘基于旧容量）。
	diff := &domain.Disk{
		RepoID: repoID, Kind: domain.DiskKindDiff, VHDXPath: filepath.Join("C:", "vault", "diff.vhdx"),
		ParentID: &parent.ID, SizeBytes: parent.SizeBytes, State: domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, diff); err != nil {
		t.Fatalf("建差异盘失败：%v", err)
	}
	if _, err := svc.ResizeParentDisk(ctx, repoID, 2<<30, "op"); err == nil || apperr.CodeOf(err) != "repo.resize_requires_cleanup" {
		t.Fatalf("有差异盘时应拒绝扩容，实际 %v", err)
	}
	if err := st.DeleteDisk(ctx, diff.ID); err != nil {
		t.Fatalf("清理差异盘失败：%v", err)
	}

	// 缩小：拒绝。
	if _, err := svc.ResizeParentDisk(ctx, repoID, 512<<20, "op"); err == nil {
		t.Fatal("只扩不缩：更小的容量应被拒绝")
	}

	// 正常扩容：调用平台并把记录容量写回。
	if _, err := svc.ResizeParentDisk(ctx, repoID, 4<<30, "op"); err != nil {
		t.Fatalf("维护态扩容失败：%v", err)
	}
	if !spy.called || spy.size != 4<<30 {
		t.Fatalf("平台 Resize 未被正确调用：called=%v size=%d", spy.called, spy.size)
	}
	got, err := st.GetDisk(ctx, parent.ID)
	if err != nil {
		t.Fatalf("回查母盘失败：%v", err)
	}
	if got.SizeBytes != 4<<30 {
		t.Fatalf("母盘容量应更新为 %d，实际 %d", int64(4<<30), got.SizeBytes)
	}
}

// TestRunCleanupDiffsRevokesLeasesAndDeletesDisks 清理任务必须先撤销在线租约（通知客户端断开），
// 再删除差异盘记录与文件引用。没有撤销租约、或没有先拆 iSCSI 目标，文件会因占用删不掉。
func TestRunCleanupDiffsRevokesLeasesAndDeletesDisks(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	parentID := "parent-1"
	disk := &domain.Disk{
		RepoID:        repoID,
		Kind:          domain.DiskKindDiff,
		VHDXPath:      filepath.Join("C:", "vault", "disks", "diffs", "x.vhdx"),
		ParentID:      &parentID,
		SizeBytes:     1 << 30,
		State:         domain.DiskStateReady,
		ParentVersion: 1,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建差异盘失败：%v", err)
	}
	allocID := "alloc-" + disk.ID
	if err := st.CreateAllocation(ctx, &domain.Allocation{
		ID: allocID, RepoID: repoID, DiskID: disk.ID,
		UserID: userID, State: domain.AllocationStateMounted,
	}); err != nil {
		t.Fatalf("建分配失败：%v", err)
	}
	// 该差异盘必须有 iSCSI 目标：清理任务要连目标一起拆，否则文件会因映射/会话被占用删不掉。
	if err := st.CreateIscsiTarget(ctx, &domain.IscsiTarget{
		TargetName: "target-1", DiskID: &disk.ID, Purpose: domain.PurposeUser,
		AuthMode: domain.AuthModeNone,
	}); err != nil {
		t.Fatalf("建 iSCSI 目标失败：%v", err)
	}
	lease := &domain.Lease{
		AllocationID: allocID,
		TargetName:   "target-1",
		UserID:       userID,
		ClientID:     "client-1",
		State:        domain.LeaseStateActive,
		ExpiresAt:    1 << 62,
		LastSeenAt:   1,
	}
	if err := st.UpsertLease(ctx, lease); err != nil {
		t.Fatalf("建租约失败：%v", err)
	}

	leases := &LeaseService{Deps: Deps{Store: st, Log: testLogger()}}
	disks := &DiskService{Deps: Deps{Store: st, Log: testLogger()}, Leases: leases}

	payload, err := json.Marshal(cleanupDiffsPayload{RepoID: repoID, ParentDiskID: parentID})
	if err != nil {
		t.Fatalf("序列化 payload 失败：%v", err)
	}
	j := &domain.Job{ID: "job-1", Type: domain.JobCleanupDiffs, RefID: repoID, Payload: string(payload)}
	if err := disks.runCleanupDiffs(ctx, j, job.ReporterFunc(func(int) {})); err != nil {
		t.Fatalf("执行清理任务失败：%v", err)
	}

	// 租约被撤销（客户端会收到 revoke 指令并展示原因）。
	gotLease, err := st.GetLease(ctx, lease.ID)
	if err != nil {
		t.Fatalf("回查租约失败：%v", err)
	}
	if !gotLease.State.IsTerminal() {
		t.Fatalf("租约应被撤销为终态，实际 %s", gotLease.State)
	}
	// 差异盘与分配都被删除，母盘下不再有子盘。
	if remaining, err := st.ListDiffDisksByParent(ctx, parentID); err != nil {
		t.Fatalf("列出差异盘失败：%v", err)
	} else if len(remaining) != 0 {
		t.Fatalf("差异盘应全部删除，实际仍有 %d 个", len(remaining))
	}
	if _, err := st.GetAllocation(ctx, allocID); !isNotFound(err) {
		t.Fatalf("分配记录应已删除，err=%v", err)
	}
	// iSCSI 目标记录也要一起清掉：只删盘不拆目标是"删不掉文件 / 留下孤儿目标"的根因。
	if targets, err := st.ListIscsiTargetsByDisk(ctx, disk.ID); err != nil {
		t.Fatalf("列出 iSCSI 目标失败：%v", err)
	} else if len(targets) != 0 {
		t.Fatalf("差异盘的 iSCSI 目标应已删除，实际仍有 %d 个", len(targets))
	}
}

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

// TestFailCreatingDiskOnFailedJob 覆盖"建盘任务已失败 → 磁盘回滚为 error"这条对账兜底。
//
// 为什么重要：磁盘停在 creating 时挂载/发布会一直等 ready，用户每次点挂载都得到
// disk.not_ready 且永远不会变好。新失败由 worker 的 OnFinalFailure 即时回滚，
// 本对账负责历史数据（本次修复前就已失败的任务）与进程崩溃等场景。
func TestFailCreatingDiskOnFailedJob(t *testing.T) {
	const (
		jobPending = "pending"
		jobFailed  = "failed"
		jobNone    = ""
	)
	cases := []struct {
		name      string
		diskState domain.DiskState
		jobState  string
		want      domain.DiskState
	}{
		{name: "creating + 建盘任务已失败 → error", diskState: domain.DiskStateCreating, jobState: jobFailed, want: domain.DiskStateError},
		{name: "creating + 任务还在排队 → 保持 creating", diskState: domain.DiskStateCreating, jobState: jobPending, want: domain.DiskStateCreating},
		{name: "creating + 没有相关任务（上传建库流程）→ 保持 creating", diskState: domain.DiskStateCreating, jobState: jobNone, want: domain.DiskStateCreating},
		{name: "ready + 任务已失败 → 不动（只回滚中间态）", diskState: domain.DiskStateReady, jobState: jobFailed, want: domain.DiskStateReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			st := openAppTestStore(t)
			repoID := seedRepo(t, st)
			disk := &domain.Disk{
				RepoID:   repoID,
				Kind:     domain.DiskKindDiff,
				VHDXPath: filepath.Join("C:", "vault", "disk.vhdx"),
				State:    c.diskState,
			}
			if err := st.CreateDisk(ctx, disk); err != nil {
				t.Fatalf("建磁盘记录失败：%v", err)
			}
			if c.jobState != jobNone {
				createTestJob(t, st, disk.ID, domain.JobState(c.jobState))
			}

			application := &App{Deps: Deps{
				Store: st,
				Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			}}
			reported := 0
			application.failCreatingDiskOnFailedJob(ctx, disk,
				func(string, string, string, bool) { reported++ },
				application.Log)

			got, err := st.GetDisk(ctx, disk.ID)
			if err != nil {
				t.Fatalf("回查磁盘失败：%v", err)
			}
			if got.State != c.want {
				t.Fatalf("磁盘状态应为 %s，实际=%s", c.want, got.State)
			}
			wantReports := 0
			if c.want == domain.DiskStateError && c.diskState == domain.DiskStateCreating {
				wantReports = 1
			}
			if reported != wantReports {
				t.Fatalf("对账上报次数应为 %d，实际=%d", wantReports, reported)
			}
		})
	}
}

// createTestJob 插入一条 ref_id=diskID 的任务（建盘任务的 ref_id 就是磁盘 ID）。
func createTestJob(t *testing.T, st *store.Store, diskID string, state domain.JobState) {
	t.Helper()
	ctx := context.Background()
	job := &domain.Job{
		Type:    domain.JobCreateDiff,
		RefID:   diskID,
		IdemKey: "create_diff:" + diskID,
		Payload: "{}",
		State:   state,
	}
	if _, _, err := st.CreateJob(ctx, job); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
}

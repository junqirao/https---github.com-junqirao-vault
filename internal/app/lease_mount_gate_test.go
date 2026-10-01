package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// TestRequestMountRejectsRepoNotActive 锁定挂载准入的**库状态闸门**。
//
// domain.RepoStateCreating 的注释一直写着"该状态下拒绝分配与挂载"，但挂载侧长期没落地：
// 用户在建库中的卡片/详情页点挂载，服务端会拿着还没派生好的池位盘去发布，报错与"建库中"
// 完全对不上（真实反馈："逻辑有误啊，creating 中的存储库不允许挂载啊"）。
// 分配侧（repo.Allocate）早就拦了，这条测试钉住挂载侧与它一致。
func TestRequestMountRejectsRepoNotActive(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, userID := seedRepoWithOwner(t, st)

	disk := &domain.Disk{
		RepoID:   repoID,
		Kind:     domain.DiskKindDiff,
		VHDXPath: filepath.Join(t.TempDir(), "diff.vhdx"),
		State:    domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}
	alloc := &domain.Allocation{
		RepoID: repoID,
		DiskID: disk.ID,
		UserID: userID,
		State:  domain.AllocationStateAllocated,
	}
	if err := st.CreateAllocation(ctx, alloc); err != nil {
		t.Fatalf("建分配记录失败：%v", err)
	}

	svc := &LeaseService{Deps: Deps{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	cases := []struct {
		state domain.RepoState
		want  string
	}{
		// 建库中：必须单独报 repo.creating，前端据此把"挂载"置灰并说明"库还在创建中"。
		{state: domain.RepoStateCreating, want: "repo.creating"},
		// 其它非 active 状态：笼统的"状态不对"即可（用户看到的是"该库当前不可挂载"）。
		{state: domain.RepoStateDeleting, want: "repo.state_invalid"},
		{state: domain.RepoStateError, want: "repo.state_invalid"},
		{state: domain.RepoStateSealing, want: "repo.state_invalid"},
	}
	for _, c := range cases {
		t.Run(string(c.state), func(t *testing.T) {
			repo, err := st.GetRepository(ctx, repoID)
			if err != nil {
				t.Fatalf("查存储库失败：%v", err)
			}
			repo.State = c.state
			if err := st.UpdateRepository(ctx, repo); err != nil {
				t.Fatalf("改存储库状态失败：%v", err)
			}
			_, err = svc.RequestMount(ctx, alloc.ID, "client-1", userID)
			if got := apperr.CodeOf(err); got != c.want {
				t.Fatalf("库状态 %s 时申请挂载的错误码应为 %s，实际=%s（err=%v）",
					c.state, c.want, got, err)
			}
		})
	}
}

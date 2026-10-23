package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/store"
)

// newUploadNameTestService 造一个带真库的 UploadService（含 Disks：清理残骸要用）。
func newUploadNameTestService(t *testing.T) (*UploadService, *store.Store) {
	t.Helper()
	st := openAppTestStore(t)
	deps := Deps{Store: st, Log: testLogger()}
	return &UploadService{Deps: deps, Disks: &DiskService{Deps: deps}}, st
}

// seedUploadUser 建一个用户。
func seedUploadUser(t *testing.T, st *store.Store, username string) string {
	t.Helper()
	u := &domain.User{Username: username, Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("建用户失败：%v", err)
	}
	return u.ID
}

// seedUploadSession 预置一条上传会话（含真实的暂存目录），返回 (upload_id, staging_dir)。
func seedUploadSession(t *testing.T, st *store.Store, userID, repoName string, totalFiles int, totalBytes int64, state string) (string, string) {
	t.Helper()
	ctx := context.Background()
	manifest, err := marshalManifest(uploadManifestRecord{
		Root:  repoName,
		Files: []domain.ManifestEntry{{Path: "a.bin", Size: totalBytes}},
	})
	if err != nil {
		t.Fatalf("构造清单失败：%v", err)
	}
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("建暂存目录失败：%v", err)
	}
	u := &domain.Upload{
		UserID: userID, RepoName: repoName, Mode: domain.UploadModeCopy,
		TotalFiles: totalFiles, TotalBytes: totalBytes,
		StagingDir: staging, Manifest: manifest, State: state,
	}
	if err := st.CreateUpload(ctx, u); err != nil {
		t.Fatalf("建上传会话失败：%v", err)
	}
	return u.ID, staging
}

// seedUploadRepo 预置一个同名存储库（模拟上次上传留下的记录）。
func seedUploadRepo(t *testing.T, st *store.Store, ownerID, name string, state domain.RepoState) string {
	t.Helper()
	repo := &domain.Repository{Name: name, Mode: domain.RepoModeShared, OwnerID: ownerID, State: state}
	if err := st.CreateRepository(context.Background(), repo); err != nil {
		t.Fatalf("建存储库失败：%v", err)
	}
	return repo.ID
}

// TestUploadReusable 锁定"旧会话能不能接着用"的判据：只有状态未终态 + 内容一致
// （文件数与总字节）才复用 —— 分块偏移是按清单顺序算的，内容不同就是对不上号。
func TestUploadReusable(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")

	cases := []struct {
		name       string
		state      string
		bytes      int64
		storageID  string
		wantReuse  bool
		checkState string
	}{
		{name: "open + 内容一致 → 复用", state: domain.UploadStateOpen, bytes: 10, wantReuse: true},
		{name: "verifying（建库任务可能还在跑）→ 复用", state: domain.UploadStateVerifying, bytes: 10, wantReuse: true},
		{name: "complete → 不复用", state: domain.UploadStateComplete, bytes: 10},
		{name: "aborted → 不复用", state: domain.UploadStateAborted, bytes: 10},
		{name: "failed → 不复用", state: domain.UploadStateFailed, bytes: 10},
		{name: "内容不一致 → 不复用", state: domain.UploadStateOpen, bytes: 99},
		{name: "会话指定了另一个存储 → 不复用", state: domain.UploadStateOpen, bytes: 10, storageID: "st-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, _ := seedUploadSession(t, st, user, "repo-"+c.name, 1, 10, c.state)
			if c.storageID != "" {
				// 会话自己记着 storage_id（st-1），本次上传要求落到 st-2：不复用。
				raw, err := st.GetUpload(ctx, id)
				if err != nil {
					t.Fatalf("回查会话失败：%v", err)
				}
				rec, err := parseManifest(raw.Manifest)
				if err != nil {
					t.Fatalf("解析清单失败：%v", err)
				}
				rec.StorageID = "st-1"
				manifest, err := marshalManifest(*rec)
				if err != nil {
					t.Fatalf("写回清单失败：%v", err)
				}
				if err := st.UpdateUploadManifest(ctx, id, manifest); err != nil {
					t.Fatalf("更新清单失败：%v", err)
				}
				raw.Manifest = manifest
			}
			raw, err := st.GetUpload(ctx, id)
			if err != nil {
				t.Fatalf("回查会话失败：%v", err)
			}
			got := svc.uploadReusable(raw, 1, c.bytes, c.storageID)
			if got != c.wantReuse {
				t.Fatalf("uploadReusable = %v，期望 %v", got, c.wantReuse)
			}
		})
	}
}

// TestPrepareUploadNameReusesMatchingSession 是"上次失败、这次同名重传"的主路径：
// 服务端把内容一致的旧会话原样还回去，已传的分块一个都不用重传。
func TestPrepareUploadNameReusesMatchingSession(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")
	uploadID, staging := seedUploadSession(t, st, user, "No Mans Sky", 3, 4096, domain.UploadStateOpen)

	plan, err := svc.prepareUploadName(ctx, user, "No Mans Sky", 3, 4096, "")
	if err != nil {
		t.Fatalf("prepareUploadName 失败：%v", err)
	}
	if plan.reusable == nil || plan.reusable.ID != uploadID {
		t.Fatalf("应复用会话 %s，实际 %+v", uploadID, plan.reusable)
	}
	if plan.overwrite {
		t.Fatal("仅复用会话时不应标记覆盖（库还没建出来）")
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("复用会话时暂存目录必须保留：%v", err)
	}
	if _, err := st.GetUpload(ctx, uploadID); err != nil {
		t.Fatalf("复用会话时必须保留会话行：%v", err)
	}
}

// TestPrepareUploadNameDiscardsStaleSession 覆盖"同名但内容已经变了"：
// 旧会话（偏移对不上）必须连同暂存目录一起作废，否则既白占盘、又会让后续 Status 认错人。
func TestPrepareUploadNameDiscardsStaleSession(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")
	uploadID, staging := seedUploadSession(t, st, user, "No Mans Sky", 3, 4096, domain.UploadStateOpen)
	// 会话是"上次失败留下"的：早就没人动过（小于 0 的窗口 = 一律按久未活动处理）。
	svc.supersedeIdleMillis = -1

	// 本地内容变了：文件数与总字节都对不上。
	plan, err := svc.prepareUploadName(ctx, user, "No Mans Sky", 4, 8192, "")
	if err != nil {
		t.Fatalf("prepareUploadName 失败：%v", err)
	}
	if plan.reusable != nil {
		t.Fatalf("内容不一致不应复用，实际 %+v", plan.reusable)
	}
	if _, err := st.GetUpload(ctx, uploadID); !isNotFound(err) {
		t.Fatalf("旧会话应被删除，实际 err=%v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("旧暂存目录应被删除，实际 err=%v", err)
	}
	if len(plan.cleaned) == 0 {
		t.Fatal("应记录清理明细（供日志/审计）")
	}
}

// TestPrepareUploadNameKeepsRecentlyActiveSession 守住"别打断另一台机器"：
// 5 分钟内还被喂过分块的同名会话不许删暂存，但**不能因此挡住本次上传**。
func TestPrepareUploadNameKeepsRecentlyActiveSession(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")
	uploadID, staging := seedUploadSession(t, st, user, "No Mans Sky", 3, 4096, domain.UploadStateOpen)
	// 模拟"刚刚还在传"：最近收到过一个分块。
	if err := st.AddUploadReceived(ctx, uploadID, 1024); err != nil {
		t.Fatalf("累加已接收字节失败：%v", err)
	}

	// 内容已变 → 不复用，但也不该删掉那条刚活跃过的会话。
	plan, err := svc.prepareUploadName(ctx, user, "No Mans Sky", 4, 8192, "")
	if err != nil {
		t.Fatalf("刚活跃过的同名会话不应挡住上传：%v", err)
	}
	if plan.reusable != nil {
		t.Fatalf("内容不一致不应复用，实际 %+v", plan.reusable)
	}
	if _, err := st.GetUpload(ctx, uploadID); err != nil {
		t.Fatalf("刚活跃过的会话不该被删：%v", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("刚活跃过的暂存目录不该被删：%v", err)
	}
}

// TestPrepareUploadNameRefusesWhileBuilding 建库任务正在写盘时，不能另起同名上传
// （既不能续传其它内容，也不能清库把任务打成残骸）。
func TestPrepareUploadNameRefusesWhileBuilding(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")
	uploadID, _ := seedUploadSession(t, st, user, "No Mans Sky", 3, 4096, domain.UploadStateOpen)
	if _, _, err := st.CreateJob(ctx, &domain.Job{
		Type: domain.JobUploadComplete, RefID: uploadID,
		IdemKey: uploadCompleteIdemKey(uploadID), State: domain.JobStateRunning,
	}); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}

	_, err := svc.prepareUploadName(ctx, user, "No Mans Sky", 4, 8192, "")
	if err == nil {
		t.Fatal("建库任务在跑时必须拒绝另起同名上传")
	}
	if got := apperr.CodeOf(err); got != "repo.upload_conflict" {
		t.Fatalf("错误码 = %s，期望 repo.upload_conflict", got)
	}
	if _, err := st.GetUpload(ctx, uploadID); err != nil {
		t.Fatalf("正在建库的会话不能被删：%v", err)
	}
}

// TestPrepareUploadNamePurgesRemnantRepo 是用户报的"点击上传直接失败"的直接回归：
// 上次上传失败会留下一个 importing/error 的库，只要名字还在，旧实现就永远报
// repo.name_taken。现在必须在开始上传时把它清掉，让建库从零重建。
func TestPrepareUploadNamePurgesRemnantRepo(t *testing.T) {
	for _, state := range []domain.RepoState{domain.RepoStateImporting, domain.RepoStateError} {
		t.Run(string(state), func(t *testing.T) {
			svc, st := newUploadNameTestService(t)
			ctx := context.Background()
			user := seedUploadUser(t, st, "tester")
			repoID := seedUploadRepo(t, st, user, "No Mans Sky", state)

			plan, err := svc.prepareUploadName(ctx, user, "No Mans Sky", 3, 4096, "")
			if err != nil {
				t.Fatalf("prepareUploadName 失败：%v", err)
			}
			if !plan.overwrite {
				t.Fatal("残骸被清掉后本次上传就是覆盖重建，应标记 overwrite")
			}
			if _, err := st.GetRepositoryByName(ctx, "No Mans Sky"); !isNotFound(err) {
				t.Fatalf("残骸库应被清理，实际 err=%v", err)
			}
			if _, err := st.GetRepository(ctx, repoID); !isNotFound(err) {
				t.Fatalf("残骸库记录应被删除，实际 err=%v", err)
			}
			if len(plan.cleaned) == 0 {
				t.Fatal("应记录清理明细（供日志/审计）")
			}
		})
	}
}

// TestPrepareUploadNameAllowsOverwriteForIdleRepo 覆盖"上次库建成了、这次重传同一个名字"：
// 本人 + 没人用（无租约/未挂载/没分配过盘）→ 放行，由建库任务就地重建母盘，库记录不删。
func TestPrepareUploadNameAllowsOverwriteForIdleRepo(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	user := seedUploadUser(t, st, "tester")
	repoID := seedUploadRepo(t, st, user, "No Mans Sky", domain.RepoStateActive)

	plan, err := svc.prepareUploadName(ctx, user, "No Mans Sky", 3, 4096, "")
	if err != nil {
		t.Fatalf("prepareUploadName 失败：%v", err)
	}
	if !plan.overwrite {
		t.Fatal("空壳库应允许被本次上传覆盖重建")
	}
	if _, err := st.GetRepository(ctx, repoID); err != nil {
		t.Fatalf("已建成的库不应被删除（由建库任务就地重建母盘）：%v", err)
	}
}

// TestPrepareUploadNameRefusesForeignRepo 不能接管别人的库：这是唯一必须拒绝的"名字冲突"。
func TestPrepareUploadNameRefusesForeignRepo(t *testing.T) {
	svc, st := newUploadNameTestService(t)
	ctx := context.Background()
	owner := seedUploadUser(t, st, "owner")
	other := seedUploadUser(t, st, "other")
	seedUploadRepo(t, st, owner, "No Mans Sky", domain.RepoStateActive)

	_, err := svc.prepareUploadName(ctx, other, "No Mans Sky", 3, 4096, "")
	if err == nil {
		t.Fatal("别人的库必须拒绝接管")
	}
	if got := apperr.CodeOf(err); got != "repo.upload_conflict" {
		t.Fatalf("错误码 = %s，期望 repo.upload_conflict", got)
	}
	if _, err := st.GetRepositoryByName(ctx, "No Mans Sky"); err != nil {
		t.Fatalf("拒绝时不得动那个库：%v", err)
	}
}

// TestPurgeUploadRemnant 覆盖"走本地续传记录重试"那条路径：它不经过 CreateUpload，
// 残骸必须在建库前自愈，否则用户会一直撞在同一个 error 库上（"重试秒失败"）。
func TestPurgeUploadRemnant(t *testing.T) {
	cases := []struct {
		name      string
		state     domain.RepoState
		otherOwn  bool
		wantPurge bool
	}{
		{name: "error 残骸 → 清掉", state: domain.RepoStateError, wantPurge: true},
		{name: "active 正常库 → 不动", state: domain.RepoStateActive},
		{name: "importing（可能还在建）→ 不动", state: domain.RepoStateImporting},
		{name: "别人的 error 库 → 不动", state: domain.RepoStateError, otherOwn: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, st := newUploadNameTestService(t)
			ctx := context.Background()
			user := seedUploadUser(t, st, "tester")
			owner := user
			if c.otherOwn {
				owner = seedUploadUser(t, st, "owner")
			}
			seedUploadRepo(t, st, owner, "No Mans Sky", c.state)

			if err := svc.purgeUploadRemnant(ctx, "No Mans Sky", user); err != nil {
				t.Fatalf("purgeUploadRemnant 失败：%v", err)
			}
			_, err := st.GetRepositoryByName(ctx, "No Mans Sky")
			cleaned := isNotFound(err)
			if cleaned != c.wantPurge {
				t.Fatalf("已清理 = %v，期望 %v（err=%v）", cleaned, c.wantPurge, err)
			}
		})
	}
}

// TestManifestWithOptionsKeepsFileOrder 守住复用会话时最容易出事的那个不变量：
// 只改建库选项，**文件列表与根必须一个字都不动** —— 分块偏移是按文件顺序算的。
func TestManifestWithOptionsKeepsFileOrder(t *testing.T) {
	rec := &uploadManifestRecord{
		Root: "No Mans Sky",
		Files: []domain.ManifestEntry{
			{Path: "b.bin", Size: 5},
			{Path: "a.bin", Size: 7},
			{Path: "c.bin", Size: 3},
		},
		RepoMode: "shared", StorageID: "", QuotaBytes: 0, ClientOS: "windows",
	}
	got, err := manifestWithOptions(rec, "exclusive", "st-9", 1<<30, 3, true, domain.ClientOSLinux)
	if err != nil {
		t.Fatalf("manifestWithOptions 失败：%v", err)
	}
	next, err := parseManifest(got)
	if err != nil {
		t.Fatalf("解析结果失败：%v", err)
	}
	if next.RepoMode != "exclusive" || next.StorageID != "st-9" || next.QuotaBytes != 1<<30 ||
		next.MaxDiffDisks != 3 || !next.Preallocate || next.ClientOS != "linux" {
		t.Fatalf("建库选项未按本次请求刷新：%+v", next)
	}
	if next.Root != rec.Root || len(next.Files) != len(rec.Files) {
		t.Fatalf("根与文件列表必须原样保留：%+v", next)
	}
	for i := range rec.Files {
		if next.Files[i] != rec.Files[i] {
			t.Fatalf("第 %d 个文件被改动了：%+v != %+v", i, next.Files[i], rec.Files[i])
		}
	}
}

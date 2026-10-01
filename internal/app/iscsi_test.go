package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform"
	"vault/internal/store"
)

// fakeDiskBackend 只实现本测试用到的 Exists；其余方法由嵌入的接口兜底。
//
// 嵌入接口（而非实现全部方法）是刻意的：本测试只关心"底层盘文件在不在"，
// 多写十几个空实现只会让测试更难读。
type fakeDiskBackend struct {
	platform.DiskBackend
	exists bool
}

func (f fakeDiskBackend) Exists(string) bool { return f.exists }

// openAppTestStore 在临时目录打开一个 SQLite 库并建表（与 store 包测试同样的做法）。
func openAppTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, store.Options{
		Dialect:    store.DialectSQLite,
		SQLitePath: filepath.Join(t.TempDir(), "vault.db"),
	})
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	// Open 只建连接，建表必须显式迁移（否则 disks 表不存在）。
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("迁移测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newBackingTestService(t *testing.T, diskExists bool) (*IscsiService, *store.Store) {
	t.Helper()
	st := openAppTestStore(t)
	svc := &IscsiService{Deps: Deps{
		Store: st,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Disk:  fakeDiskBackend{exists: diskExists},
	}}
	return svc, st
}

// seedRepo 建一个用户 + 存储库，返回存储库 ID。
//
// disks.repo_id 有外键约束（迁移里启用），因此建磁盘记录前必须先有存储库。
func seedRepo(t *testing.T, st *store.Store) string {
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
	return repo.ID
}

// TestAwaitBackingReady 覆盖挂载/发布前的"等底层盘就绪"判定。
//
// 关键回归（真实工单现象）：卸载（Lease.Release）后磁盘**保持 published**
// （目标继续发布，便于客户端再次挂载）。published 蕴含"盘已建好且发布成功"，
// 是 ready 的超集，必须同样判为就绪——只认 ready 会让"卸载后再挂载"每次白等
// 15 秒，然后报 disk.not_ready，且 100% 复现。
func TestAwaitBackingReady(t *testing.T) {
	cases := []struct {
		name     string
		state    domain.DiskState
		exists   bool
		wantCode string // 空串表示应当成功
	}{
		{name: "ready 且文件存在", state: domain.DiskStateReady, exists: true},
		{name: "published 且文件存在（卸载后再次挂载）", state: domain.DiskStatePublished, exists: true},
		{name: "ready 但文件缺失", state: domain.DiskStateReady, exists: false, wantCode: "disk.not_found"},
		{name: "published 但文件缺失", state: domain.DiskStatePublished, exists: false, wantCode: "disk.not_found"},
		{name: "建盘已失败（error）", state: domain.DiskStateError, exists: false, wantCode: "disk.create_failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			svc, st := newBackingTestService(t, c.exists)
			repoID := seedRepo(t, st)
			disk := &domain.Disk{
				RepoID:    repoID,
				Kind:      domain.DiskKindDiff,
				VHDXPath:  filepath.Join("C:", "vault", "disk.vhdx"),
				SizeBytes: 1 << 20,
				State:     c.state,
			}
			if err := st.CreateDisk(ctx, disk); err != nil {
				t.Fatalf("建磁盘记录失败：%v", err)
			}

			err := svc.awaitBackingReady(ctx, disk)
			if c.wantCode == "" {
				if err != nil {
					t.Fatalf("应判定为就绪，实际错误=%v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应返回错误，实际通过")
			}
			e, ok := apperr.As(err)
			if !ok {
				t.Fatalf("应为业务错误，实际=%v", err)
			}
			if e.Code != c.wantCode {
				t.Fatalf("错误码应为 %s，实际=%s（%v）", c.wantCode, e.Code, err)
			}
		})
	}
}

// TestAwaitBackingReadyPollsCreating 建盘中间态必须**等待**而不是立刻失败：
// 差异盘由分配异步派生，紧接着的挂载请求会抢跑，轮询到 ready 后应正常返回。
func TestAwaitBackingReadyPollsCreating(t *testing.T) {
	ctx := context.Background()
	svc, st := newBackingTestService(t, true)
	disk := &domain.Disk{
		RepoID:   seedRepo(t, st),
		Kind:     domain.DiskKindDiff,
		VHDXPath: filepath.Join("C:", "vault", "disk.vhdx"),
		State:    domain.DiskStateCreating,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	// 模拟异步建盘任务：稍后把状态推进到 ready。
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(150 * time.Millisecond)
		current, err := st.GetDisk(context.Background(), disk.ID)
		if err != nil {
			return
		}
		current.State = domain.DiskStateReady
		_ = st.UpdateDisk(context.Background(), current)
	}()

	if err := svc.awaitBackingReady(ctx, disk); err != nil {
		t.Fatalf("轮询到 ready 后应成功，实际错误=%v", err)
	}
	<-done
}

package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"vault/internal/domain"
	"vault/internal/platform"
)

// fakeVolumeBackend 用临时目录模拟"母盘卷已挂载到服务端本地"。
type fakeVolumeBackend struct {
	platform.VolumeBackend
	root string
}

func (f *fakeVolumeBackend) MountVolume(context.Context, string, string) (string, error) {
	return f.root, nil
}

func (f *fakeVolumeBackend) UnmountVolume(context.Context, string, string) error { return nil }

// TestParentFilesCRUDAndTraversal 锁定维护态母盘文件管理的基本行为与路径安全：
// 上传/读取/列表/新建目录/重命名/删除，以及**任何 `..` 越界路径都必须被拒绝**。
func TestParentFilesCRUDAndTraversal(t *testing.T) {
	ctx := context.Background()
	st := openAppTestStore(t)
	repoID, _ := seedRepoWithOwner(t, st)

	parent := &domain.Disk{
		RepoID:     repoID,
		Kind:       domain.DiskKindParent,
		VHDXPath:   filepath.Join("C:", "vault", "base.vhdx"),
		SizeBytes:  1 << 30,
		State:      domain.DiskStateReady,
		FileSystem: domain.FileSystem("ntfs"),
	}
	if err := st.CreateDisk(ctx, parent); err != nil {
		t.Fatalf("建母盘失败：%v", err)
	}
	repo, _ := st.GetRepository(ctx, repoID)
	repo.ParentDiskID = &parent.ID
	if err := st.UpdateRepository(ctx, repo); err != nil {
		t.Fatalf("更新存储库失败：%v", err)
	}
	if err := st.SetParentCondition(ctx, repoID, domain.ParentMaintenance, false); err != nil {
		t.Fatalf("置维护态失败：%v", err)
	}

	root := t.TempDir()
	svc := &RepoService{Deps: Deps{Store: st, Log: testLogger(), Vol: &fakeVolumeBackend{root: root}}}

	// 上传（自动建父目录）。
	if _, err := svc.SaveParentFile(ctx, repoID, "/boot/grub/grub.cfg", strings.NewReader("hello")); err != nil {
		t.Fatalf("写入文件失败：%v", err)
	}
	listing, err := svc.ListParentFiles(ctx, repoID, "/boot/grub")
	if err != nil {
		t.Fatalf("列目录失败：%v", err)
	}
	if len(listing.Items) != 1 || listing.Items[0].Name != "grub.cfg" || listing.Items[0].IsDir {
		t.Fatalf("列目录结果不对：%+v", listing.Items)
	}

	if got, err := svc.ReadParentText(ctx, repoID, "/boot/grub/grub.cfg"); err != nil || got != "hello" {
		t.Fatalf("读取文本 = %q, err=%v", got, err)
	}
	if err := svc.WriteParentText(ctx, repoID, "/boot/grub/grub.cfg", []byte("world")); err != nil {
		t.Fatalf("保存文本失败：%v", err)
	}
	if got, _ := svc.ReadParentText(ctx, repoID, "/boot/grub/grub.cfg"); got != "world" {
		t.Fatalf("保存后内容 = %q", got)
	}

	if err := svc.MkdirParent(ctx, repoID, "/empty"); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if _, err := svc.RenameParentEntry(ctx, repoID, "/empty", "/empty2"); err != nil {
		t.Fatalf("重命名失败：%v", err)
	}
	if err := svc.DeleteParentEntry(ctx, repoID, "/empty2"); err != nil {
		t.Fatalf("删除空目录失败：%v", err)
	}

	// 非空目录不允许直接删（避免误删整棵备份目录）。
	if err := svc.DeleteParentEntry(ctx, repoID, "/boot"); err == nil {
		t.Fatal("非空目录不应被删除")
	}

	// 路径穿越必须被拒绝。
	for _, bad := range []string{"../outside", "/../../etc/passwd", `..\..\windows`} {
		if _, _, err := resolveParentPath(root, bad); err == nil {
			t.Fatalf("越界路径 %q 应被拒绝", bad)
		}
	}
}

package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"vault/internal/apperr"
	"vault/internal/config"
	"vault/internal/domain"
	"vault/internal/platform"
	"vault/internal/store"
)

// orphanTestDisk 只补 Kind()：孤儿扫描的前提是 Windows 的 VHDX 目录布局（见孤立扫描的平台判定），
// 本测试用不到 DiskBackend 的其它方法，让它们保持 nil 即可（调用会 panic，正是我们想要的信号）。
type orphanTestDisk struct{ platform.DiskBackend }

func (orphanTestDisk) Kind() platform.Kind { return platform.KindWindows }

// newOrphanTestApp 构造带真实配置（storage.disks_dir / orphan_dir）、Store 与假磁盘后端的 App，
// 并登记一个启用存储根 root。
//
// 配置必须真的走一遍 config 加载：磁盘目录名是"根 + disks_dir"拼出来的，缺了它
// Resolve("") 会退化成根本身 —— 那正是删除白名单最危险的形态（见 disksDirOf）。
func newOrphanTestApp(t *testing.T, root string) (*App, *store.Store) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	// 单引号标量：Windows 路径里的反斜杠在 YAML 双引号里是转义字符，必须原样保留。
	cfgYAML := "database:\n  driver: sqlite\nstorage:\n  whitelist_roots:\n    - '" + root + "'\n" +
		"  disks_dir: disks\n  orphan_dir: meta/orphan\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("写测试配置失败：%v", err)
	}
	watcher, err := config.NewWatcher(cfgPath, nil)
	if err != nil {
		t.Fatalf("加载测试配置失败：%v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })

	st := openAppTestStore(t)
	app := &App{Deps: Deps{
		Store: st,
		Cfg:   watcher,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Disk:  orphanTestDisk{},
	}}
	if err := st.CreateStorage(context.Background(), &domain.Storage{
		Name:    "orphan-test",
		Path:    root,
		Enabled: true,
	}); err != nil {
		t.Fatalf("登记存储根失败：%v", err)
	}
	return app, st
}

// writeFile 建出目录与文件，返回绝对路径。
func writeOrphanFile(t *testing.T, dir, name string, size int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	return p
}

// TestScanOrphanFilesListsOnlyUnregisteredVHDX 锁定「孤儿磁盘」页面的清单口径：
// 只列 disks/ 下**未登记**的 .vhdx，登记盘/非 vhdx/根外文件都不算，并带上分类与所属根。
//
// 真实反馈：对账发现孤儿盘时只能刷一屏 "发现孤儿差异盘文件（仅报告）"，用户看到日志却
// 无处处理；清单口径错了（漏报或误报成能删的孤儿）比没有页面更危险。
func TestScanOrphanFilesListsOnlyUnregisteredVHDX(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	app, st := newOrphanTestApp(t, root)

	// 未登记的差异盘（就是用户日志里那份）。
	orphanDiff := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "2748afef"), "d0369b41.vhdx", 4096)
	// 未登记的其它位置文件。
	orphanOther := writeOrphanFile(t, filepath.Join(root, "disks", "misc"), "stray.vhdx", 512)
	// 已登记的差异盘：故意用大写扩展名登记，验证比对是大小写不敏感归一。
	registered := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "2748afef"), "in-use.vhdx", 1024)
	// 非 vhdx 文件：不参与统计。
	writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "2748afef"), "notes.txt", 8)
	// disks/ 之外的文件：不属于孤儿扫描范围（对账会把它们移进 meta/orphan）。
	outside := writeOrphanFile(t, filepath.Join(root, "meta", "orphan"), "moved.vhdx", 64)

	repoID := seedRepo(t, st)
	disk := &domain.Disk{
		RepoID:   repoID,
		Kind:     domain.DiskKindDiff,
		VHDXPath: registered[:len(registered)-len(".vhdx")] + ".VHDX",
		State:    domain.DiskStateReady,
	}
	if err := st.CreateDisk(ctx, disk); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	scan, err := app.ScanOrphanFiles(ctx)
	if err != nil {
		t.Fatalf("扫描孤儿文件失败：%v", err)
	}
	if scan.Checked != 3 {
		t.Fatalf("扫过的 .vhdx 数应为 3（两份孤儿 + 一份登记盘），实际=%d", scan.Checked)
	}
	if len(scan.Files) != 2 {
		t.Fatalf("孤儿清单应为 2 份，实际=%d：%+v", len(scan.Files), scan.Files)
	}
	byPath := map[string]OrphanFile{}
	for _, f := range scan.Files {
		byPath[f.Path] = f
	}
	if f, ok := byPath[orphanDiff]; !ok || f.Kind != OrphanKindDiff {
		t.Fatalf("未把未登记的差异盘列为 diff：%+v", byPath[orphanDiff])
	}
	if f, ok := byPath[orphanOther]; !ok || f.Kind != OrphanKindOther {
		t.Fatalf("disks/ 下的其它 .vhdx 应列为 other：%+v", byPath[orphanOther])
	}
	if _, ok := byPath[registered]; ok {
		t.Fatal("已登记的盘不得出现在孤儿清单里（删它就是删真实数据）")
	}
	if _, ok := byPath[outside]; ok {
		t.Fatal("disks/ 之外的文件不属于孤儿扫描范围")
	}
	if scan.TotalBytes != 4096+512 {
		t.Fatalf("大小合计应为 4608，实际=%d", scan.TotalBytes)
	}
	if len(scan.Roots) != 1 || scan.Roots[0] != root {
		t.Fatalf("应报告覆盖的存储根，实际=%v", scan.Roots)
	}
}

// TestDeleteOrphanFileGuards 锁定删除的四道闸门 + 正常删除路径。
//
// 这是本系统里唯一"按用户给的路径直接删文件"的接口：任何一个闸门失守都等于给了
// 一个任意文件删除原语，因此这里逐个钉住（含"清单过期后文件已被登记"的竞态）。
func TestDeleteOrphanFileGuards(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	app, st := newOrphanTestApp(t, root)

	orphan := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "a"), "orphan.vhdx", 2048)
	registered := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "a"), "in-use.vhdx", 1024)
	outside := writeOrphanFile(t, filepath.Join(root, "meta", "orphan"), "moved.vhdx", 64)
	notVHDX := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "a"), "notes.txt", 8)

	repoID := seedRepo(t, st)
	if err := st.CreateDisk(ctx, &domain.Disk{
		RepoID:   repoID,
		Kind:     domain.DiskKindDiff,
		VHDXPath: registered,
		State:    domain.DiskStateReady,
	}); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "空路径", path: "  ", want: apperr.CodeInvalidParam},
		{name: "相对路径", path: filepath.Join("disks", "diffs", "a", "orphan.vhdx"), want: "system.orphan_path_invalid"},
		{name: "不是 vhdx", path: notVHDX, want: "system.orphan_path_invalid"},
		{name: "不在任何存储根下", path: filepath.Join(t.TempDir(), "x.vhdx"), want: "system.orphan_path_invalid"},
		{name: "根内但不在 disks 目录下", path: outside, want: "system.orphan_path_invalid"},
		{name: "已被数据库登记", path: registered, want: "system.orphan_registered"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := app.DeleteOrphanFile(ctx, c.path)
			if got := apperr.CodeOf(err); got != c.want {
				t.Fatalf("错误码应为 %s，实际=%s（err=%v）", c.want, got, err)
			}
		})
	}
	// 被拒绝的路径必须原封不动还在（尤其登记盘）。
	if _, err := os.Stat(registered); err != nil {
		t.Fatalf("登记盘不得被删除：%v", err)
	}

	res, err := app.DeleteOrphanFile(ctx, orphan)
	if err != nil {
		t.Fatalf("删除孤儿文件失败：%v", err)
	}
	if res.FreedBytes != 2048 || res.Path != orphan {
		t.Fatalf("删除结果不对：%+v", res)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("文件应已删除，err=%v", err)
	}

	// 第二次点同一行（页面没刷新）：报"已不存在"，而不是静默成功。
	if _, err := app.DeleteOrphanFile(ctx, orphan); apperr.CodeOf(err) != "system.orphan_not_found" {
		t.Fatalf("重复删除应报 system.orphan_not_found，实际=%v", err)
	}
}

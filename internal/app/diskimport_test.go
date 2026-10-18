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

// importTestDisk 只实现导入流程真正会调到的方法：
//   - Kind：平台判定（非 Windows 直接不扫）；
//   - PhysicalSize / Fingerprint：导入时"尽力补充"的只读信息。
//
// 其余方法由嵌入的接口兜底 —— 被调到就会 panic，正是"不该被调用"的信号。
type importTestDisk struct{ platform.DiskBackend }

func (importTestDisk) Kind() platform.Kind { return platform.KindWindows }

func (importTestDisk) PhysicalSize(string) (int64, error) { return 4096, nil }

func (importTestDisk) Fingerprint(string) (string, error) { return "fp-import-test", nil }

// newImportTestApp 构造带真实配置、Store 与假磁盘后端的 App，并登记一个启用存储根 root。
//
// 与 newOrphanTestApp 同样的理由：disks 目录名是"根 + disks_dir"拼出来的，
// 缺了配置项 Resolve("") 会退化成根本身，白名单校验就形同虚设。
func newImportTestApp(t *testing.T, root string) (*App, *store.Store) {
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
		Disk:  importTestDisk{},
	}}
	if err := st.CreateStorage(context.Background(), &domain.Storage{
		Name:    "import-test",
		Path:    root,
		Enabled: true,
	}); err != nil {
		t.Fatalf("登记存储根失败：%v", err)
	}
	return app, st
}

// importOwner 建一个启用的普通用户，作为导入库的归属（导入必须显式指定 owner）。
func importOwner(t *testing.T, st *store.Store) *domain.User {
	t.Helper()
	user := &domain.User{Username: "importer", Role: domain.RoleUser, Enabled: true}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("建归属用户失败：%v", err)
	}
	return user
}

// writeDiskMeta 在磁盘文件旁写一份 sidecar。
func writeDiskMeta(t *testing.T, vhdxPath string, meta DiskMetaFile) {
	t.Helper()
	if err := writeDiskMetaFile(vhdxPath, meta); err != nil {
		t.Fatalf("写元数据失败：%v", err)
	}
}

// TestScanDiskImportsClassifiesCandidates 锁定导入页面的候选口径：
// 列出**全部** .vhdx（含已登记的），逐条给出分类、可导入性、建议名称与容量来源。
//
// 与孤儿扫描口径的差别正是这个测试要钉住的：导入页面必须让管理员看到"我那 8 块盘
// 是不是都搬过来了"，因此已登记的盘也要出现（标成"已存在（跳过）"），而不是藏起来。
func TestScanDiskImportsClassifiesCandidates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	app, st := newImportTestApp(t, root)

	// 母盘 + sidecar：建议名称与容量都该取自元数据（可信）。
	parent := writeOrphanFile(t, filepath.Join(root, "disks", "parents", "p1"), "base.vhdx", 8192)
	writeDiskMeta(t, parent, DiskMetaFile{
		Name:          "财务Win10",
		Note:          "财务的 Win10 母盘",
		CapacityBytes: 64 << 30,
		FileSystem:    string(domain.FSNTFS),
		VHDType:       string(domain.VHDTypeDynamic),
	})

	// 独享盘、无 sidecar：容量只能按文件大小估，界面据此提示需复核。
	standalone := writeOrphanFile(t, filepath.Join(root, "disks", "standalone", "s1"), "disk.vhdx", 4096)

	// 未登记的差异盘：不可独立成库。
	diff := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "d1"), "diff.vhdx", 1024)

	// 已登记的母盘：跳过。
	registered := writeOrphanFile(t, filepath.Join(root, "disks", "parents", "p2"), "known.vhdx", 512)
	repoID := seedRepo(t, st)
	if err := st.CreateDisk(ctx, &domain.Disk{
		RepoID: repoID, Kind: domain.DiskKindParent, VHDXPath: registered,
		SizeBytes: 512, State: domain.DiskStateReady,
	}); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	// 非 vhdx 文件：不参与统计。
	writeOrphanFile(t, filepath.Join(root, "disks", "parents", "p1"), "readme.txt", 8)

	scan, err := app.ScanDiskImports(ctx)
	if err != nil {
		t.Fatalf("扫描导入候选失败：%v", err)
	}
	if scan.Checked != 4 {
		t.Fatalf("扫过的 .vhdx 数应为 4，实际=%d", scan.Checked)
	}
	if len(scan.Candidates) != 4 {
		t.Fatalf("候选应为 4 条，实际=%d：%+v", len(scan.Candidates), scan.Candidates)
	}
	if scan.ImportableCount != 2 {
		t.Fatalf("可导入数应为 2（母盘 + 独享盘），实际=%d", scan.ImportableCount)
	}

	byPath := map[string]DiskImportCandidate{}
	for _, c := range scan.Candidates {
		byPath[c.Path] = c
	}
	if c, ok := byPath[parent]; !ok {
		t.Fatal("母盘应出现在候选里")
	} else if c.Kind != DiskImportKindParent || !c.Importable || c.SkipReason != "" {
		t.Fatalf("母盘应可导入且分类为 parent：%+v", c)
	} else if c.SuggestedName != "财务Win10" {
		t.Fatalf("建议名称应取自元数据，实际=%q", c.SuggestedName)
	} else if c.SuggestedCapacityBytes != 64<<30 || c.CapacitySource != DiskImportCapacityMeta {
		t.Fatalf("建议容量应取自元数据（meta），实际=%d/%s", c.SuggestedCapacityBytes, c.CapacitySource)
	} else if c.Meta == nil || c.Meta.Note != "财务的 Win10 母盘" {
		t.Fatalf("候选应带上读到的元数据：%+v", c.Meta)
	}

	if c, ok := byPath[standalone]; !ok {
		t.Fatal("独享盘应出现在候选里")
	} else if c.Kind != DiskImportKindStandalone || !c.Importable {
		t.Fatalf("独享盘应可导入且分类为 standalone：%+v", c)
	} else if c.CapacitySource != DiskImportCapacityFile || c.SuggestedCapacityBytes != 4096 {
		t.Fatalf("无元数据时容量应按文件大小估（file）：%+v", c)
	}

	if c, ok := byPath[diff]; !ok {
		t.Fatal("差异盘应出现在候选里（不藏起来，界面才解释得了为什么不能用）")
	} else if c.Kind != DiskImportKindDiff || c.Importable || c.SkipReason != DiskImportSkipDiff {
		t.Fatalf("差异盘应不可导入且给出 diff_disk：%+v", c)
	}

	if c, ok := byPath[registered]; !ok {
		t.Fatal("已登记的盘也应出现在候选里（用户要确认盘是不是都搬过来了）")
	} else if !c.Registered || c.Importable || c.SkipReason != DiskImportSkipRegistered {
		t.Fatalf("已登记的盘应标为已存在并跳过：%+v", c)
	}
}

// TestImportDisksTakesOverInPlace 覆盖"原地接管"的完整语义：
// 文件不移动不复制 → 落库（库 + 盘 + 母盘指针）→ 回写 sidecar。
func TestImportDisksTakesOverInPlace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	app, st := newImportTestApp(t, root)
	owner := importOwner(t, st)

	parent := writeOrphanFile(t, filepath.Join(root, "disks", "parents", "p1"), "base.vhdx", 8192)
	writeDiskMeta(t, parent, DiskMetaFile{
		Name:          "财务Win10",
		Note:          "财务的 Win10 母盘，别删",
		CapacityBytes: 64 << 30,
		FileSystem:    string(domain.FSNTFS),
		MaxDiffDisks:  5,
		Source: &DiskMetaSource{
			Kind: DiskMetaSourceDir, Dir: `D:\src\win10`, Group: "财务", Host: "old-server",
		},
	})

	res, err := app.ImportDisks(ctx, "actor-1", DiskImportInput{
		OwnerID: owner.ID,
		Items:   []DiskImportItem{{Path: parent}},
	})
	if err != nil {
		t.Fatalf("导入磁盘失败：%v", err)
	}
	if len(res.Imported) != 1 || len(res.Skipped) != 0 || len(res.Failed) != 0 {
		t.Fatalf("应恰好成功一条：%+v", res)
	}
	got := res.Imported[0]
	if got.Path != parent {
		t.Fatalf("导入结果应回原路径：%q", got.Path)
	}
	if got.CapacityBytes != 64<<30 || got.CapacityEstimated {
		t.Fatalf("容量应取自元数据且不标为估算：%+v", got)
	}
	if got.Mode != string(domain.RepoModeShared) || got.Kind != string(domain.DiskKindParent) {
		t.Fatalf("母盘应建成共享库的母盘：%+v", got)
	}

	// 原地接管：文件必须还在原处（不移动、不复制）。
	if _, err := os.Stat(parent); err != nil {
		t.Fatalf("导入不得移动磁盘文件：%v", err)
	}

	repo, err := st.GetRepository(ctx, got.RepoID)
	if err != nil {
		t.Fatalf("读回存储库失败：%v", err)
	}
	if repo.Name != "财务Win10" || repo.OwnerID != owner.ID || repo.Mode != domain.RepoModeShared {
		t.Fatalf("库记录不对：%+v", repo)
	}
	if repo.Meta.Note != "财务的 Win10 母盘，别删" || repo.Meta.Group != "财务" {
		t.Fatalf("备注与分组应随元数据恢复：%+v", repo.Meta)
	}
	if repo.MaxDiffDisks != 5 {
		t.Fatalf("差异盘上限应取自元数据，实际=%d", repo.MaxDiffDisks)
	}
	if repo.ParentDiskID == nil {
		t.Fatal("共享库必须回填母盘指针（库容量与分配都靠它定位）")
	}

	disk, err := st.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		t.Fatalf("读回磁盘失败：%v", err)
	}
	if disk.VHDXPath != parent || disk.Kind != domain.DiskKindParent {
		t.Fatalf("磁盘记录不对：%+v", disk)
	}
	if disk.SizeBytes != 64<<30 {
		t.Fatalf("磁盘标称容量应为元数据里的值（客户端按容量精确匹配），实际=%d", disk.SizeBytes)
	}
	if disk.ContentFingerprint != "fp-import-test" {
		t.Fatalf("导入时应尽力补上内容指纹，实际=%q", disk.ContentFingerprint)
	}

	// sidecar 回写：从此这块盘带着名字与来历，下次迁移还能认出来。
	meta, err := readDiskMetaFile(parent)
	if err != nil {
		t.Fatalf("读回元数据失败：%v", err)
	}
	if meta == nil {
		t.Fatal("导入后必须回写元数据文件")
	}
	if meta.Name != "财务Win10" || meta.CapacityBytes != 64<<30 {
		t.Fatalf("回写的元数据不对：%+v", meta)
	}
	if meta.Source == nil || meta.Source.Kind != DiskMetaSourceImport {
		t.Fatalf("回写应把来历标成 import：%+v", meta.Source)
	}
	if meta.Source.Dir != `D:\src\win10` {
		t.Fatalf("原有源目录信息应被保留，实际=%q", meta.Source.Dir)
	}

	// 再导一次：用户口径"已有的跳过"。
	again, err := app.ImportDisks(ctx, "actor-1", DiskImportInput{
		OwnerID: owner.ID,
		Items:   []DiskImportItem{{Path: parent}},
	})
	if err != nil {
		t.Fatalf("重复导入不应报错：%v", err)
	}
	if len(again.Imported) != 0 || len(again.Skipped) != 1 || again.Skipped[0].Code != DiskImportSkipRegistered {
		t.Fatalf("重复导入应跳过：%+v", again)
	}
}

// TestImportDisksGuards 钉住逐条校验：非法路径/越界路径/文件不存在/元数据损坏
// 一律失败而不是静默跳过，差异盘与已登记盘则跳过。
func TestImportDisksGuards(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	app, st := newImportTestApp(t, root)
	owner := importOwner(t, st)

	diff := writeOrphanFile(t, filepath.Join(root, "disks", "diffs", "d1"), "diff.vhdx", 1024)
	// sidecar 损坏：刻意不降级成"没有元数据"—— 里面可能带着容量，容量错了客户端匹配不到盘。
	badMeta := writeOrphanFile(t, filepath.Join(root, "disks", "parents", "p3"), "broken.vhdx", 4096)
	if err := os.WriteFile(diskMetaPath(badMeta), []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("写坏元数据失败：%v", err)
	}
	missing := filepath.Join(root, "disks", "parents", "gone", "gone.vhdx")
	notVHDX := writeOrphanFile(t, filepath.Join(root, "disks", "parents", "p4"), "notes.txt", 8)
	// 在存储根内但不在 disks/ 下：不属于磁盘目录，不得被登记。
	outsideDisks := writeOrphanFile(t, filepath.Join(root, "meta", "orphan"), "moved.vhdx", 64)

	registered := writeOrphanFile(t, filepath.Join(root, "disks", "parents", "p5"), "known.vhdx", 512)
	repoID := seedRepo(t, st)
	if err := st.CreateDisk(ctx, &domain.Disk{
		RepoID: repoID, Kind: domain.DiskKindParent, VHDXPath: registered,
		SizeBytes: 512, State: domain.DiskStateReady,
	}); err != nil {
		t.Fatalf("建磁盘记录失败：%v", err)
	}

	res, err := app.ImportDisks(ctx, "actor-1", DiskImportInput{
		OwnerID: owner.ID,
		Items: []DiskImportItem{
			{Path: "  "},
			{Path: filepath.Join("disks", "parents", "p9", "rel.vhdx")},
			{Path: notVHDX},
			{Path: outsideDisks},
			{Path: filepath.Join(t.TempDir(), "outside.vhdx")},
			{Path: missing},
			{Path: badMeta},
			{Path: diff},
			{Path: registered},
		},
	})
	if err != nil {
		t.Fatalf("批量导入不应整体失败：%v", err)
	}
	if len(res.Imported) != 0 {
		t.Fatalf("这一批不该有任何成功项：%+v", res.Imported)
	}

	failed := map[string]string{}
	for _, f := range res.Failed {
		failed[f.Path] = f.Code
	}
	// 前四项是"路径非法"：空路径 / 相对路径 / 非 vhdx / 不在 disks 下 / 根外。
	invalidCount := 0
	for _, code := range failed {
		if code == DiskImportFailInvalid {
			invalidCount++
		}
	}
	if invalidCount != 5 {
		t.Fatalf("应有 5 条 path_invalid，实际=%d（%+v）", invalidCount, res.Failed)
	}
	if code := failed[missing]; code != DiskImportFailNotFound {
		t.Fatalf("缺失文件应报 not_found，实际=%q", code)
	}
	if code := failed[badMeta]; code != DiskImportFailMetaBad {
		t.Fatalf("元数据损坏应报 meta_unreadable，实际=%q", code)
	}

	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.Path] = s.Code
	}
	if skipped[diff] != DiskImportSkipDiff {
		t.Fatalf("差异盘应跳过（diff_disk），实际=%q", skipped[diff])
	}
	if skipped[registered] != DiskImportSkipRegistered {
		t.Fatalf("已登记盘应跳过（already_registered），实际=%q", skipped[registered])
	}

	// 被拒绝的路径不得产生任何库记录（只有 seedRepo 那一个）。
	repos, err := st.ListRepositories(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("列存储库失败：%v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("失败的导入不得建库，实际库数=%d", len(repos))
	}
}

// TestImportDisksRequestValidation 覆盖整批级的参数校验：空清单、归属用户非法。
func TestImportDisksRequestValidation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	app, st := newImportTestApp(t, root)
	owner := importOwner(t, st)
	disabled := &domain.User{Username: "disabled", Role: domain.RoleUser, Enabled: false}
	if err := st.CreateUser(ctx, disabled); err != nil {
		t.Fatalf("建停用用户失败：%v", err)
	}

	if _, err := app.ImportDisks(ctx, "actor-1", DiskImportInput{OwnerID: owner.ID}); apperr.CodeOf(err) != "system.disk_import_empty" {
		t.Fatalf("空清单应报 disk_import_empty，实际=%v", err)
	}
	if _, err := app.ImportDisks(ctx, "actor-1", DiskImportInput{
		OwnerID: "no-such-user",
		Items:   []DiskImportItem{{Path: filepath.Join(root, "disks", "parents", "x", "x.vhdx")}},
	}); apperr.CodeOf(err) != "system.disk_import_owner_invalid" {
		t.Fatalf("归属用户不存在应报 owner_invalid，实际=%v", err)
	}
	if _, err := app.ImportDisks(ctx, "actor-1", DiskImportInput{
		OwnerID: disabled.ID,
		Items:   []DiskImportItem{{Path: filepath.Join(root, "disks", "parents", "x", "x.vhdx")}},
	}); apperr.CodeOf(err) != "system.disk_import_owner_invalid" {
		t.Fatalf("归属用户停用应报 owner_invalid，实际=%v", err)
	}
}

// TestImportDisksSuffixesConflictingName 覆盖名称冲突：迁移后老名字很可能已被新机器上的库占用，
// 此时自动加序号而不是让这条导入失败（用户要的是"这块盘能用"，不是"名字必须一模一样"）。
func TestImportDisksSuffixesConflictingName(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	app, st := newImportTestApp(t, root)
	owner := importOwner(t, st)
	// seedRepo 建的库名为 repo-1，正好占住下面这块盘元数据里的名字。
	seedRepo(t, st)

	parent := writeOrphanFile(t, filepath.Join(root, "disks", "parents", "p1"), "base.vhdx", 4096)
	writeDiskMeta(t, parent, DiskMetaFile{Name: "repo-1", CapacityBytes: 1 << 30})

	res, err := app.ImportDisks(ctx, "actor-1", DiskImportInput{
		OwnerID: owner.ID,
		Items:   []DiskImportItem{{Path: parent}},
	})
	if err != nil {
		t.Fatalf("导入失败：%v", err)
	}
	if len(res.Imported) != 1 {
		t.Fatalf("名称冲突不该让导入失败：%+v", res)
	}
	if res.Imported[0].Name != "repo-1-2" {
		t.Fatalf("冲突时应自动加序号，实际=%q", res.Imported[0].Name)
	}
}

// TestImportDiskCapacityPriority 覆盖容量取值优先级：请求显式填 > 元数据 > 文件大小。
//
// 容量是导入里最要命的字段（客户端按容量精确匹配本地盘），因此"谁能覆盖谁"必须有确定的次序。
func TestImportDiskCapacityPriority(t *testing.T) {
	fileSize := int64(4096)
	meta := &DiskMetaFile{CapacityBytes: 1 << 30}

	if got, src := pickImportCapacity(DiskImportItem{CapacityBytes: 2 << 30}, meta, fileSize); got != 2<<30 || src != DiskImportCapacityManual {
		t.Fatalf("请求填的容量应优先，实际=%d/%s", got, src)
	}
	if got, src := pickImportCapacity(DiskImportItem{}, meta, fileSize); got != 1<<30 || src != DiskImportCapacityMeta {
		t.Fatalf("其次用元数据里的容量，实际=%d/%s", got, src)
	}
	if got, src := pickImportCapacity(DiskImportItem{}, nil, fileSize); got != fileSize || src != DiskImportCapacityFile {
		t.Fatalf("最后按文件大小估，实际=%d/%s", got, src)
	}
	if got, src := pickImportCapacity(DiskImportItem{}, nil, 0); got != 0 || src != DiskImportCapacityFile {
		t.Fatalf("没有任何来源时应为 0 并由调用方判失败，实际=%d/%s", got, src)
	}
	if got, _ := pickImportCapacity(DiskImportItem{CapacityBytes: 3 << 30}, nil, fileSize); got != 3<<30 {
		t.Fatalf("没有元数据时请求填的值仍应生效，实际=%d", got)
	}
}

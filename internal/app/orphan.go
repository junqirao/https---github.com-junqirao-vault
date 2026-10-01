package app

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform"
)

// 孤儿磁盘文件 = 存储根 disks/ 目录下**未被数据库登记**的 .vhdx。
//
// 产生原因都是真实发生过的：建库/派生在"落盘之后、写库之前"失败、管理员手工删过记录、
// 数据库被还原到更早的时间点等。这类文件本来只由对账（reconcile.go）发现，而它"仅报告"
// 的表现形式就是刷一屏 WARN，用户看到日志却无处处理（真实反馈："打出来没用啊，干脆直接
// 给个管理页面能手动删掉这些盘得了"）。本文件提供管理端「孤儿磁盘」页面所需的两件事：
//
//	ScanOrphanFiles  —— 现扫一遍，给出可勾可删的清单（不读对账报告的旧快照）；
//	DeleteOrphanFile —— 逐个删除，删除前**重新校验**它确实还不是登记在册的盘。
//
// 为什么仍然不自动删：差异盘/母盘可能承载用户数据（见 5.11 的取舍），而删除不可逆。
// 把不可逆操作留在管理员的判断之下，既不丢数据，也让日志不必再逐条刷。

// 孤儿文件分类（与对账报告里的 kind 一致）。
const (
	OrphanKindDiff   = "diff"
	OrphanKindParent = "parent"
	OrphanKindOther  = "other"
)

// orphanAdminPageHint 是"这些文件该去哪儿处理"的指路文案（日志与对账报告共用）。
//
// 真实反馈就是"打出来没用啊"：日志只报"发现孤儿文件"而不说去哪儿处理，用户无从下手。
const orphanAdminPageHint = "管理端「孤儿磁盘」页面（管理端 → 孤儿磁盘，/admin/orphans）"

// orphanScanSkipOtherPlatform 说明"为什么这台机器没有孤儿文件可扫"。
//
// 本扫描的前提是"磁盘 = storages.path 目录下的 .vhdx 文件"，只对 Windows 成立；
// Linux 上磁盘是 LVM thin LV（不在 storages.path 目录里）。
const orphanScanSkipOtherPlatform = "orphan_vhdx_scan: 仅适用于 Windows 的 VHDX 文件布局，当前平台已跳过"

// OrphanFile 是一个未被数据库登记的磁盘文件（管理端「孤儿磁盘」的一行）。
type OrphanFile struct {
	// Path 绝对路径。
	Path string `json:"path"`
	// Root 该文件所属的存储根（界面用来分组与展示）。
	Root string `json:"root"`
	// Kind diff / parent / other —— 差异盘与母盘可能承载用户数据，界面据此加重提示。
	Kind string `json:"kind"`
	// SizeBytes 文件长度（字节）。
	//
	// 注意：VHDX 多为稀疏文件，这里给的是**逻辑大小**而非物理占用（拿物理占用要对每个
	// 文件调一次 PowerShell，会把"扫一眼"变成几十秒的等待）。界面按"文件大小"表述。
	SizeBytes int64 `json:"size_bytes"`
	// ModifiedAt 文件最后修改时间（毫秒）。
	ModifiedAt int64 `json:"modified_at"`
}

// OrphanScan 是一次孤儿扫描的结果。
type OrphanScan struct {
	// At 扫描时间（毫秒）。
	At int64 `json:"at"`
	// Roots 本次覆盖的存储根。
	Roots []string `json:"roots"`
	// Checked 扫过的 .vhdx 文件数（含已登记的，用来表明"确实扫过了"）。
	Checked int `json:"checked"`
	// Files 未被登记的文件（按路径排序，界面顺序稳定）。
	Files []OrphanFile `json:"files"`
	// TotalBytes Files 的大小合计。
	TotalBytes int64 `json:"total_bytes"`
	// Skipped 未能扫描的位置说明（平台不支持、根不可访问等），界面原样展示。
	Skipped []string `json:"skipped,omitempty"`
}

// OrphanDeleteResult 是一次孤儿文件删除的结果。
type OrphanDeleteResult struct {
	// Path 被删除的文件绝对路径。
	Path string `json:"path"`
	// FreedBytes 释放的字节数（文件长度）。
	FreedBytes int64 `json:"freed_bytes"`
}

// ScanOrphanFiles 扫描所有生效存储根，返回未被登记的磁盘文件清单（仅超级管理员接口调用）。
func (a *App) ScanOrphanFiles(ctx context.Context) (*OrphanScan, error) {
	dbPaths, err := a.registeredDiskPaths(ctx)
	if err != nil {
		return nil, err
	}
	guards, err := a.storageGuards(ctx)
	if err != nil {
		return nil, err
	}
	out := &OrphanScan{At: time.Now().UnixMilli(), Roots: guards.Roots(), Files: []OrphanFile{}}
	if a.platformKind() != platform.KindWindows {
		out.Skipped = append(out.Skipped, orphanScanSkipOtherPlatform)
		return out, nil
	}
	if guards.Empty() {
		out.Skipped = append(out.Skipped, "orphan_vhdx_scan: 未配置任何启用的存储，无处可扫")
		return out, nil
	}
	files, checked, skipped := a.collectOrphanFiles(ctx, guards, dbPaths, a.Log)
	out.Checked = checked
	out.Skipped = append(out.Skipped, skipped...)
	out.Files = files
	for i := range files {
		out.TotalBytes += files[i].SizeBytes
	}
	return out, nil
}

// DeleteOrphanFile 手动删除一个孤儿磁盘文件（管理端「孤儿磁盘」页面）。
//
// 四道闸门，顺序即"最便宜的判断在前"：
//  1. 绝对路径、扩展名 .vhdx —— 这是本系统里唯一"按用户给的路径直接删文件"的接口，
//     参数校验就是最后一道防线；
//  2. 必须落在某个**生效存储根**的 disks 目录之下（根白名单 + disks_dir 双重约束：
//     即使用户把根配成整个 C:\，也只可能删到 C:\disks\ 下的 .vhdx）；
//  3. **重新查库**确认没有任何磁盘记录引用它 —— 页面上的清单是扫描那一刻的快照，
//     期间文件可能已经被重新用上（建库/派生），只信"此刻的数据库"；
//  4. 文件确实存在且不是目录。
//
// 删除不可逆（不做软删除、不搬去孤儿目录）：用户要的就是释放空间，而能走到这里的文件
// 一定是数据库里没有任何记录指向的。被进程占用时 Windows 会直接拒绝删除，原样报错，
// 用户据此先关掉占用它的程序。
func (a *App) DeleteOrphanFile(ctx context.Context, path string) (*OrphanDeleteResult, error) {
	raw := strings.TrimSpace(path)
	if raw == "" {
		return nil, apperr.InvalidParam("path")
	}
	abs := filepath.Clean(raw)
	if abs == "." || !filepath.IsAbs(abs) || !strings.EqualFold(filepath.Ext(abs), ".vhdx") {
		return nil, apperr.OrphanPathInvalid().WithArg("path", raw)
	}

	guards, err := a.storageGuards(ctx)
	if err != nil {
		return nil, err
	}
	guard, ok := guards.GuardForPath(abs)
	if !ok {
		return nil, apperr.OrphanPathInvalid().WithArg("path", abs)
	}
	disksRoot, ok := a.disksDirOf(guard)
	if !ok || !domain.UnderRoot(disksRoot, abs) {
		return nil, apperr.OrphanPathInvalid().WithArg("path", abs)
	}

	dbPaths, err := a.registeredDiskPaths(ctx)
	if err != nil {
		return nil, err
	}
	if dbPaths[pathKeyLocal(abs)] {
		return nil, apperr.OrphanRegistered().WithArg("path", abs)
	}

	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apperr.OrphanNotFound().WithArg("path", abs)
		}
		return nil, apperr.OrphanDeleteFailed().WithArg("path", abs).WithCause(err)
	}
	if fi.IsDir() {
		return nil, apperr.OrphanPathInvalid().WithArg("path", abs)
	}
	if err := os.Remove(abs); err != nil {
		return nil, apperr.OrphanDeleteFailed().WithArg("path", abs).WithCause(err)
	}
	a.Log.Warn("已手动删除孤儿磁盘文件", "path", abs, "size_bytes", fi.Size())
	return &OrphanDeleteResult{Path: abs, FreedBytes: fi.Size()}, nil
}

// disksDirOf 解析某个根下的"磁盘目录"（默认 disks/）。
//
// 返回 ok=false 的两种情况都表示"这个根不适用文件式磁盘布局"：
//   - 配置里没有 disks 子目录名；
//   - 解析结果就是根本身（没配 disks_dir 或配成 "."）。
//
// 第二种必须挡住：那意味着"根下所有 .vhdx 都算磁盘文件"，而根里还有 meta/、staging/
// 等同样放 .vhdx 的地方。删除接口的白名单绝不能是"整个根"，宁可不扫/不删。
func (a *App) disksDirOf(guard domain.PathGuard) (string, bool) {
	rel := strings.TrimSpace(a.raw().Storage.DisksDir)
	if rel == "" {
		return "", false
	}
	dir, err := guard.Resolve(rel)
	if err != nil || strings.EqualFold(filepath.Clean(dir), filepath.Clean(guard.Root)) {
		return "", false
	}
	return dir, true
}

// registeredDiskPaths 返回数据库里全部磁盘的 VHDX 路径键（pathKeyLocal 归一，大小写不敏感）。
func (a *App) registeredDiskPaths(ctx context.Context) (map[string]bool, error) {
	disks, err := a.Store.ListAllDisks(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(disks))
	for i := range disks {
		out[pathKeyLocal(disks[i].VHDXPath)] = true
	}
	return out, nil
}

// collectOrphanFiles 扫描各根 disks 目录下未被登记的文件（纯扫描，不做任何变更）。
//
// 互为父子的根只扫上游那一个，避免同一区域被重复报告；返回的 files 按路径排序。
// checked 是扫过的 .vhdx 总数（含已登记的），skipped 是未能扫描的位置说明。
func (a *App) collectOrphanFiles(ctx context.Context, guards domain.PathGuardSet,
	dbPaths map[string]bool, log *slog.Logger) (files []OrphanFile, checked int, skipped []string) {
	scanned := make([]string, 0, guards.Len())
	for _, guard := range guards.Guards() {
		if ctx.Err() != nil {
			break
		}
		if isUnderAny(guard.Root, scanned) {
			log.Debug("跳过被上游根覆盖的白名单根", "root", guard.Root)
			continue
		}
		scanned = append(scanned, guard.Root)

		disksRoot, ok := a.disksDirOf(guard)
		if !ok {
			skipped = append(skipped, "orphan_vhdx_scan: 根 "+guard.Root+" 未配置 disks 子目录，已跳过")
			continue
		}
		if _, statErr := os.Stat(disksRoot); statErr != nil {
			// 根下还没有 disks/（新配的存储）：不是问题，跳过即可，不必报。
			continue
		}
		_ = filepath.Walk(disksRoot, func(p string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil || fi == nil || fi.IsDir() {
				return nil
			}
			if ctx.Err() != nil {
				return fs.SkipAll
			}
			if !strings.EqualFold(filepath.Ext(p), ".vhdx") {
				return nil
			}
			checked++
			if dbPaths[pathKeyLocal(p)] {
				return nil
			}
			files = append(files, OrphanFile{
				Path:       p,
				Root:       guard.Root,
				Kind:       orphanKindOf(p, disksRoot),
				SizeBytes:  fi.Size(),
				ModifiedAt: fi.ModTime().UnixMilli(),
			})
			return nil
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, checked, skipped
}

// orphanKindOf 按文件在 disks/ 下的位置分类：diffs/ 下是差异盘、parents/ 下是母盘，其余归 other。
func orphanKindOf(p, disksRoot string) string {
	rel := strings.ToLower(filepath.ToSlash(strings.TrimPrefix(p, disksRoot)))
	switch {
	case strings.Contains(rel, "/diffs/") || strings.HasPrefix(rel, "/diffs/"):
		return OrphanKindDiff
	case strings.Contains(rel, "/parents/") || strings.HasPrefix(rel, "/parents/"):
		return OrphanKindParent
	default:
		return OrphanKindOther
	}
}

// orphanKindLabel 返回分类的中文名（用于对账报告措辞）。
func orphanKindLabel(kind string) string {
	switch kind {
	case OrphanKindDiff:
		return "差异盘"
	case OrphanKindParent:
		return "母盘"
	default:
		return "VHDX"
	}
}

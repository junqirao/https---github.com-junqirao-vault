package app

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform"
	"vault/internal/store"
)

// 导入已有母盘 = 把一块已躺在存储根 disks/ 下的 .vhdx 登记成存储库 + 磁盘记录，
// 文件原地不动（不复制、不移动）。
//
// 场景（用户原话）："母盘万一我想迁移别的系统，我完全不知道里面是什么"。换一台服务端、
// 把整个 disks/ 目录拷过去之后，那些文件在系统里什么都不是（管理端只在「孤儿磁盘」里
// 显示"路径 + 大小"），既不敢删也不知道怎么用回来。因此分两步：
//
//  1. 每个母盘旁边都有一份元数据 sidecar（见 diskmeta.go），说明它是什么；
//  2. 本文件提供"扫描候选 → 原地接管"，元数据缺失的老盘也能导入（管理员补名称与容量）。
//
// 与 orphan.go 的关系：两者扫同一批文件，目的相反 —— 孤儿页面处理"不要的"（删除），
// 导入页面处理"要的"（接管）。因此判定口径必须一致（同一套根白名单、同一套 .vhdx 判定、
// 同一份登记集合），差别只在未登记的文件该删还是该用。
//
// 为什么不自动导入：登记一块盘等于把它变成可分配的存储库（用户能挂载它）。
// 扫描是"看"、导入是"用"，把"看"直接变成"用"会让一次误扫把测试盘变成生产库，
// 所以扫描只读、导入必须由管理员逐条确认。

// 候选分类：按文件在 disks/ 下的位置判定（与 orphanKindOf 同一套目录语义）。
const (
	// DiskImportKindParent disks/parents/** —— 共享库的母盘（差异盘的基线）。
	DiskImportKindParent = "parent"
	// DiskImportKindStandalone disks/standalone/** —— 独享库的单盘（整块给一个用户）。
	DiskImportKindStandalone = "standalone"
	// DiskImportKindDiff disks/diffs/** —— 差异盘，**不可导入**。
	//
	// 它只存增量块，必须挂在某个母盘上才存在：单独接管既读不出内容也无法挂载。
	// 迁移时正确的做法是导入母盘、让差异盘按需重新派生。
	DiskImportKindDiff = "diff"
	// DiskImportKindOther 不在标准布局里的 .vhdx（手工拷进来的、老版本布局），按独享库处理。
	DiskImportKindOther = "other"
)

// 逐条跳过（不是错误）与失败（需要处理）的原因码。全部是稳定英文码，界面负责本地化。
const (
	// DiskImportSkipRegistered 数据库里已有这条路径 —— 用户口径："已有的跳过"。
	DiskImportSkipRegistered = "already_registered"
	// DiskImportSkipDiff 差异盘，不能独立成库（见 DiskImportKindDiff）。
	DiskImportSkipDiff = "diff_disk"

	// DiskImportFailInvalid 路径不是绝对路径 / 不是 .vhdx / 不在生效存储根的 disks 下。
	DiskImportFailInvalid = "path_invalid"
	// DiskImportFailNotFound 文件不存在（清单是扫描那一刻的快照，期间可能已被删）。
	DiskImportFailNotFound = "not_found"
	// DiskImportFailMetaBad 有 sidecar 却读不出来（JSON 损坏 / 权限不足）。
	//
	// 刻意不降级成"没有元数据"继续导入：sidecar 里带着**容量**，而容量错了客户端就永远
	// 匹配不到这块盘（见 diskmeta.go 的 CapacityBytes）。宁可让用户去修文件。
	DiskImportFailMetaBad = "meta_unreadable"
	// DiskImportFailCapacity 容量非法（<= 0）。
	DiskImportFailCapacity = "capacity_invalid"
	// DiskImportFailCreate 落库失败（并发抢占、数据库错误等）。
	DiskImportFailCreate = "create_failed"
)

// 容量来源：告诉界面"这个数字有多可信"。
const (
	// DiskImportCapacityMeta 来自 sidecar（原机器写的标称容量，可信）。
	DiskImportCapacityMeta = "meta"
	// DiskImportCapacityManual 管理员在导入界面上填的。
	DiskImportCapacityManual = "manual"
	// DiskImportCapacityFile 按**文件大小**估的。VHDX 是稀疏文件，这个值几乎必然偏小，
	// 界面必须显著提示：容量对不上，客户端会匹配不到盘。
	DiskImportCapacityFile = "file"
)

// diskImportMaxItems 单次导入的条数上限。
//
// 不做分页是刻意的：批量导入是迁移时的一次性操作，几百块盘一条请求最省事；
// 上限只为挡住"把整个磁盘填进 JSON"这种明显误用。
const diskImportMaxItems = 1000

// diskImportNameLimit 存储库名称长度上限（与建库接口的校验一致）。
const diskImportNameLimit = 120

// DiskImportCandidate 是导入页面上的一行候选（扫描结果是只读快照）。
type DiskImportCandidate struct {
	// Path 磁盘文件绝对路径（导入时原样回传）。
	Path string `json:"path"`
	// Root 所属存储根。
	Root string `json:"root"`
	// Kind parent / standalone / diff / other（见 DiskImportKind*）。
	Kind string `json:"kind"`
	// SizeBytes 文件长度（字节）—— 注意是**文件大小**，不是盘的标称容量。
	SizeBytes int64 `json:"size_bytes"`
	// ModifiedAt 文件最后修改时间（毫秒）。
	ModifiedAt int64 `json:"modified_at"`
	// MetaPath sidecar 路径（无论存不存在都给，便于界面提示"去哪里补元数据"）。
	MetaPath string `json:"meta_path,omitempty"`
	// Meta 读到的元数据（没有 sidecar 时为 null）。
	Meta *DiskMetaFile `json:"meta,omitempty"`
	// MetaError sidecar 存在但读不出来时的原因。
	MetaError string `json:"meta_error,omitempty"`
	// Registered 数据库里已有这条路径（导入时会被跳过）。
	Registered bool `json:"registered"`
	// Importable 可导入（= 未登记 + 不是差异盘 + 元数据可读）。
	Importable bool `json:"importable"`
	// SkipReason 不可导入时的原因码（Importable=false 才有值）。
	SkipReason string `json:"skip_reason,omitempty"`
	// SuggestedName 建议的存储库名称（元数据名称 → 父目录名兜底），界面预填、可改。
	SuggestedName string `json:"suggested_name"`
	// SuggestedCapacityBytes 建议的标称容量（见 CapacitySource）。
	SuggestedCapacityBytes int64 `json:"suggested_capacity_bytes"`
	// CapacitySource meta / manual / file（见 DiskImportCapacity*）。
	CapacitySource string `json:"capacity_source"`
}

// DiskImportScan 是一次导入扫描的结果。
type DiskImportScan struct {
	// At 扫描时间（毫秒）。
	At int64 `json:"at"`
	// Roots 本次覆盖的存储根。
	Roots []string `json:"roots"`
	// Checked 扫过的 .vhdx 文件数（含已登记与不可导入的）。
	Checked int `json:"checked"`
	// ImportableCount 其中可导入的数量（界面用来做汇总与"全选"）。
	ImportableCount int `json:"importable_count"`
	// Candidates 全部候选（含已登记与差异盘：界面要能让用户看到"它为什么被跳过"，
	// 而不是把它藏起来让用户怀疑"我那块盘怎么不见了"）。
	Candidates []DiskImportCandidate `json:"candidates"`
	// Skipped 未能扫描的位置说明（平台不支持、根不可访问等），界面原样展示。
	Skipped []string `json:"skipped,omitempty"`
}

// DiskImportItem 是一条导入请求。
type DiskImportItem struct {
	// Path 磁盘文件绝对路径（来自扫描结果的 Path）。
	Path string `json:"path"`
	// Name 存储库名称；空则用元数据里的名称，再退到父目录名。
	Name string `json:"name,omitempty"`
	// Note 说明备注；空则用元数据里的备注。
	Note string `json:"note,omitempty"`
	// CapacityBytes 标称容量（字节）；0 则用元数据里的容量，再退到文件大小。
	CapacityBytes int64 `json:"capacity_bytes,omitempty"`
}

// DiskImportInput 是一次批量导入请求。
type DiskImportInput struct {
	// OwnerID 归属用户（必填）：迁移后原主人在新机器上不一定存在，
	// 因此由管理员显式指定这些库归属谁。
	OwnerID string `json:"owner_id"`
	// Items 要导入的磁盘清单。
	Items []DiskImportItem `json:"items"`
}

// ImportedDisk 是一条导入成功的结果。
type ImportedDisk struct {
	Path          string `json:"path"`
	RepoID        string `json:"repo_id"`
	Name          string `json:"name"`
	Mode          string `json:"mode"`
	Kind          string `json:"kind"`
	CapacityBytes int64  `json:"capacity_bytes"`
	// CapacityEstimated 容量是**按文件大小估的**（可能偏小 → 客户端可能匹配不到盘）。
	CapacityEstimated bool `json:"capacity_estimated"`
}

// DiskImportSkip 是一条被跳过的结果。
type DiskImportSkip struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

// DiskImportFailure 是一条失败的结果。
type DiskImportFailure struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// DiskImportResult 是一次批量导入的结果。
//
// 逐条返回而不是"全成或全败"：迁移时几十块盘里有一块读不出来（被占用、权限不足），
// 不该让其余全部陪葬。界面按三类分别展示。
type DiskImportResult struct {
	Imported []ImportedDisk      `json:"imported"`
	Skipped  []DiskImportSkip    `json:"skipped"`
	Failed   []DiskImportFailure `json:"failed"`
}

// ScanDiskImports 扫描所有生效存储根，列出可导入的磁盘文件（仅超级管理员调用）。
//
// 与 ScanOrphanFiles 的唯一区别：它返回**全部** .vhdx（含已登记的）。导入页面的任务不是
// "找出多余的"，而是"让管理员决定哪些文件该用起来"——已登记的必须出现并被标成
// "已存在（跳过）"，否则管理员无法确认"我那 8 块盘是不是都搬过来了"。
func (a *App) ScanDiskImports(ctx context.Context) (*DiskImportScan, error) {
	guards, err := a.storageGuards(ctx)
	if err != nil {
		return nil, err
	}
	dbPaths, err := a.registeredDiskPaths(ctx)
	if err != nil {
		return nil, err
	}
	out := &DiskImportScan{At: time.Now().UnixMilli(), Roots: guards.Roots(), Candidates: []DiskImportCandidate{}}
	if a.platformKind() != platform.KindWindows {
		out.Skipped = append(out.Skipped, orphanScanSkipOtherPlatform)
		return out, nil
	}
	if guards.Empty() {
		out.Skipped = append(out.Skipped, "disk_import_scan: 未配置任何启用的存储，无处可扫")
		return out, nil
	}
	candidates, checked, skipped := a.collectImportCandidates(ctx, guards, dbPaths)
	out.Checked = checked
	out.Skipped = append(out.Skipped, skipped...)
	out.Candidates = candidates
	for i := range candidates {
		if candidates[i].Importable {
			out.ImportableCount++
		}
	}
	return out, nil
}

// collectImportCandidates 遍历各存储根的 disks 目录产出候选清单（只读，不做任何变更）。
//
// 与 collectOrphanFiles 一样，互为父子的根只扫上游那一个，避免同一区域重复出现。
func (a *App) collectImportCandidates(ctx context.Context, guards domain.PathGuardSet,
	dbPaths map[string]bool) (candidates []DiskImportCandidate, checked int, skipped []string) {
	scanned := make([]string, 0, guards.Len())
	for _, guard := range guards.Guards() {
		if ctx.Err() != nil {
			break
		}
		if isUnderAny(guard.Root, scanned) {
			a.Log.Debug("跳过被上游根覆盖的白名单根", "root", guard.Root)
			continue
		}
		scanned = append(scanned, guard.Root)

		disksRoot, ok := a.disksDirOf(guard)
		if !ok {
			skipped = append(skipped, "disk_import_scan: 根 "+guard.Root+" 未配置 disks 子目录，已跳过")
			continue
		}
		if _, statErr := os.Stat(disksRoot); statErr != nil {
			// 根下还没有 disks/（新配的存储）：不是问题，跳过即可。
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
			candidates = append(candidates, a.importCandidate(p, guard.Root, disksRoot, fi, dbPaths))
			return nil
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return candidates, checked, skipped
}

// importCandidate 组装单行候选（读 sidecar、判可导入性、给建议名称与容量）。
func (a *App) importCandidate(p, root, disksRoot string, fi os.FileInfo, dbPaths map[string]bool) DiskImportCandidate {
	c := DiskImportCandidate{
		Path:       p,
		Root:       root,
		Kind:       diskImportKindOf(p, disksRoot),
		SizeBytes:  fi.Size(),
		ModifiedAt: fi.ModTime().UnixMilli(),
		MetaPath:   diskMetaPath(p),
		Registered: dbPaths[pathKeyLocal(p)],
	}
	meta, metaErr := readDiskMetaFile(p)
	switch {
	case metaErr != nil:
		c.MetaError = metaErr.Error()
	case meta != nil:
		c.Meta = meta
	}
	c.SuggestedName = pickImportName(DiskImportItem{}, meta, p)
	c.SuggestedCapacityBytes, c.CapacitySource = pickImportCapacity(DiskImportItem{}, meta, fi.Size())

	// 不可导入的情形按"用户最该先解决哪个"排序：已登记（无需处理）→ 差异盘（不能处理）
	// → 元数据坏了 / 无容量（可以处理）。路径合法性在扫描阶段必然成立（文件就是从白名单
	// 根下走出来的），"路径非法"只可能出现在导入请求里（清单过期 + 手工构造）。
	switch {
	case c.Registered:
		c.SkipReason = DiskImportSkipRegistered
	case c.Kind == DiskImportKindDiff:
		c.SkipReason = DiskImportSkipDiff
	case c.MetaError != "":
		c.SkipReason = DiskImportFailMetaBad
	case c.SuggestedCapacityBytes <= 0:
		c.SkipReason = DiskImportFailCapacity
	default:
		c.Importable = true
	}
	return c
}

// diskImportKindOf 按文件在 disks/ 下的位置分类。
func diskImportKindOf(p, disksRoot string) string {
	rel := strings.ToLower(filepath.ToSlash(strings.TrimPrefix(p, disksRoot)))
	switch {
	case strings.Contains(rel, "/diffs/") || strings.HasPrefix(rel, "/diffs/"):
		return DiskImportKindDiff
	case strings.Contains(rel, "/parents/") || strings.HasPrefix(rel, "/parents/"):
		return DiskImportKindParent
	case strings.Contains(rel, "/standalone/") || strings.HasPrefix(rel, "/standalone/"):
		return DiskImportKindStandalone
	default:
		return DiskImportKindOther
	}
}

// pickImportName 决定存储库名称：请求里填的 → 元数据里的 → 父目录名（原机器上的库 ID，
// 认不出内容但至少唯一）→ 文件名主干。界面会拿到它作为预填值，管理员可改。
func pickImportName(item DiskImportItem, meta *DiskMetaFile, abs string) string {
	if n := truncateName(strings.TrimSpace(item.Name)); n != "" {
		return n
	}
	if meta != nil {
		if n := truncateName(strings.TrimSpace(meta.Name)); n != "" {
			return n
		}
	}
	if d := strings.TrimSpace(filepath.Base(filepath.Dir(abs))); d != "" && d != "." && d != string(filepath.Separator) {
		return truncateName(d)
	}
	return truncateName(strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs)))
}

// pickImportCapacity 决定标称容量与它的可信度（见 DiskImportCapacity*）。
//
// 优先级：请求里显式填的 > sidecar 里的 > 文件大小（估）。
// 前两者是"有人知道真实值"，最后一个是"只能猜"——VHDX 稀疏，猜出来的通常偏小，
// 因此必须把来源一路传到界面，让用户知道哪个数字需要复核。
func pickImportCapacity(item DiskImportItem, meta *DiskMetaFile, fileSize int64) (int64, string) {
	if item.CapacityBytes > 0 {
		return item.CapacityBytes, DiskImportCapacityManual
	}
	if meta != nil && meta.CapacityBytes > 0 {
		return meta.CapacityBytes, DiskImportCapacityMeta
	}
	if fileSize > 0 {
		return fileSize, DiskImportCapacityFile
	}
	return 0, DiskImportCapacityFile
}

// importFileSystem 决定盘的文件系统：元数据里写了且合法就用它，否则按默认客户端系统（NTFS）。
//
// 这个值决定"这块盘能给哪个系统的客户端用"（见 domain.FileSystem），
// 元数据缺失时按 Windows 处理是历史行为，也是最保守的（Linux 客户端能挂 NTFS，反之不行）。
func importFileSystem(meta *DiskMetaFile) domain.FileSystem {
	if meta != nil {
		fs := domain.FileSystem(strings.ToLower(strings.TrimSpace(meta.FileSystem)))
		if fs.Valid() {
			return fs
		}
	}
	return domain.ClientOSDefault.FileSystem()
}

// importVHDType 决定盘的类型：元数据里写了且合法就用它，否则按动态盘。
func importVHDType(meta *DiskMetaFile) domain.VHDType {
	if meta != nil {
		t := domain.VHDType(strings.ToLower(strings.TrimSpace(meta.VHDType)))
		if t.Valid() {
			return t
		}
	}
	return domain.VHDTypeDynamic
}

// truncateName 按字符（不是字节）截断名称，避免把中文切坏。
func truncateName(s string) string {
	r := []rune(s)
	if len(r) <= diskImportNameLimit {
		return s
	}
	return string(r[:diskImportNameLimit])
}

// uniqueRepoName 为导入的库找一个没被占用的名称：原名 → 原名-2 → 原名-3 …
//
// 名称在库里是全局唯一的（repositories.name UNIQUE），而迁移后老名字很可能已经被新机器上
// 的库占用了。自动加序号而不是让这条导入失败：用户想恢复的是"这块盘能用"，
// 名字冲突只是小事，失败重试还得让他手工改名。
func uniqueRepoName(ctx context.Context, tx *store.Store, base string) (string, error) {
	name := truncateName(strings.TrimSpace(base))
	for i := 2; i < 1000; i++ {
		_, err := tx.GetRepositoryByName(ctx, name)
		if err != nil {
			if isNotFound(err) {
				return name, nil
			}
			return "", err
		}
		suffix := "-" + itoa(i)
		name = truncateName(strings.TrimSuffix(base, suffix)) + suffix
	}
	return "", apperr.InvalidParam("name")
}

// itoa 是 strconv.Itoa 的薄封装（本文件只为拼接名称序号用它，避免为一个整数再引一个包）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ---- 导入执行 ----

// ImportDisks 原地接管一批磁盘文件（仅超级管理员调用）。
//
// 每个文件做四件事：校验路径 → 查重（已登记即跳过）→ 建存储库 + 磁盘记录 → 回写 sidecar。
// **绝不触碰磁盘文件本身**（不复制、不移动）：这正是"迁移"需要的语义 ——
// 几十 GB 的母盘拷一次就够了，导入不该再拷一次。
func (a *App) ImportDisks(ctx context.Context, actorID string, in DiskImportInput) (*DiskImportResult, error) {
	if len(in.Items) == 0 {
		return nil, apperr.DiskImportEmpty()
	}
	if len(in.Items) > diskImportMaxItems {
		return nil, apperr.InvalidParam("items")
	}
	owner, err := a.Store.GetUserByID(ctx, strings.TrimSpace(in.OwnerID))
	if err != nil {
		if isNotFound(err) {
			return nil, apperr.DiskImportOwnerInvalid().WithArg("owner_id", in.OwnerID)
		}
		return nil, err
	}
	if !owner.Enabled {
		// 停用用户不能作为归属：他立刻无法登录，库就没人管了。
		return nil, apperr.DiskImportOwnerInvalid().WithArg("owner_id", in.OwnerID)
	}

	guards, err := a.storageGuards(ctx)
	if err != nil {
		return nil, err
	}
	dbPaths, err := a.registeredDiskPaths(ctx)
	if err != nil {
		return nil, err
	}

	res := &DiskImportResult{Imported: []ImportedDisk{}, Skipped: []DiskImportSkip{}, Failed: []DiskImportFailure{}}
	for _, item := range in.Items {
		if ctx.Err() != nil {
			break
		}
		imported, skip, failure := a.importOneDisk(ctx, guards, dbPaths, owner, item)
		switch {
		case imported != nil:
			// 立刻把新登记的路径塞进集合：同一批里重复提交同一路径时，第二条必须走
			// "已登记"而不是撞数据库的唯一约束。
			dbPaths[pathKeyLocal(imported.Path)] = true
			res.Imported = append(res.Imported, *imported)
		case skip != nil:
			res.Skipped = append(res.Skipped, *skip)
		case failure != nil:
			res.Failed = append(res.Failed, *failure)
		}
	}
	a.Log.Info("导入已有磁盘完成",
		"actor", actorID, "owner", owner.ID,
		"imported", len(res.Imported), "skipped", len(res.Skipped), "failed", len(res.Failed))
	return res, nil
}

// importOneDisk 处理一条导入请求，返回三者之一（成功 / 跳过 / 失败）。
func (a *App) importOneDisk(ctx context.Context, guards domain.PathGuardSet, dbPaths map[string]bool,
	owner *domain.User, item DiskImportItem) (imported *ImportedDisk, skip *DiskImportSkip, failure *DiskImportFailure) {
	raw := strings.TrimSpace(item.Path)
	abs := filepath.Clean(raw)
	if abs == "." || !filepath.IsAbs(abs) || !strings.EqualFold(filepath.Ext(abs), ".vhdx") {
		return nil, nil, &DiskImportFailure{Path: raw, Code: DiskImportFailInvalid}
	}
	// 白名单校验与删除孤儿同一套（见 DeleteOrphanFile）：即使请求被手工构造，
	// 也不可能把根外的文件登记进来 —— 否则后续挂载/删除/对账都会在路径解析上失败。
	guard, ok := guards.GuardForPath(abs)
	if !ok {
		return nil, nil, &DiskImportFailure{Path: abs, Code: DiskImportFailInvalid}
	}
	disksRoot, ok := a.disksDirOf(guard)
	if !ok || !domain.UnderRoot(disksRoot, abs) {
		return nil, nil, &DiskImportFailure{Path: abs, Code: DiskImportFailInvalid}
	}
	kind := diskImportKindOf(abs, disksRoot)
	if kind == DiskImportKindDiff {
		return nil, &DiskImportSkip{Path: abs, Code: DiskImportSkipDiff}, nil
	}
	if dbPaths[pathKeyLocal(abs)] {
		// 用户口径："已有的跳过"。这里**重新查**而不是信清单：清单是扫描那一刻的快照，
		// 期间这块盘可能已经被别的任务接管了。
		return nil, &DiskImportSkip{Path: abs, Code: DiskImportSkipRegistered}, nil
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, &DiskImportFailure{Path: abs, Code: DiskImportFailNotFound}
		}
		return nil, nil, &DiskImportFailure{Path: abs, Code: DiskImportFailCreate, Detail: err.Error()}
	}
	if fi.IsDir() {
		return nil, nil, &DiskImportFailure{Path: abs, Code: DiskImportFailInvalid}
	}
	meta, metaErr := readDiskMetaFile(abs)
	if metaErr != nil {
		return nil, nil, &DiskImportFailure{Path: abs, Code: DiskImportFailMetaBad, Detail: metaErr.Error()}
	}

	name := pickImportName(item, meta, abs)
	capacity, capacitySource := pickImportCapacity(item, meta, fi.Size())
	if capacity <= 0 {
		return nil, nil, &DiskImportFailure{Path: abs, Code: DiskImportFailCapacity}
	}

	mode := domain.RepoModeShared
	diskKind := domain.DiskKindParent
	if kind != DiskImportKindParent {
		mode = domain.RepoModeExclusive
		diskKind = domain.DiskKindStandalone
	}
	note := strings.TrimSpace(item.Note)
	if note == "" && meta != nil {
		note = strings.TrimSpace(meta.Note)
	}
	maxDiff := a.raw().Storage.DefaultMaxDiffDisks
	if meta != nil && meta.MaxDiffDisks > 0 {
		maxDiff = meta.MaxDiffDisks
	}
	if maxDiff <= 0 {
		maxDiff = 50
	}
	repo := &domain.Repository{
		Mode:         mode,
		OwnerID:      owner.ID,
		MaxDiffDisks: maxDiff,
		State:        domain.RepoStateActive,
	}
	if mode == domain.RepoModeShared {
		cond := domain.ParentIdle
		repo.ParentCondition = &cond
	}
	repo.Meta.Note = note
	if meta != nil && meta.Source != nil {
		// 分组标签也算"自定义源信息"，跟着元数据一起回来。
		repo.Meta.Group = meta.Source.Group
	}

	disk := &domain.Disk{
		RepoID:     "",
		Kind:       diskKind,
		VHDXPath:   abs,
		SizeBytes:  capacity,
		VHDType:    importVHDType(meta),
		State:      domain.DiskStateReady,
		FileSystem: importFileSystem(meta),
	}
	// 只读补充信息（失败不影响导入）：物理占用用于容量展示，内容指纹让对账不必
	// 把这块"刚接管、还没被动过"的母盘报成"指纹缺失"。
	if physical, pErr := a.Disk.PhysicalSize(abs); pErr == nil && physical > 0 {
		disk.PhysicalBytes = physical
	}
	if fp, fErr := a.Disk.Fingerprint(abs); fErr == nil {
		disk.ContentFingerprint = fp
	} else {
		a.Log.Warn("导入时计算磁盘指纹失败（不影响导入，对账会补算）", "path", abs, "error", fErr.Error())
	}

	err = a.Store.Tx(ctx, func(tx *store.Store) error {
		// 名称是全局唯一的：迁移后老名字可能已被占用，自动加序号而不是让整条导入失败。
		unique, uErr := uniqueRepoName(ctx, tx, name)
		if uErr != nil {
			return uErr
		}
		repo.Name = unique
		if err := tx.CreateRepository(ctx, repo); err != nil {
			return err
		}
		disk.RepoID = repo.ID
		if err := tx.CreateDisk(ctx, disk); err != nil {
			return err
		}
		if mode == domain.RepoModeShared {
			// 共享库必须回填母盘指针：库容量、分配、母盘维护全都靠它定位这块盘
			// （见 store.repoCapacitySQL 的派生口径）。
			pid := disk.ID
			repo.ParentDiskID = &pid
			return tx.UpdateRepository(ctx, repo)
		}
		return nil
	})
	if err != nil {
		a.Log.Warn("导入磁盘失败", "path", abs, "error", err.Error())
		return nil, nil, &DiskImportFailure{Path: abs, Code: DiskImportFailCreate, Detail: err.Error()}
	}

	// 回写 sidecar：让这块盘从此带着自己的名字与来历，下次迁移/换机器还能认出来。
	// source.kind=import 记录"它是在这台机器上被接管的"，同时保留原有的源目录信息。
	src := &DiskMetaSource{Kind: DiskMetaSourceImport, At: time.Now().UnixMilli()}
	if meta != nil && meta.Source != nil {
		src.Dir = meta.Source.Dir
		src.Group = meta.Source.Group
		src.RepoID = meta.Source.RepoID
		src.Host = meta.Source.Host
	}
	writeDiskMetaForDisk(a.Log, repo, disk, src)

	a.Log.Info("已原地接管磁盘",
		"path", abs, "repo_id", repo.ID, "name", repo.Name, "mode", string(mode),
		"capacity_bytes", capacity, "capacity_source", capacitySource)
	return &ImportedDisk{
		Path:              abs,
		RepoID:            repo.ID,
		Name:              repo.Name,
		Mode:              string(mode),
		Kind:              string(diskKind),
		CapacityBytes:     capacity,
		CapacityEstimated: capacitySource == DiskImportCapacityFile,
	}, nil, nil
}

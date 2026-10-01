package app

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// 目录浏览/统计的限额与时间预算（见 docs/implementation.md 6.3「目录浏览」）。
const (
	// fsStatMaxFiles 单次递归统计的文件数上限；超过即截断（返回已统计值 + truncated）。
	fsStatMaxFiles = 200000
	// fsStatTimeout 单次递归统计的时间预算；超过即截断。
	fsStatTimeout = 10 * time.Second
)

// errSourceNotAllowed 源目录不在允许的白名单（storage.source_roots）之内。
func errSourceNotAllowed(p string) *apperr.Error {
	return apperr.New("source_dir.not_allowed", http.StatusForbidden).WithArg("path", p)
}

// FSRoot 是一个可浏览的源目录根。
type FSRoot struct {
	// Name 展示名（路径末段；盘根回退为盘符）。
	Name string
	// Path 规范化后的绝对路径。
	Path string
	// Exists 该根当前是否真实存在（首次建盘前尚未创建属正常状态）。
	Exists bool
}

// FSEntry 是浏览结果中的一条**子目录**（不列文件）。
type FSEntry struct {
	Name string
	Path string
	// HasChildren 该子目录下是否还有子目录。
	HasChildren bool
}

// FSBrowseResult 是目录浏览结果。
type FSBrowseResult struct {
	Path string
	// Parent 上一级（仅在仍处于根白名单之下时给出）。
	Parent string
	Root   string
	// Exists 目标路径是否存在；不存在时 Items 为空且不视为错误。
	Exists bool
	Items  []FSEntry
}

// FSStatResult 是目录统计结果。
type FSStatResult struct {
	Path string
	// Exists 目标路径是否存在；不存在时计数均为 0 且不视为错误。
	Exists     bool
	FileCount  int
	TotalBytes int64
	// Truncated 命中文件数上限或时间预算，统计值为**已统计**的部分。
	Truncated bool
}

// FSService 提供服务端本地目录的浏览与统计（受 source roots 白名单约束）。
type FSService struct{ Deps }

// sourceRoots 返回当前允许被浏览/作为源目录的根白名单（已规范化、去重）。
//
// 配置 storage.source_roots 非空时以它为准（相对路径按配置文件目录解析）；
// 留空则回退为当前生效的存储根（storageSelection），保证默认行为与历史一致。
func (d Deps) sourceRoots(ctx context.Context) ([]string, error) {
	configured := d.raw().Storage.SourceRoots
	out := make([]string, 0, len(configured))
	seen := make(map[string]bool, len(configured))
	base := d.configDir()
	for _, r := range configured {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			if base != "" {
				r = filepath.Join(base, r)
			} else if abs, err := filepath.Abs(r); err == nil {
				r = abs
			}
		}
		r = filepath.Clean(r)
		key := strings.ToLower(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	if len(out) > 0 {
		return out, nil
	}
	return d.storageRoots(ctx)
}

// resolveSourcePath 校验并规范化一个"源目录"路径：
// 必须绝对、非 UNC、且落在某个 source root 之下（大小写不敏感、按目录边界比较）。
// 返回规范化路径与其所属的源目录根。
func (d Deps) resolveSourcePath(ctx context.Context, raw, field string) (cleaned, root string, err error) {
	p := strings.TrimSpace(raw)
	if p == "" || !filepath.IsAbs(p) || isUNCPath(p) {
		return "", "", apperr.InvalidParam(field)
	}
	p = filepath.Clean(p)
	roots, err := d.sourceRoots(ctx)
	if err != nil {
		return "", "", err
	}
	for _, r := range roots {
		if domain.UnderRoot(r, p) {
			return p, r, nil
		}
	}
	return "", "", errSourceNotAllowed(p)
}

// validateSourceDir 校验并规范化仓库的 source_dir（拒绝白名单之外的目录）。
func (d Deps) validateSourceDir(ctx context.Context, raw string) (string, error) {
	cleaned, _, err := d.resolveSourcePath(ctx, raw, "source_dir")
	if err != nil {
		return "", err
	}
	return cleaned, nil
}

// Roots 列出可浏览的源目录根。
//
// 根当前不存在（尚未建盘）是合法状态：不报错、不跳过，只把 Exists 置为 false。
func (s *FSService) Roots(ctx context.Context) ([]FSRoot, error) {
	roots, err := s.sourceRoots(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]FSRoot, 0, len(roots))
	for _, r := range roots {
		out = append(out, FSRoot{Name: rootDisplayName(r), Path: r, Exists: pathExists(r)})
	}
	return out, nil
}

// Browse 列出指定目录下的**子目录**（不跟随符号链接，不列文件），按名称排序。
//
// 路径不存在（首次建盘前尚未创建）是合法状态：返回 Exists=false、Items 为空且 **不报错**。
func (s *FSService) Browse(ctx context.Context, rawPath string) (*FSBrowseResult, error) {
	if strings.TrimSpace(rawPath) == "" {
		// path 必填：前端先调 roots，再对具体路径 browse。
		return nil, apperr.InvalidParam("path")
	}
	cleaned, root, err := s.resolveSourcePath(ctx, rawPath, "path")
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			return browseResult(cleaned, root, false, []FSEntry{}), nil
		}
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "stat").WithCause(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, apperr.InvalidParam("path")
	}

	entries, err := os.ReadDir(cleaned)
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "read").WithCause(err)
	}
	items := make([]FSEntry, 0, len(entries))
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			// 只列真实子目录；符号链接一律不跟随也不列出。
			continue
		}
		full := filepath.Join(cleaned, e.Name())
		items = append(items, FSEntry{
			Name:        e.Name(),
			Path:        full,
			HasChildren: dirHasSubdir(full),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	return browseResult(cleaned, root, true, items), nil
}

// browseResult 组装浏览结果：parent 仅在仍处于根白名单之下时给出。
func browseResult(cleaned, root string, exists bool, items []FSEntry) *FSBrowseResult {
	res := &FSBrowseResult{Path: cleaned, Root: root, Exists: exists, Items: items}
	if parent := filepath.Dir(cleaned); parent != cleaned && domain.UnderRoot(root, parent) {
		res.Parent = parent
	}
	return res
}

// pathExists 判断路径是否存在（不跟随符号链接；权限等其它错误一律按"存在"处理，交由后续步骤报错）。
func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil || !os.IsNotExist(err)
}

// Stat 递归统计指定目录下的常规文件数与总字节数（跳过符号链接，不跟随）。
//
// 为免超大目录把请求拖死，设有文件数上限与时间预算：超限返回已统计值并置 Truncated=true。
// 路径不存在（首次建盘前尚未创建）是合法状态：返回 Exists=false、计数为 0 且 **不报错**。
func (s *FSService) Stat(ctx context.Context, rawPath string) (*FSStatResult, error) {
	if strings.TrimSpace(rawPath) == "" {
		return nil, apperr.InvalidParam("path")
	}
	cleaned, _, err := s.resolveSourcePath(ctx, rawPath, "path")
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(cleaned)
	if err != nil {
		if os.IsNotExist(err) {
			return &FSStatResult{Path: cleaned, Exists: false}, nil
		}
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "stat").WithCause(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, apperr.InvalidParam("path")
	}

	res := &FSStatResult{Path: cleaned, Exists: true}
	deadline := time.Now().Add(fsStatTimeout)
	walkErr := filepath.WalkDir(cleaned, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if p == cleaned {
				return walkErr
			}
			return nil // 跳过不可读子树，尽量给出部分结果
		}
		if res.FileCount >= fsStatMaxFiles || time.Now().After(deadline) {
			res.Truncated = true
			return filepath.SkipAll
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, iErr := d.Info(); iErr == nil {
				res.FileCount++
				res.TotalBytes += fi.Size()
			}
		}
		return nil
	})
	if walkErr != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "walk").WithCause(walkErr)
	}
	return res, nil
}

// dirHasSubdir 判断目录下是否至少有一个真实子目录（用于浏览结果的 has_children）。
func dirHasSubdir(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	for {
		entries, err := f.ReadDir(64)
		for _, e := range entries {
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if e.IsDir() {
				return true
			}
		}
		if err != nil {
			return false
		}
	}
}

// rootDisplayName 由路径推导展示名：取末段；盘根（如 D:\）回退为盘符。
func rootDisplayName(p string) string {
	base := strings.Trim(filepath.Base(filepath.Clean(p)), `\/`)
	if base == "" || base == "." || base == string(filepath.Separator) {
		if v := filepath.VolumeName(p); v != "" {
			return v
		}
		return p
	}
	return base
}

package domain

import (
	"context"
	"path/filepath"
	"strings"

	"vault/internal/apperr"
)

// VolumeSpace 描述某个路径所在卷的空间与标识。
type VolumeSpace struct {
	// Name 卷标识（规范化、小写，如 "d:"）。挂载点这类非盘符卷可能不精确（见 PathGuardSet 说明）。
	Name string
	// FileSystem 卷的文件系统名（如 NTFS / ReFS）。
	FileSystem string
	// FreeBytes / TotalBytes 卷可用空间 / 总容量。
	FreeBytes  int64
	TotalBytes int64
}

// VolumeSpaceProvider 提供"路径 → 所在卷空间"的查询能力，由 platform/volume.Manager 实现。
type VolumeSpaceProvider interface {
	SpaceUsageOf(ctx context.Context, path string) (*VolumeSpace, error)
}

// VolumeUsage 是按卷去重后的用量快照。
type VolumeUsage struct {
	// Name 卷标识（规范化、小写）。
	Name string
	// FileSystem 卷文件系统。
	FileSystem string
	// FreeBytes / TotalBytes 卷可用空间 / 总容量。
	FreeBytes  int64
	TotalBytes int64
	// Roots 落在该卷上的全部白名单根（按配置顺序）。
	Roots []string
}

// VolumeID 返回某个绝对路径所在卷的标识（规范化、小写）。
//
// 说明：这里用 filepath.VolumeName 取盘符/卷名，对"盘符卷"是准确的；
// 对挂载点（把一个卷挂到某个空目录）不精确——filepath.VolumeName 返回的是承载挂载点
// 的那块盘的盘符，而不是被挂载卷的标识。这是已知取舍，仅影响极端部署形态下的去重与锁粒度。
func VolumeID(abs string) string {
	return NormalizeVolumeName(filepath.VolumeName(filepath.Clean(abs)), abs)
}

// NormalizeVolumeName 把脚本返回的盘符或 filepath.VolumeName 的结果归一为小写卷标识。
// driveLetter 为空时回退用 abs 的 VolumeName。
func NormalizeVolumeName(driveLetter, abs string) string {
	v := strings.TrimSpace(driveLetter)
	if v == "" {
		v = filepath.VolumeName(filepath.Clean(abs))
	}
	v = strings.TrimSpace(v)
	// 单字母盘符补冒号，使 "D" 与 "D:" 归一。
	if len(v) == 1 {
		v += ":"
	}
	return strings.ToLower(strings.TrimRight(v, `\/`))
}

// PathGuardSet 是多根路径守卫：为每个白名单根维护一个 PathGuard，并负责"选根"与按卷统计。
//
// 与单根 PathGuard 的关系：PathGuard 的单根语义保持不变，本类型在其之上做多根编排。
type PathGuardSet struct {
	guards []PathGuard
	roots  []string
}

// NewPathGuardSet 为每个根构造守卫；roots 会去空白、Clean、去重（Windows 大小写不敏感）。
func NewPathGuardSet(roots []string) PathGuardSet {
	out := make([]PathGuard, 0, len(roots))
	names := make([]string, 0, len(roots))
	seen := make(map[string]bool, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		key := strings.ToLower(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		names = append(names, r)
		out = append(out, NewPathGuard(r))
	}
	return PathGuardSet{guards: out, roots: names}
}

// Len 返回根个数。
func (s PathGuardSet) Len() int { return len(s.guards) }

// Empty 表示没有任何根。
func (s PathGuardSet) Empty() bool { return len(s.guards) == 0 }

// Roots 返回全部根（规范化、去重、按配置顺序）的副本。
func (s PathGuardSet) Roots() []string {
	return append([]string(nil), s.roots...)
}

// Guards 返回全部根守卫（按配置顺序）。
func (s PathGuardSet) Guards() []PathGuard {
	return append([]PathGuard(nil), s.guards...)
}

// First 返回第一个根守卫；无根时返回 false。
func (s PathGuardSet) First() (PathGuard, bool) {
	if len(s.guards) == 0 {
		return PathGuard{}, false
	}
	return s.guards[0], true
}

// Resolve 按第一个根解析相对路径（用于读语义：staging/trash/orphan 的默认位置等）。
func (s PathGuardSet) Resolve(rel string) (string, error) {
	if len(s.guards) == 0 {
		return "", apperr.InvalidParam("roots")
	}
	return s.guards[0].Resolve(rel)
}

// GuardForPath 返回给定绝对路径所属的根守卫。
//
// 匹配规则：按配置顺序，返回第一个"包含"该路径的根；包含判断大小写不敏感，
// 且按父目录边界比较（`D:\a` 不匹配 `D:\ab`）。
// 若配置了互为父子的根（如 D:\Vault 与 D:\Vault\extra），两者必在同一卷，
// 因此返回哪一个都不影响"同卷"这一保证。
func (s PathGuardSet) GuardForPath(abs string) (PathGuard, bool) {
	p := filepath.Clean(abs)
	for _, g := range s.guards {
		if underRootFold(g.Root, p) {
			return g, true
		}
	}
	return PathGuard{}, false
}

// Pick 选择用来创建新磁盘的根：在可用空间足够的根里，选所在卷可用空间最大的那个；
// 同卷并列（可用空间相同）时按配置顺序取第一个。
//
// 都不足够时返回 storage.insufficient_space（与 ensureVolumeFree 语义一致）。
// provider 为 nil（非 Windows/未装配卷管理器）时退化为第一个根，不做空间判断。
func (s PathGuardSet) Pick(ctx context.Context, provider VolumeSpaceProvider, need int64) (PathGuard, error) {
	if len(s.guards) == 0 {
		return PathGuard{}, apperr.InvalidParam("roots")
	}
	if provider == nil {
		return s.guards[0], nil
	}
	bestIdx := -1
	var bestFree int64
	var lastErr error
	for i, g := range s.guards {
		space, err := provider.SpaceUsageOf(ctx, g.Root)
		if err != nil {
			// 查询失败不等于空间不足：记下真实错误，全部根都失败时把它抛出去，
			// 避免把"卷空间查询不可用"误报成"空间不足"。
			lastErr = err
			continue
		}
		if space.FreeBytes < need {
			continue
		}
		// 严格大于 → 同卷并列时保留配置顺序更靠前的根。
		if bestIdx < 0 || space.FreeBytes > bestFree {
			bestIdx, bestFree = i, space.FreeBytes
		}
	}
	if bestIdx < 0 {
		if lastErr != nil {
			return PathGuard{}, lastErr
		}
		return PathGuard{}, apperr.New("storage.insufficient_space", 507).WithArg("need_bytes", need)
	}
	return s.guards[bestIdx], nil
}

// Volumes 返回按卷去重后的用量列表。
//
// 去重键 = 卷标识（规范化、小写）。同一卷上的多个根会合并为一条，Roots 列出全部根。
// 某个根的查询失败会被跳过，不影响其余根（避免单点故障让整体统计为空）。
func (s PathGuardSet) Volumes(ctx context.Context, provider VolumeSpaceProvider) ([]VolumeUsage, error) {
	if provider == nil || len(s.guards) == 0 {
		return nil, nil
	}
	out := make([]VolumeUsage, 0, len(s.guards))
	idx := make(map[string]int, len(s.guards))
	for _, g := range s.guards {
		space, err := provider.SpaceUsageOf(ctx, g.Root)
		if err != nil {
			continue
		}
		name := space.Name
		if name == "" {
			name = VolumeID(g.Root)
		}
		if i, ok := idx[name]; ok {
			out[i].Roots = append(out[i].Roots, g.Root)
			continue
		}
		idx[name] = len(out)
		out = append(out, VolumeUsage{
			Name:       name,
			FileSystem: space.FileSystem,
			FreeBytes:  space.FreeBytes,
			TotalBytes: space.TotalBytes,
			Roots:      []string{g.Root},
		})
	}
	return out, nil
}

// UnderRoot 判断 p 是否位于 root 之下（含相等）；大小写不敏感、按父目录边界比较。
func UnderRoot(root, p string) bool { return underRootFold(root, p) }

// underRootFold 判断 p 是否位于 root 之下（含相等）；大小写不敏感、按父目录边界比较。
func underRootFold(root, p string) bool {
	if strings.EqualFold(root, p) {
		return true
	}
	if len(p) <= len(root) || !strings.EqualFold(p[:len(root)], root) {
		return false
	}
	if strings.HasSuffix(root, string(filepath.Separator)) || strings.HasSuffix(root, "/") {
		return true
	}
	return isSepByte(p[len(root)])
}

func isSepByte(b byte) bool { return b == '\\' || b == '/' }

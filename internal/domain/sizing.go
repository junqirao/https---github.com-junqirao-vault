package domain

import (
	"path/filepath"
	"strings"
)

// Sizing 描述 VHDX 容量计算参数（见 docs/implementation.md 5.2）。
type Sizing struct {
	// GranularityBytes 容量向上取整粒度。
	GranularityBytes int64
	// ReservePermille 预留千分比（文件系统元数据 + 碎片）。
	ReservePermille int64
	// ReserveFloorBytes 预留下限，避免小目录预留过少。
	ReserveFloorBytes int64
	// MinSizeBytes VirtDisk 的硬下限（3MB）。
	MinSizeBytes int64
	// EmptyDirFallbackBytes 空目录且用户未填尺寸时的兜底值。
	EmptyDirFallbackBytes int64
}

// DefaultSizing 返回与文档一致的默认参数。
//
// 参数来源：docs/implementation.md 5.2
//   - **1MB 粒度**
//   - 10% 预留，且不低于 16MB
//   - 硬下限 3MB
//   - 空目录兜底 64MB
//
// ⚠️ 为什么从「1GB 粒度 + 512MB 预留下限 + 1GB 兜底」改成 MB 级（真实反馈：
// "我一点空间都没用就占了 1G"）：VHDX 是动态盘，写入前几乎不占物理空间，所以标称容量
// 只是"上限"；但分配时是**按母盘标称容量预留用量**的，于是空目录/小目录推导出的 1GB
// 标称容量会让每个分配一上来就占掉 1GB 配额（校正要等差异盘建完才发生，期间一直挂着）。
// 按 MB 取整既不会写不下（动态盘可增长、且仍保留 10% 预留），也不会凭空吃掉配额。
func DefaultSizing() Sizing {
	return Sizing{
		GranularityBytes:      1 << 20,  // 1 MiB
		ReservePermille:       100,      // 10%
		ReserveFloorBytes:     16 << 20, // 16 MiB
		MinSizeBytes:          3 << 20,  // 3 MiB
		EmptyDirFallbackBytes: 64 << 20, // 64 MiB
	}
}

// CalculateVHDXSize 计算创建 VHDX 时应申请的容量。
//
// 规则：
//
//	dirBytes   源目录实际占用字节数（0 表示空目录/不基于目录创建）
//	requested  用户在表单中填写的容量（0 表示未填）
//
//	基础值 = max(requested, ceil(dirBytes 到粒度) + 预留)
//	空目录且未填 → EmptyDirFallbackBytes
//	最终值向上取整到粒度，并不低于 MinSizeBytes
func (s Sizing) CalculateVHDXSize(dirBytes, requested int64) int64 {
	if s.GranularityBytes <= 0 {
		s = DefaultSizing()
	}

	base := requested
	if base < 0 {
		base = 0
	}

	if dirBytes > 0 {
		withReserve := roundUp(dirBytes, s.GranularityBytes) + s.reserveFor(dirBytes)
		if withReserve > base {
			base = withReserve
		}
	} else if base == 0 {
		base = s.EmptyDirFallbackBytes
	}

	if base < s.MinSizeBytes {
		base = s.MinSizeBytes
	}
	return roundUp(base, s.GranularityBytes)
}

func (s Sizing) reserveFor(dirBytes int64) int64 {
	r := dirBytes * s.ReservePermille / 1000
	if r < s.ReserveFloorBytes {
		return s.ReserveFloorBytes
	}
	return r
}

func roundUp(n, unit int64) int64 {
	if unit <= 1 {
		return n
	}
	if r := n % unit; r != 0 {
		return n + (unit - r)
	}
	return n
}

// ManualExportEstimate 估算"把差异盘导出为独立库"的额外空间与目标容量。
//
// 注意：NTFS 无块克隆（见 docs/implementation.md 5.9.2），
// 导出必然是全量物理拷贝，因此额外空间约等于源文件大小。
func ManualExportEstimate(srcFileSize, srcLogicalSize int64, s Sizing) (requiredVolumeFree, targetSize int64) {
	if srcFileSize < 0 {
		srcFileSize = 0
	}
	// 卷需要容纳：新文件全量拷贝 + 10% 余量。
	requiredVolumeFree = srcFileSize + srcFileSize/10
	targetSize = s.CalculateVHDXSize(0, srcLogicalSize)
	return requiredVolumeFree, targetSize
}

// PathGuard 校验路径是否落在白名单根目录内（防路径穿越）。
//
// 约定：
//   - root 必须已经是绝对路径且经过 EvalSymlinks 规范化；
//   - candidate 允许是相对路径，会基于 root 解析；
//   - 拒绝 UNC、盘符绝对路径、以及 `..` 逃逸。
type PathGuard struct {
	// Root 白名单根（绝对路径，已规范化）。
	Root string
}

// NewPathGuard 构造 PathGuard，root 会被 Clean 处理。
func NewPathGuard(root string) PathGuard {
	return PathGuard{Root: filepath.Clean(root)}
}

// Resolve 将相对路径解析为白名单内的绝对路径；越界时返回错误。
func (g PathGuard) Resolve(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return g.Root, nil
	}
	if isAbsoluteLike(rel) {
		return "", &PathEscapeError{Path: rel}
	}
	target := filepath.Clean(filepath.Join(g.Root, rel))
	if !g.Contains(target) {
		return "", &PathEscapeError{Path: rel}
	}
	return target, nil
}

// Contains 判断绝对路径是否位于白名单根内（含根本身）。
func (g PathGuard) Contains(abs string) bool {
	p := filepath.Clean(abs)
	if p == g.Root {
		return true
	}
	// 补分隔符，避免 "/data/vault-evil" 被误判为在 "/data/vault" 内。
	return strings.HasPrefix(p, g.Root+string(filepath.Separator))
}

// isAbsoluteLike 识别绝对路径与 UNC，避免候选值绕过 Join 语义。
func isAbsoluteLike(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

// PathEscapeError 表示路径越界。
type PathEscapeError struct{ Path string }

func (e *PathEscapeError) Error() string { return "路径越界: " + e.Path }

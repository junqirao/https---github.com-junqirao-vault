package app

import (
	"context"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// PoolService 暴露"存储池（LVM thin pool）/ 缓存设备"的平台管理能力。
//
// 能力差异：Linux 有真实实现（建 VG/thin pool、挂 dm-cache、枚举块设备）；
// Windows 后端整体返回 platform.ErrUnsupported，本层统一翻译为 platform.unsupported，
// 由管理端据此隐藏入口，而不是把裸错误暴露给用户。
//
// ⚠️ 缓存本体由 LVM 原生 dm-cache 承担，本服务**不实现任何自定义缓存逻辑**，
// 只负责"把选定的块设备交给 LVM"。
type PoolService struct{ Deps }

// InitializePoolInput 是一次存储池初始化的输入（幂等：已存在的一律跳过）。
type InitializePoolInput struct {
	// VG / ThinPool 目标位置；留空时取配置 platform.lvm.vg / thin_pool。
	VG       string
	ThinPool string
	// HDDDevices 组成 VG 的块设备全路径（VG 已存在时忽略）。
	HDDDevices []string
	// ChunkSize / MetadataSize 建池参数；留空时取配置值。
	ChunkSize    string
	MetadataSize string
	// CacheDevices 用作 dm-cache 的块设备全路径（空表示不加缓存）。
	CacheDevices []string
	// CacheChunkSize 缓存块大小；留空时由后端取默认。
	CacheChunkSize string
	// CachePolicy 缓存策略（smq / mq 等）；留空时由后端取默认。
	CachePolicy string
	// CacheMode 缓存模式：writethrough（默认、安全）| writeback。
	CacheMode string
}

// PoolStatus 返回存储池与缓存的现状（只读展示）。
func (s *PoolService) Status(ctx context.Context) (*platform.PoolStatus, error) {
	if s.Platform == nil {
		return nil, apperr.PlatformUnsupported()
	}
	st, err := s.Platform.Status(ctx)
	if err != nil {
		return nil, unsupportedAsApperr(err)
	}
	return st, nil
}

// ListBlockDevices 枚举可选的块设备（供"缓存设备选择器"使用）。
func (s *PoolService) ListBlockDevices(ctx context.Context) ([]platform.BlockDevice, error) {
	if s.Platform == nil {
		return nil, apperr.PlatformUnsupported()
	}
	devs, err := s.Platform.ListBlockDevices(ctx)
	if err != nil {
		return nil, unsupportedAsApperr(err)
	}
	return devs, nil
}

// InitializePool 幂等地初始化存储池（含可选的缓存设备），返回初始化后的现状。
func (s *PoolService) InitializePool(ctx context.Context, in InitializePoolInput) (*platform.PoolStatus, error) {
	if s.Platform == nil {
		return nil, apperr.PlatformUnsupported()
	}
	spec := platform.PoolSpec{
		VG:           strings.TrimSpace(in.VG),
		ThinPool:     strings.TrimSpace(in.ThinPool),
		HDDDevices:   trimAll(in.HDDDevices),
		ChunkSize:    strings.TrimSpace(in.ChunkSize),
		MetadataSize: strings.TrimSpace(in.MetadataSize),
	}
	if cache := trimAll(in.CacheDevices); len(cache) > 0 {
		spec.Cache = &platform.CacheSpec{
			Devices:   cache,
			ChunkSize: strings.TrimSpace(in.CacheChunkSize),
			Policy:    strings.TrimSpace(in.CachePolicy),
			Mode:      strings.TrimSpace(in.CacheMode),
		}
	}
	if err := s.Platform.InitializePool(ctx, spec); err != nil {
		return nil, unsupportedAsApperr(err)
	}
	return s.Status(ctx)
}

// unsupportedAsApperr 把"平台不支持"翻译为业务错误码，其余错误原样返回。
func unsupportedAsApperr(err error) error {
	if platform.IsUnsupported(err) {
		return apperr.PlatformUnsupported()
	}
	return err
}

// trimAll 去除切片中每项的空白并丢弃空项（nil 输入返回 nil）。
func trimAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

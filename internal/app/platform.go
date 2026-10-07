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
	// SizeBytes thin pool 的**数据容量**（字节）。
	//
	// thin pool 必须给容量，否则 lvcreate 只会报 "No command with matching syntax
	// recognised"（真实反馈）。<=0 表示占满 VG 当前剩余空间。
	SizeBytes int64
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

// PoolCatalog 是"多存储池"的读视图。
type PoolCatalog struct {
	// Items 现有存储池（每个 VG 的 thin pool 一项），默认池在最前。
	Items []platform.PoolStatus
	// VolumeGroups 现有卷组（供"在已有卷组里新建池 / 新建卷组"选择）。
	VolumeGroups []platform.VolumeGroup
	// DefaultVG / DefaultThinPool 后端配置的默认池（前端预选用）。
	DefaultVG       string
	DefaultThinPool string
}

// ListPools 列出全部存储池与卷组。
//
// 平台不支持（Windows：没有池概念）时返回**空的目录**而不是错误：
// 前端据此把"存储池"整块 UI 隐藏掉，不需要为此写分支。
func (s *PoolService) ListPools(ctx context.Context) (*PoolCatalog, error) {
	out := &PoolCatalog{}
	if s.Platform == nil {
		return out, nil // 平台缺失等价于"没有池"，按空目录处理
	}
	if def, ok := s.Platform.(interface {
		DefaultPool() (string, string)
	}); ok {
		out.DefaultVG, out.DefaultThinPool = def.DefaultPool()
	}
	cat, ok := s.Platform.(platform.PoolCatalog)
	if !ok {
		return out, nil
	}
	items, err := cat.ListPools(ctx)
	if err != nil {
		return nil, unsupportedAsApperr(err)
	}
	groups, err := cat.ListVolumeGroups(ctx)
	if err != nil {
		return nil, unsupportedAsApperr(err)
	}
	out.Items, out.VolumeGroups = items, groups
	return out, nil
}

// PoolStatusOf 查询某个池的现状（供"创建存储时选中的池是否存在/有多少余量"校验）。
func (s *PoolService) PoolStatusOf(ctx context.Context, vg, thinPool string) (*platform.PoolStatus, error) {
	if s.Platform == nil {
		return nil, apperr.PlatformUnsupported()
	}
	cat, ok := s.Platform.(platform.PoolCatalog)
	if !ok {
		return nil, apperr.PlatformUnsupported()
	}
	st, err := cat.PoolStatusOf(ctx, vg, thinPool)
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

// EstimatePoolMaxBytes 估算"用这些设备新建卷组后能建多大的 thin pool"（字节）。
//
// 前端在"新建卷组"这条路径上要用它：卷组还没建，存储目录里没有 PoolMaxBytes 可用，
// 而容量口径必须与真正建池时的那次校验一致（否则"自动填好的容量"一提交就超限）。
// 平台不支持（Windows 没有池概念）时返回 platform.unsupported，
// 前端据此退回"不自动填、交给服务端校验"。
func (s *PoolService) EstimatePoolMaxBytes(ctx context.Context, devices []string, metaSize string) (int64, error) {
	if s.Platform == nil {
		return 0, apperr.PlatformUnsupported()
	}
	est, ok := s.Platform.(platform.PoolEstimator)
	if !ok {
		return 0, apperr.PlatformUnsupported()
	}
	max, err := est.EstimatePoolMaxBytes(ctx, trimAll(devices), strings.TrimSpace(metaSize))
	if err != nil {
		return 0, unsupportedAsApperr(err)
	}
	return max, nil
}

// ReleaseBlockDevice 把一块盘清成"可被选作卷组设备"的干净状态。
//
// 动作是**破坏性**的（卸载/关 swap/抹签名/移出卷组），因此只由管理端在用户显式确认后调用。
// 过程性失败逐条记在返回值里（不报错），让前端能看到卡在哪一步。
// 平台不支持（Windows 没有 LVM 概念）时返回 platform.unsupported。
func (s *PoolService) ReleaseBlockDevice(ctx context.Context, path string) (*platform.DeviceReleaseReport, error) {
	if s.Platform == nil {
		return nil, apperr.PlatformUnsupported()
	}
	rel, ok := s.Platform.(platform.DeviceReleaser)
	if !ok {
		return nil, apperr.PlatformUnsupported()
	}
	rep, err := rel.ReleaseDevice(ctx, strings.TrimSpace(path))
	if err != nil {
		return nil, unsupportedAsApperr(err)
	}
	return rep, nil
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
		SizeBytes:    in.SizeBytes,
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
	// 返回**被创建的那个池**（而默认池）：前端多池场景下要靠响应里的 key 作为 pool_ref
	// 去绑定刚建的池，若这里回到默认池就会把存储建错地方。
	if st, ok := s.poolStatusAfterInit(ctx, spec); ok {
		return st, nil
	}
	return s.Status(ctx)
}

// poolStatusAfterInit 读取刚初始化的池的现状；读不到时返回 ok=false，由调用方回退默认池。
//
// 读不到并不算失败：池已经建好了，这里只是取状态，不该把"建池成功"变成错误。
func (s *PoolService) poolStatusAfterInit(ctx context.Context, spec platform.PoolSpec) (*platform.PoolStatus, bool) {
	cat, ok := s.Platform.(platform.PoolCatalog)
	if !ok {
		return nil, false
	}
	vg, pool := spec.VG, spec.ThinPool
	if vg == "" || pool == "" {
		// 目标名留空表示"用配置里的默认池"，此时才能拿默认名来补全。
		def, ok := s.Platform.(interface{ DefaultPool() (string, string) })
		if !ok {
			return nil, false
		}
		dvg, dpool := def.DefaultPool()
		if vg == "" {
			vg = dvg
		}
		if pool == "" {
			pool = dpool
		}
	}
	if vg == "" || pool == "" {
		return nil, false
	}
	st, err := cat.PoolStatusOf(ctx, vg, pool)
	if err != nil || st == nil {
		return nil, false
	}
	return st, true
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

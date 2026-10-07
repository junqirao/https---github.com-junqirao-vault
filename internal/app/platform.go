package app

import (
	"context"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
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

// DeletePoolInput 是"删除存储池"的入参（管理端"存储池管理"面板）。
type DeletePoolInput struct {
	// VG 卷组名；ThinPool 留空表示只删卷组（此时 RemoveVolumeGroup 必须为 true）。
	VG       string
	ThinPool string
	// RemoveVolumeGroup 连卷组一起删除（卷组里还有别的逻辑卷时被拒）。
	RemoveVolumeGroup bool
	// ReleaseDevices 删掉卷组后把物理卷标签清回"可选"状态，磁盘可再次被选作卷组设备。
	ReleaseDevices bool
}

// DeletePool 删除一个存储池（可选连带删除卷组），是"存储池管理"面板唯一的写操作。
//
// ⚠️ 破坏性：池里的 thin 卷就是各存储的底层卷、母盘与差异盘。三道防线缺一不可：
//
//  1. 能力探测：只有实现了 platform.PoolAdmin 的平台（Linux）能删，其余返回 platform.unsupported，
//     前端据此不显示入口（Windows 没有"池"这个概念）；
//  2. 默认池：先按配置拦下（platform.lvm.vg/thin_pool）——它是所有建库/建存储的落脚点，
//     删掉服务随即不可用；"这是默认池，要删先改配置"比"删完才发现"好懂得多；
//  3. 数据库引用：该池上还挂着存储（storage_volumes.pool_ref，或卷所在的 VG 就是它）时拒绝，
//     并带上"还剩几个"。平台层的"池里还有 thin 卷"是最后一道——它连绕过数据库直接建的卷也能挡住。
func (s *PoolService) DeletePool(ctx context.Context, in DeletePoolInput) (*platform.PoolDeleteReport, error) {
	if s.Platform == nil {
		return nil, apperr.PlatformUnsupported()
	}
	admin, ok := s.Platform.(platform.PoolAdmin)
	if !ok {
		return nil, apperr.PlatformUnsupported()
	}
	vg, pool := strings.TrimSpace(in.VG), strings.TrimSpace(in.ThinPool)
	if vg == "" {
		return nil, apperr.InvalidParam("vg")
	}
	if pool == "" && !in.RemoveVolumeGroup {
		// 只有卷组名、又不让删卷组：没有可执行的动作，拒绝比"静默成功"诚实。
		return nil, apperr.InvalidParam("thin_pool")
	}
	if def, ok := s.Platform.(interface {
		DefaultPool() (string, string)
	}); ok {
		if dvg, dpool := def.DefaultPool(); dvg == vg && (pool == "" || pool == dpool) {
			return nil, apperr.PoolProtected("default_pool").
				WithArg("vg", vg).WithArg("thin_pool", pool)
		}
	}
	if err := s.assertPoolUnreferenced(ctx, vg, pool); err != nil {
		return nil, err
	}
	rep, err := admin.DeletePool(ctx, vg, pool, platform.PoolDeleteOptions{
		RemoveVolumeGroup: in.RemoveVolumeGroup,
		ReleaseDevices:    in.ReleaseDevices,
	})
	if err != nil {
		return nil, unsupportedAsApperr(err)
	}
	return rep, nil
}

// assertPoolUnreferenced 拒绝"池上还有存储"的删除。
//
// 为什么在应用层再查一遍库，而不是只信平台层的"池里还有 thin 卷"：
// 平台层只能报出"还剩 N 个卷（名字是 UUID 串）"，而用户真正能操作的对象是**存储**；
// 先查库才能给出"该池上还有 2 个存储，请先删掉它们"这种可行动的提示。
// 磁盘不必单独查：盘都住在存储目录下，池里还有盘时平台层的 thin 卷判定会兜住。
// 判据本身见 volumeOnPool（"哪个卷会被这次删除带走"是这里唯一的难点）。
func (s *PoolService) assertPoolUnreferenced(ctx context.Context, vg, thinPool string) error {
	if s.Store == nil {
		return nil
	}
	vols, err := s.Store.ListStorageVolumes(ctx)
	if err != nil {
		return err
	}
	count := 0
	for _, v := range vols {
		if volumeOnPool(v, vg, thinPool) {
			count++
		}
	}
	if count > 0 {
		return apperr.PoolInUse("storages").WithArg("count", count).WithArg("vg", vg)
	}
	return nil
}

// volumeOnPool 判断一个存储的底层卷会不会被"删除 vg/thinPool"一起带走。
//
// 判据按记录的新旧分两种，混用会两头出错：
//
//   - 有 PoolRef（多池之后登记的）：直接比对池键。**不能**顺带用卷引用反解卷组——
//     同一卷组里另一个池上的存储会被误算成"在目标池上"，于是"删 pool2"被 pool1 的存储挡住，
//     用户按提示去删又删不掉（错法本身自相矛盾）；
//   - PoolRef 为空（多池之前登记的，未知落在哪个池）：只能靠卷引用反解卷组。
//     此时卷组相同就拦下：它可能就住在这个池里，而"删掉还在用的存储"是不可逆的。
//
// thinPool 为空表示只删卷组：卷组里任何池上的存储都会被一起带走。
func volumeOnPool(v domain.StorageVolume, vg, thinPool string) bool {
	if ref := strings.TrimSpace(v.PoolRef); ref != "" {
		pvg, ppool, err := platform.SplitPoolKey(ref)
		if err != nil {
			// 库里的池键坏了，无从判断它落在哪：宁可拦下（多拦一次可解释，误删不可逆）。
			return true
		}
		if pvg != vg {
			return false
		}
		return thinPool == "" || ppool == thinPool
	}
	return vgOfDeviceRef(v.Ref) == vg
}

// vgOfDeviceRef 从 /dev/mapper/<vg>-<lv> 反解卷组名；不是这种形态时返回空串。
//
// VG/LV 名被 linuxlvm.lvNameRe 限制在 [A-Za-z0-9_+.]，不含 '-'，因此第一个 '-' 就是分隔符。
// （真实 LVM 会把名字里的 '-' 转义成 '--'，那种名字本系统不会创建；真遇到时这里解不出来，
// 由平台层的 thin 卷判定兜底，绝不会因为"解不出来"就误删。）
func vgOfDeviceRef(ref string) string {
	const prefix = "/dev/mapper/"
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, prefix) {
		return ""
	}
	name := ref[len(prefix):]
	if i := strings.IndexByte(name, '-'); i > 0 {
		return name[:i]
	}
	return ""
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

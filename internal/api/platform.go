package api

import (
	"net/http"

	"vault/internal/app"
	"vault/internal/platform"
)

// blockDeviceDTO 是一块可被选作 LVM 物理卷/缓存盘的块设备。
//
// 之所以不直接序列化 platform.BlockDevice：该结构是内部契约，字段增删不应影响 API；
// 且 API 需要稳定的 JSON 命名与 omitempty 语义（见 api 包注释的分层约定）。
type blockDeviceDTO struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// SizeBytes 容量（字节）。
	SizeBytes int64 `json:"size_bytes"`
	// Model 型号（可能为空）。
	Model string `json:"model"`
	// Rotational 为 true 表示机械盘（HDD），false 为固态（SSD/NVMe）。
	Rotational bool `json:"rotational"`
	// System 为 true 表示承载根文件系统/引导/swap —— 前端必须禁止选作缓存盘。
	System bool `json:"system"`
	// HasFS / HasPV 设备或其子分区上已有文件系统 / 已被 LVM 占用。
	HasFS bool `json:"has_fs"`
	HasPV bool `json:"has_pv"`
	// VGName 若已是某 VG 的成员，给出 VG 名。
	VGName string `json:"vg_name,omitempty"`
	// Partitions 子分区数量。
	Partitions int `json:"partitions"`
	// Reason 不可选用时给出原因（可直接展示）；可选用时为空。
	Reason string `json:"reason,omitempty"`
}

// poolStatusDTO 是存储池与缓存的现状快照。
type poolStatusDTO struct {
	Kind     string `json:"kind"`
	VG       string `json:"vg"`
	ThinPool string `json:"thin_pool"`
	// Key 池的稳定标识（"<vg>/<thin_pool>"），创建存储时用它指定 pool_ref。
	Key string `json:"key"`
	// Default 是否为服务端配置的默认池（前端预选/置顶用）。
	Default bool `json:"default"`
	// Exists 目标 VG 与 thin pool 是否都已存在（false 表示需要先初始化）。
	Exists    bool  `json:"exists"`
	SizeBytes int64 `json:"size_bytes"`
	FreeBytes int64 `json:"free_bytes"`
	// DataPercent / MetadataPercent thin pool 的数据/元数据使用率（0–100）。
	DataPercent     float64 `json:"data_percent"`
	MetadataPercent float64 `json:"metadata_percent"`
	// MaxStorageVolumeBytes 该池上新建存储卷能设置的最大逻辑容量（字节；0 表示未知）。
	//
	// 即池自身的数据容量（**不是**上面的 SizeBytes——那是卷组容量）：创建存储时用它预填
	// "容量"输入框并作为 max，口径只在后端算。
	MaxStorageVolumeBytes int64 `json:"max_storage_volume_bytes"`
	// CacheAttached 是否已挂载 dm-cache。
	CacheAttached    bool   `json:"cache_attached"`
	CacheMode        string `json:"cache_mode,omitempty"`
	CacheChunkSize   string `json:"cache_chunk_size,omitempty"`
	CachePolicy      string `json:"cache_policy,omitempty"`
	CacheDirtyBlocks int64  `json:"cache_dirty_blocks"`
	CacheReadHits    int64  `json:"cache_read_hits"`
	CacheReadMisses  int64  `json:"cache_read_misses"`
	CacheWriteHits   int64  `json:"cache_write_hits"`
	CacheWriteMisses int64  `json:"cache_write_misses"`
	// CacheDevices 正在充当缓存的设备。
	CacheDevices []string `json:"cache_devices,omitempty"`
	// CacheHealth 缓存健康状态（空表示正常；否则为 Fail / needs_check 等）。
	CacheHealth string `json:"cache_health,omitempty"`
}

// initializePoolRequest 是初始化存储池的请求。
type initializePoolRequest struct {
	// VG / ThinPool 目标位置；留空取服务端配置 platform.lvm.*。
	VG       string `json:"vg"`
	ThinPool string `json:"thin_pool"`
	// HDDDevices 组成 VG 的块设备全路径（VG 已存在时忽略）。
	HDDDevices []string `json:"hdd_devices"`
	// SizeBytes thin pool 的**数据容量**（字节）。
	//
	// thin pool 必须给容量，否则 lvcreate 只会报 "No command with matching syntax
	// recognised"。0 表示"占满 VG 当前剩余空间"。
	SizeBytes int64 `json:"size_bytes"`
	// ChunkSize / MetadataSize 建池参数；留空取服务端配置值。
	ChunkSize    string `json:"chunk_size"`
	MetadataSize string `json:"metadata_size"`
	// CacheDevices 用作 dm-cache 的块设备全路径（空数组/缺省表示不加缓存）。
	CacheDevices []string `json:"cache_devices"`
	// CacheChunkSize / CachePolicy / CacheMode 缓存参数。
	CacheChunkSize string `json:"cache_chunk_size"`
	CachePolicy    string `json:"cache_policy"`
	// CacheMode writethrough（默认、安全）| writeback。
	CacheMode string `json:"cache_mode"`
}

// handleLvmStatus 返回存储池与缓存的现状（仅超级管理员）。
//
// 池不存在不是错误：返回 exists=false，由前端引导执行一次初始化。
func (r *Router) handleLvmStatus(w http.ResponseWriter, req *http.Request) {
	st, err := r.deps.App.Pools().Status(req.Context())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toPoolStatusDTO(st))
}

// volumeGroupDTO 是一个卷组（存储池的容量来源）。
type volumeGroupDTO struct {
	Name string `json:"name"`
	// SizeBytes / FreeBytes 卷组总容量与剩余空间（字节）。
	SizeBytes int64 `json:"size_bytes"`
	FreeBytes int64 `json:"free_bytes"`
	// PVCount 物理卷数量。
	PVCount int `json:"pv_count"`
	// ThinPools 该卷组里已有的 thin pool 名（通常 0 或 1 个）。
	ThinPools []string `json:"thin_pools,omitempty"`
	// PoolMaxBytes 在该卷组上新建 thin pool 时的容量上限（字节；<=0 表示不可新建）。
	//
	// 由后端统一算好给前端做输入框 max，避免两端各算一套导致"前端放行、后端拒绝"。
	PoolMaxBytes int64 `json:"pool_max_bytes"`
}

// handleListPools 列出全部存储池与卷组（仅超级管理员）。
//
// 一台机器可以有多个存储池：创建存储时前端据此让用户"选择或新建"。
// 平台不支持池概念（Windows）时返回空目录（items/vgs 均为空），前端隐藏该区块。
func (r *Router) handleListPools(w http.ResponseWriter, req *http.Request) {
	cat, err := r.deps.App.Pools().ListPools(req.Context())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	items := make([]poolStatusDTO, 0, len(cat.Items))
	for i := range cat.Items {
		items = append(items, toPoolStatusDTO(&cat.Items[i]))
	}
	vgs := make([]volumeGroupDTO, 0, len(cat.VolumeGroups))
	for _, g := range cat.VolumeGroups {
		vgs = append(vgs, volumeGroupDTO{
			Name:         g.Name,
			SizeBytes:    g.SizeBytes,
			FreeBytes:    g.FreeBytes,
			PVCount:      g.PVCount,
			ThinPools:    g.ThinPools,
			PoolMaxBytes: g.PoolMaxBytes,
		})
	}
	r.writeJSON(w, http.StatusOK, map[string]any{
		"items":         items,
		"volume_groups": vgs,
		"default_vg":    cat.DefaultVG,
		"default_pool":  cat.DefaultThinPool,
	})
}

// handleListBlockDevices 枚举块设备（供"缓存设备选择器"使用，仅超级管理员）。
func (r *Router) handleListBlockDevices(w http.ResponseWriter, req *http.Request) {
	devs, err := r.deps.App.Pools().ListBlockDevices(req.Context())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	items := make([]blockDeviceDTO, 0, len(devs))
	for _, d := range devs {
		items = append(items, toBlockDeviceDTO(d))
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// releaseBlockDeviceRequest 是"释放设备"请求：把一块盘清成可加入卷组的干净状态。
type releaseBlockDeviceRequest struct {
	// Path 设备全路径（如 "/dev/sdb"）。
	Path string `json:"path"`
}

// deviceReleaseStepDTO 是释放过程中的一步结果。
type deviceReleaseStepDTO struct {
	// Step 步骤标识（umount / swapoff / wipefs / pvremove / mdadm / dmsetup / partprobe / udevadm）。
	Step string `json:"step"`
	// Target 该步作用的设备或挂载点。
	Target string `json:"target,omitempty"`
	// OK 执行成功。
	OK bool `json:"ok"`
	// Skipped 无需执行（未挂载、不是 swap、无对应签名、工具缺失等）。
	Skipped bool `json:"skipped"`
	// Detail 失败原因或补充说明（原始命令输出，可能为空）。
	Detail string `json:"detail,omitempty"`
}

// deviceReleaseReportDTO 是一次"释放设备"的结果。
type deviceReleaseReportDTO struct {
	Path  string                 `json:"path"`
	Steps []deviceReleaseStepDTO `json:"steps"`
	// Released 释放后该设备是否已可被选作卷组设备。
	Released bool `json:"released"`
	// Reason 仍不可用的原因（released 为 true 时为空）。
	Reason string `json:"reason,omitempty"`
}

// handleReleaseBlockDevice 释放一块设备（仅超级管理员）。
//
// ⚠️ 破坏性：会卸载挂载点、关闭 swap、抹除文件系统签名并把设备移出卷组。
// 前端必须经过二次确认才调用；服务端另行拒绝系统盘。
func (r *Router) handleReleaseBlockDevice(w http.ResponseWriter, req *http.Request) {
	var in releaseBlockDeviceRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	rep, err := r.deps.App.Pools().ReleaseBlockDevice(req.Context(), in.Path)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, deviceReleaseReportDTO{
		Path:     rep.Path,
		Steps:    toDeviceReleaseStepDTOs(rep.Steps),
		Released: rep.Released,
		Reason:   rep.Reason,
	})
}

// estimatePoolSizeRequest 是"新建卷组前估算可建池容量"的请求。
type estimatePoolSizeRequest struct {
	// HDDDevices 打算组成新卷组的块设备全路径（至少一块）。
	HDDDevices []string `json:"hdd_devices"`
	// MetadataSize 池元数据大小（如 "256M"）；留空取服务端配置值，
	// 配置也留空则按池容量自适应（元数据要从同一卷组另划，pmspare 还要再占一份等大的）。
	MetadataSize string `json:"metadata_size"`
}

// estimatePoolSizeResponse 是估算结果。
type estimatePoolSizeResponse struct {
	// PoolMaxBytes 这些设备能建出的 thin pool 数据容量上限（字节；<=0 表示建不出来）。
	//
	// 已扣掉池元数据（tmeta + pmspare）与卷组自身的开销，用户照这个值填不会超限。
	PoolMaxBytes int64 `json:"pool_max_bytes"`
}

// handleEstimatePoolSize 估算"用这些设备新建卷组后能建多大的 thin pool"（仅超级管理员）。
//
// "新建卷组 + 新建池"这条路径上卷组与池都还不存在，前端拿不到 pool_max_bytes，
// 自己乘系数又必然与后端校验对不上（真机反馈：自动填好 14 GB，提交却报可用 11.6 GB）。
// 容量口径只此一处，前端拿返回值直接填入并当作输入上限。
func (r *Router) handleEstimatePoolSize(w http.ResponseWriter, req *http.Request) {
	var in estimatePoolSizeRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	max, err := r.deps.App.Pools().EstimatePoolMaxBytes(req.Context(), in.HDDDevices, in.MetadataSize)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, estimatePoolSizeResponse{PoolMaxBytes: max})
}

// handleInitializePool 幂等地初始化存储池（含可选的缓存设备，仅超级管理员）。
func (r *Router) handleInitializePool(w http.ResponseWriter, req *http.Request) {
	var in initializePoolRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	st, err := r.deps.App.Pools().InitializePool(req.Context(), app.InitializePoolInput{
		VG:             in.VG,
		ThinPool:       in.ThinPool,
		HDDDevices:     in.HDDDevices,
		SizeBytes:      in.SizeBytes,
		ChunkSize:      in.ChunkSize,
		MetadataSize:   in.MetadataSize,
		CacheDevices:   in.CacheDevices,
		CacheChunkSize: in.CacheChunkSize,
		CachePolicy:    in.CachePolicy,
		CacheMode:      in.CacheMode,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toPoolStatusDTO(st))
}

// deletePoolRequest 是"删除存储池"请求。
type deletePoolRequest struct {
	// VG 目标卷组；ThinPool 留空表示只删卷组（此时 RemoveVolumeGroup 必须为 true）。
	VG       string `json:"vg"`
	ThinPool string `json:"thin_pool"`
	// RemoveVolumeGroup 连卷组一起删除（卷组里还有别的逻辑卷时会被拒）。
	RemoveVolumeGroup bool `json:"remove_volume_group"`
	// ReleaseDevices 删掉卷组后把物理卷标签清回"可选"状态，磁盘可再次被选作卷组设备。
	ReleaseDevices bool `json:"release_devices"`
}

// poolDeleteReportDTO 是一次"删除存储池"的结果。
type poolDeleteReportDTO struct {
	VG       string `json:"vg"`
	ThinPool string `json:"thin_pool,omitempty"`
	// RemovedVolumeGroup 卷组是否也一并删掉了。
	RemovedVolumeGroup bool `json:"removed_volume_group"`
	// ReleasedDevices 被清回可用状态的设备全路径（未要求释放时为空）。
	ReleasedDevices []string `json:"released_devices,omitempty"`
	// Steps 逐步执行结果（lvremove / vgremove / pvremove / udevadm）。
	Steps []deviceReleaseStepDTO `json:"steps"`
}

// handleDeletePool 删除存储池（可选连带删除卷组，仅超级管理员）。
//
// ⚠️ 破坏性：池里的 thin 卷就是各存储的底层卷、母盘与差异盘，前端必须二次确认后再调。
// 服务端另有三道拦截（默认池 / 池里还有 thin 卷 / 卷组里还有别的卷），见 app.PoolService.DeletePool。
func (r *Router) handleDeletePool(w http.ResponseWriter, req *http.Request) {
	var in deletePoolRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	rep, err := r.deps.App.Pools().DeletePool(req.Context(), app.DeletePoolInput{
		VG:                in.VG,
		ThinPool:          in.ThinPool,
		RemoveVolumeGroup: in.RemoveVolumeGroup,
		ReleaseDevices:    in.ReleaseDevices,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, poolDeleteReportDTO{
		VG:                 rep.VG,
		ThinPool:           rep.ThinPool,
		RemovedVolumeGroup: rep.RemovedVolumeGroup,
		ReleasedDevices:    rep.ReleasedDevices,
		Steps:              toDeviceReleaseStepDTOs(rep.Steps),
	})
}

// toDeviceReleaseStepDTOs 把内部的逐步结果映射为 API DTO（释放设备与删池共用同一形状，
// 前端复用同一套步骤展示；两处各写一份必然漂移）。
func toDeviceReleaseStepDTOs(steps []platform.DeviceReleaseStep) []deviceReleaseStepDTO {
	out := make([]deviceReleaseStepDTO, 0, len(steps))
	for _, s := range steps {
		out = append(out, deviceReleaseStepDTO{
			Step:    s.Step,
			Target:  s.Target,
			OK:      s.OK,
			Skipped: s.Skipped,
			Detail:  s.Detail,
		})
	}
	return out
}

// toBlockDeviceDTO 把内部结构映射为 API DTO。
func toBlockDeviceDTO(d platform.BlockDevice) blockDeviceDTO {
	return blockDeviceDTO{
		Name:       d.Name,
		Path:       d.Path,
		SizeBytes:  d.SizeBytes,
		Model:      d.Model,
		Rotational: d.Rotational,
		System:     d.System,
		HasFS:      d.HasFS,
		HasPV:      d.HasPV,
		VGName:     d.VGName,
		Partitions: d.Partitions,
		Reason:     d.Reason,
	}
}

// toPoolStatusDTO 把内部结构映射为 API DTO。
func toPoolStatusDTO(st *platform.PoolStatus) poolStatusDTO {
	return poolStatusDTO{
		Kind:                  string(st.Kind),
		VG:                    st.VG,
		ThinPool:              st.ThinPool,
		Key:                   st.Key(),
		Default:               st.Default,
		Exists:                st.Exists,
		SizeBytes:             st.SizeBytes,
		FreeBytes:             st.FreeBytes,
		DataPercent:           st.DataPercent,
		MetadataPercent:       st.MetadataPercent,
		MaxStorageVolumeBytes: st.MaxStorageVolumeBytes,
		CacheAttached:         st.CacheAttached,
		CacheMode:             st.CacheMode,
		CacheChunkSize:        st.CacheChunkSize,
		CachePolicy:           st.CachePolicy,
		CacheDirtyBlocks:      st.CacheDirtyBlocks,
		CacheReadHits:         st.CacheReadHits,
		CacheReadMisses:       st.CacheReadMisses,
		CacheWriteHits:        st.CacheWriteHits,
		CacheWriteMisses:      st.CacheWriteMisses,
		CacheDevices:          st.CacheDevices,
		CacheHealth:           st.CacheHealth,
	}
}

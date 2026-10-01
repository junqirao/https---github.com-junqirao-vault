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
	// Exists 目标 VG 与 thin pool 是否都已存在（false 表示需要先初始化）。
	Exists    bool  `json:"exists"`
	SizeBytes int64 `json:"size_bytes"`
	FreeBytes int64 `json:"free_bytes"`
	// DataPercent / MetadataPercent thin pool 的数据/元数据使用率（0–100）。
	DataPercent     float64 `json:"data_percent"`
	MetadataPercent float64 `json:"metadata_percent"`
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
		Kind:             string(st.Kind),
		VG:               st.VG,
		ThinPool:         st.ThinPool,
		Exists:           st.Exists,
		SizeBytes:        st.SizeBytes,
		FreeBytes:        st.FreeBytes,
		DataPercent:      st.DataPercent,
		MetadataPercent:  st.MetadataPercent,
		CacheAttached:    st.CacheAttached,
		CacheMode:        st.CacheMode,
		CacheChunkSize:   st.CacheChunkSize,
		CachePolicy:      st.CachePolicy,
		CacheDirtyBlocks: st.CacheDirtyBlocks,
		CacheReadHits:    st.CacheReadHits,
		CacheReadMisses:  st.CacheReadMisses,
		CacheWriteHits:   st.CacheWriteHits,
		CacheWriteMisses: st.CacheWriteMisses,
		CacheDevices:     st.CacheDevices,
		CacheHealth:      st.CacheHealth,
	}
}

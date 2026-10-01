package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/config"
	"vault/internal/domain"
	"vault/internal/platform"
	"vault/internal/semver"
	"vault/internal/version"
)

// ClientCompatState 表示版本兼容判定的结果。
type ClientCompatState string

const (
	// CompatStateOK 兼容，可正常加载资源。
	CompatStateOK ClientCompatState = "compatible"
	// CompatStateNeedClientUpgrade 客户端版本过低，需升级客户端。
	CompatStateNeedClientUpgrade ClientCompatState = "needs_client_upgrade"
	// CompatStateNeedServerUpgrade 客户端版本过高，需升级该服务端。
	CompatStateNeedServerUpgrade ClientCompatState = "needs_server_upgrade"
	// CompatStateProtocolMismatch 协议版本不匹配（硬性不可用，不可关闭）。
	CompatStateProtocolMismatch ClientCompatState = "incompatible_protocol"
)

// ClientCompatInfo 是下发给客户端的版本兼容契约。
type ClientCompatInfo struct {
	// Enabled 为 false 表示服务端已关闭客户端版本区间检查（详见 docs/implementation.md 3.4.2）。
	Enabled bool `json:"enabled"`
	// Min / Max 为区间端点，空字符串表示该端不限制。
	Min string `json:"min,omitempty"`
	Max string `json:"max,omitempty"`
}

// Capabilities 是运行时能力探测结果，供客户端与管理页展示、并对不支持的入口做隐藏。
type Capabilities struct {
	// PlatformKind 当前平台后端种类：windows | linux（空串表示未装配磁盘后端）。
	PlatformKind string `json:"platform_kind"`
	// WinTarget 本机 iSCSI 目标服务是否可用（Windows=WinTarget 角色；Linux=LIO configfs）。
	WinTarget bool `json:"win_target"`
	// IscsiSessions 是否支持真实枚举 iSCSI 会话（Windows 不支持；Linux(LIO) 支持）。
	IscsiSessions bool `json:"iscsi_sessions"`
	// Lvm 是否具备 LVM 存储池管理能力（Windows 恒为 false）。
	Lvm bool `json:"lvm"`
	// LvmCache 目标 thin pool 上是否已挂载 dm-cache（Lvm 为 false 时恒为 false）。
	LvmCache bool `json:"lvm_cache"`
	// HyperV Hyper-V 模块是否可用（影响 ResetDiskIdentifier / Optimize-VHD 的路径；Linux 恒为 false）。
	HyperV bool `json:"hyperv"`
	// StorageRoot 第一个白名单根目录（向后兼容保留）。
	StorageRoot string `json:"storage_root"`
	// StorageRoots 全部白名单根目录（按配置顺序，已去重）。
	StorageRoots []string `json:"storage_roots"`
	// FileSystem 第一个白名单根所在卷的文件系统（应为 NTFS）。
	FileSystem string `json:"file_system"`
	// VolumeFreeBytes / VolumeTotalBytes 全部根所在卷**按卷去重后**的可用/总空间合计。
	VolumeFreeBytes  int64 `json:"volume_free_bytes"`
	VolumeTotalBytes int64 `json:"volume_total_bytes"`
	// Volumes 按卷去重后的各卷空间明细（同一卷上的多个根合并为一条）。
	Volumes []VolumeCapability `json:"volumes"`
	// DatabaseDriver 当前数据库驱动。
	DatabaseDriver string `json:"database_driver"`
}

// VolumeCapability 是单个卷的空间明细（同一卷上的多个根合并为一条）。
type VolumeCapability struct {
	// Name 卷标识（规范化、小写）。
	Name string `json:"name"`
	// FileSystem 卷文件系统。
	FileSystem string `json:"file_system"`
	// FreeBytes / TotalBytes 卷可用空间 / 总容量。
	FreeBytes  int64 `json:"free_bytes"`
	TotalBytes int64 `json:"total_bytes"`
	// Roots 落在该卷上的全部白名单根（按配置顺序）。
	Roots []string `json:"roots"`
}

// SystemInfo 是 GET /v1/system/info 的响应。
//
// 该接口**匿名可访问**：客户端需要在认证之前完成版本兼容判定，
// 否则证书/账号问题会与版本问题混淆，排障困难（见 docs/implementation.md 3.4.2 约束 3）。
type SystemInfo struct {
	ServerInstanceID string           `json:"server_instance_id"`
	ServerName       string           `json:"server_name"`
	APIVersion       int              `json:"api_version"`
	ServerVersion    string           `json:"server_version"`
	ClientCompat     ClientCompatInfo `json:"client_compat"`
	Features         []string         `json:"features"`
	Capabilities     Capabilities     `json:"capabilities"`
	// UpdateChannel 本服务端提供的更新分发通道（供客户端确认"当前更新源"的通道）。
	// 服务端只 mirror 一份发布目录，因此这是服务端级属性而非客户端参数。
	UpdateChannel string `json:"update_channel"`
}

// features 是本服务端声明支持的能力开关，客户端据此决定是否展示对应入口。
var features = []string{
	"multi_server",
	"lease_heartbeat",
	"temp_share_readonly",
	"shared_repository",
	"exclusive_repository",
	"upload_resumable",
}

// SystemInfo 组装系统信息。
func (a *App) SystemInfo(ctx context.Context) *SystemInfo {
	raw := a.raw()
	compat := a.compat()

	info := &SystemInfo{
		ServerInstanceID: raw.Server.InstanceID,
		ServerName:       raw.Server.Name,
		APIVersion:       version.APIVersion,
		ServerVersion:    version.Version,
		ClientCompat: ClientCompatInfo{
			Enabled: compat.Enabled,
		},
		Features:      features,
		UpdateChannel: a.UpdateChannel(),
	}

	if compat.Min != nil {
		info.ClientCompat.Min = compat.Min.String()
	}
	if compat.Max != nil {
		info.ClientCompat.Max = compat.Max.String()
	}

	info.Capabilities = a.probeCapabilities(ctx, raw)
	return info
}

// probeCapabilities 探测运行时能力。探测项彼此独立，任一项失败不影响其余项。
//
// 性能与副作用：每项探测背后都是一次 PowerShell 子进程（本机缺 iSCSI 角色时
// 单次约 5 秒），而 GET /v1/system/info 是客户端启动与心跳频繁调用的匿名接口，
// 因此这里对探测结果做短 TTL 缓存，避免每次请求都真实探测。
func (a *App) probeCapabilities(ctx context.Context, raw config.Config) Capabilities {
	roots := raw.Storage.EffectiveRoots()
	caps := Capabilities{
		PlatformKind:   string(a.platformKind()),
		StorageRoots:   roots,
		DatabaseDriver: raw.Database.Driver,
	}
	if len(roots) > 0 {
		caps.StorageRoot = roots[0]
	}

	if a.Deps.Iscsi != nil {
		// 类型断言一律带 ok：能力探测结果来自本进程缓存的 any，
		// 断言失败时保守跳过，绝不因类型不符 panic（曾是无条件断言的隐患）。
		if v, ok := a.cachedProbe(ctx, "win_target", func(pctx context.Context) any {
			return a.Deps.Iscsi.Available(pctx) == nil
		}).(bool); ok {
			caps.WinTarget = v
		}
		// 只有真实支持会话枚举的平台才为 true：Windows 返回 ErrUnsupported。
		if v, ok := a.cachedProbe(ctx, "iscsi_sessions", func(pctx context.Context) any {
			if _, err := a.Deps.Iscsi.ListSessions(pctx, ""); err != nil {
				return false
			}
			return true
		}).(bool); ok {
			caps.IscsiSessions = v
		}
	}
	// 存储池能力（LVM）：由 StorageAdmin.Status 是否可用判定；Windows 后端返回 ErrUnsupported。
	if a.Platform != nil {
		if st, ok := a.cachedProbe(ctx, "lvm_pool", func(pctx context.Context) any {
			s, err := a.Platform.Status(pctx)
			if err != nil {
				return (*platform.PoolStatus)(nil)
			}
			return s
		}).(*platform.PoolStatus); ok && st != nil {
			caps.Lvm = true
			caps.LvmCache = st.CacheAttached
		}
	}
	// 平台特有的额外能力（如 Windows 的 Hyper-V 模块）：由后端可选实现，未实现即"无"。
	if bc, ok := a.Disk.(platform.BackendCapabilities); ok {
		if v, ok := a.cachedProbe(ctx, "backend_extra", func(pctx context.Context) any {
			return bc.Capabilities(pctx)
		}).(map[string]bool); ok {
			caps.HyperV = v["hyperv"]
		}
	}

	if a.Vol != nil && len(roots) > 0 {
		if v, ok := a.cachedProbe(ctx, "storage_root_file_system", func(pctx context.Context) any {
			fs, err := a.Vol.FileSystemOf(pctx, roots[0])
			if err != nil {
				return ""
			}
			return fs
		}).(string); ok {
			caps.FileSystem = v
		}
		// 按卷去重：同一块盘上的多个根只计一次。
		usages, _ := a.cachedProbe(ctx, "storage_volumes", func(pctx context.Context) any {
			vs, _ := domain.NewPathGuardSet(roots).Volumes(pctx, a.Vol)
			return vs
		}).([]domain.VolumeUsage)
		var freeTotal, totalTotal int64
		for _, u := range usages {
			freeTotal += u.FreeBytes
			totalTotal += u.TotalBytes
			caps.Volumes = append(caps.Volumes, VolumeCapability{
				Name:       u.Name,
				FileSystem: u.FileSystem,
				FreeBytes:  u.FreeBytes,
				TotalBytes: u.TotalBytes,
				Roots:      u.Roots,
			})
		}
		caps.VolumeFreeBytes, caps.VolumeTotalBytes = freeTotal, totalTotal
	}
	return caps
}

// capProbeCacheTTL 是系统能力探测结果的缓存时长。
//
// 取值理由：
//   - 每次探测都要启动一次 PowerShell（本机缺 iSCSI 角色时实测约 5000ms），
//     而 GET /v1/system/info 会被客户端频繁调用；不缓存既造成明显卡顿，
//     也会因频繁创建子进程而反复弹出控制台窗口；
//   - 60 秒是"显著提速"与"管理员装/卸 Windows 功能后能较快反映"之间的折中；
//   - 缓存只存在于进程内存，**进程重启后自然失效**，因此不需要任何额外的失效/清理逻辑。
//
// 注意：只缓存"系统能力探测"这类低频变化的结果，**绝不**缓存会因管理操作立即变化的
// 业务状态（存储库 / 分配 / 租约 / 任务等一律实时读取）。
const capProbeCacheTTL = 60 * time.Second

// capProbeTimeout 限制单次真实探测的最长时间，避免请求被探测无限期拖住。
const capProbeTimeout = 30 * time.Second

// capCacheEntry 是一条能力探测缓存。
type capCacheEntry struct {
	value any
	at    time.Time
}

// cachedProbe 返回按 key 缓存的能力探测值；缓存缺失或过期时执行 probe 并写入。
//
// 使用 sync.Mutex 串行化整段逻辑：同一时刻只允许一次真实探测，
// 避免并发请求同时启动多个 PowerShell（既是性能问题，也会加剧控制台窗口问题）。
// 探测在 derived 上下文中执行（剥离请求取消、仅保留超时），
// 避免"请求被取消 → 探测失败 → 把错误结果缓存 60 秒"的污染。
func (a *App) cachedProbe(ctx context.Context, key string, probe func(context.Context) any) any {
	a.capCacheMu.Lock()
	defer a.capCacheMu.Unlock()

	if a.capCache == nil {
		a.capCache = make(map[string]capCacheEntry)
	}
	if e, ok := a.capCache[key]; ok && time.Since(e.at) < capProbeCacheTTL {
		return e.value
	}

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), capProbeTimeout)
	defer cancel()
	value := probe(probeCtx)
	a.capCache[key] = capCacheEntry{value: value, at: time.Now()}
	return value
}

// CheckClientCompat 判定客户端是否可与本服务端协作。
//
// 两步判定（顺序不可颠倒）：
//
//	① 协议版本 api_version —— 硬性、不可关闭；
//	② 客户端版本区间 client_compat —— 可被服务端配置关闭。
func (a *App) CheckClientCompat(clientAPIVersion int, clientVersion string) (ClientCompatState, error) {
	if clientAPIVersion != version.APIVersion {
		return CompatStateProtocolMismatch, apperr.IncompatibleProtocol(version.APIVersion, clientAPIVersion)
	}

	compat := a.compat()
	if !compat.Enabled {
		return CompatStateOK, nil
	}

	v := strings.TrimSpace(clientVersion)
	if v == "" {
		return CompatStateOK, nil
	}
	parsed, err := semver.Parse(v)
	if err != nil {
		// 客户端版本号非法时不拦截，交由后续请求自然失败，避免误判造成"资源不可见"。
		return CompatStateOK, nil
	}

	if compat.Min != nil && !parsed.GTE(*compat.Min) {
		return CompatStateNeedClientUpgrade, nil
	}
	if compat.Max != nil && !parsed.LTE(*compat.Max) {
		return CompatStateNeedServerUpgrade, nil
	}
	return CompatStateOK, nil
}

// HealthStatus 是健康检查结果。
type HealthStatus struct {
	OK       bool              `json:"ok"`
	Checks   map[string]string `json:"checks"`
	Failures []string          `json:"failures,omitempty"`
}

// Health 执行健康检查：数据库、iSCSI 服务、白名单卷。
func (a *App) Health(ctx context.Context) *HealthStatus {
	st := &HealthStatus{OK: true, Checks: map[string]string{}}

	if a.Store != nil && a.Store.DB() != nil {
		if err := a.Store.DB().PingContext(ctx); err != nil {
			st.record("database", "error: "+err.Error())
		} else {
			st.record("database", "ok")
		}
	}

	if a.Deps.Iscsi != nil {
		if err := a.Deps.Iscsi.Available(ctx); err != nil {
			st.record("win_target", "error: "+err.Error())
		} else {
			st.record("win_target", "ok")
		}
	}

	root := ""
	if roots := a.raw().Storage.EffectiveRoots(); len(roots) > 0 {
		root = roots[0]
	}
	if a.Vol != nil && root != "" {
		if _, err := a.Vol.FileSystemOf(ctx, root); err != nil {
			st.record("storage_root", "error: "+err.Error())
		} else {
			st.record("storage_root", "ok")
		}
	}

	return st
}

func (h *HealthStatus) record(name, result string) {
	if h.Checks == nil {
		h.Checks = map[string]string{}
	}
	h.Checks[name] = result
	if strings.HasPrefix(result, "error") {
		h.OK = false
		h.Failures = append(h.Failures, name)
	}
}

// errNotImplemented 用于显式标注尚未接入的能力，避免静默返回错误结果。
var errNotImplemented = errors.New("该能力尚未实现")

package agent

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// 本机挂载卷用量的取数节奏。
const (
	// volumeUsageTTL 是实测快照的有效期。
	//
	// 每测一次都要跑一次 PowerShell（Get-Volume，约 1s），而界面按 10s 轮询：没有缓存就是
	// 每 10s 起一个 PowerShell。有它以后开销降到"每 TTL 一次"，而用量是给人看的数字，
	// 滞后十几秒无妨。
	volumeUsageTTL = 15 * time.Second
	// volumeUsageProbeTimeout 是单块卷的取数超时（脚本自身有超时，这里只兜底）。
	volumeUsageProbeTimeout = 15 * time.Second
)

// MountVolumeUsage 是本机某块**已挂载卷**的真实空间占用。
//
// 为什么由客户端自己量（真实反馈："会一直记录最高水位，而不是真实已用"）：服务端的「已用」
// 是**宿主侧物理占用**（Windows 上是差异盘 VHDX 的文件大小），客户机里删掉数据不会让它回落，
// 卡片上的数字于是永远停在"写过的最高水位"，而不是用户眼里的"这块库用了多少"。后者只有
// 本机挂载着的那个卷知道。
//
// 因此这个数字只在本机量、本机界面看，**不上报服务端**：服务端不关心用量（配额仍按宿主侧
// 占用算 —— 那是存储设备真实吃掉的空间，也是防超卖的底线）。
type MountVolumeUsage struct {
	AllocationID string `json:"allocation_id"`
	RepoID       string `json:"repo_id"`
	MountPath    string `json:"mount_path"`
	MountMode    string `json:"mount_mode,omitempty"`
	State        string `json:"state,omitempty"`
	// TotalBytes / FreeBytes 是卷的容量与可用空间，UsedBytes 为两者之差（文件系统口径）。
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
	FileSystem string `json:"file_system,omitempty"`
	// CheckedAt 本次实测时刻（毫秒时间戳）。
	CheckedAt int64 `json:"checked_at,omitempty"`
	// Error 取数失败的原因（"阶段:稳定错误码"）；非空时 total_bytes 为 0，界面按"没有实测值"
	// 处理（回退服务端数字）。失败仍然返回这一条是为了可诊断：为什么这块盘读不到。
	Error string `json:"error,omitempty"`
}

// volumeUsageCache 缓存"本机挂载卷用量"快照：一次实测喂多个轮询请求。
type volumeUsageCache struct {
	mu       sync.Mutex
	items    []MountVolumeUsage
	at       time.Time
	inflight bool

	// 以下字段可注入，便于测试（默认见 newVolumeUsageCache）。
	ttl    time.Duration
	now    func() time.Time
	mounts func() []MountState
	probe  func(context.Context, string) (*domain.VolumeSpace, error)
}

// newVolumeUsageCache 构造快照缓存：mounts 取本机挂载记录，probe 量"路径所在卷"的空间。
func newVolumeUsageCache(
	mounts func() []MountState,
	probe func(context.Context, string) (*domain.VolumeSpace, error),
) *volumeUsageCache {
	return &volumeUsageCache{ttl: volumeUsageTTL, now: time.Now, mounts: mounts, probe: probe}
}

// snapshot 返回最新快照；缓存过期时**同步**重测一次。
//
// 为什么同步：界面第一帧就想要真实数字（后台异步刷新会让它先空一轮）。并发请求只会有
// 一路真的去测、其余直接拿上一份快照 —— 否则多开几个页面就会把 PowerShell 排成队。
func (c *volumeUsageCache) snapshot(ctx context.Context) []MountVolumeUsage {
	c.mu.Lock()
	if !c.at.IsZero() && c.now().Sub(c.at) < c.ttl {
		items := c.items
		c.mu.Unlock()
		return items
	}
	if c.inflight {
		items := c.items
		c.mu.Unlock()
		return items
	}
	c.inflight = true
	c.mu.Unlock()

	items := c.measure(ctx)

	c.mu.Lock()
	c.items, c.at, c.inflight = items, c.now(), false
	c.mu.Unlock()
	return items
}

// measure 逐条实测：只测 mounted 的记录（其它状态没有可读的卷），单条失败只影响该条。
//
// ⚠️ directory 模式的挂载点是"某个卷上的目录"，但挂上去的是**卷挂载点**
// （Add-PartitionAccessPath）：Get-Volume -FilePath 会按路径解析到被挂上的那个卷，
// 所以量到的仍是这块盘，而不是承载这个目录的宿主盘。
func (c *volumeUsageCache) measure(ctx context.Context) []MountVolumeUsage {
	mounts := c.mounts()
	items := make([]MountVolumeUsage, 0, len(mounts))
	for _, ms := range mounts {
		if ms.State != MountStateMounted {
			continue
		}
		path := strings.TrimSpace(ms.MountPath)
		if path == "" {
			continue
		}
		item := MountVolumeUsage{
			AllocationID: ms.AllocationID,
			RepoID:       ms.RepoID,
			MountPath:    ms.MountPath,
			MountMode:    ms.MountMode,
			State:        ms.State,
		}
		probeCtx, cancel := context.WithTimeout(ctx, volumeUsageProbeTimeout)
		space, err := c.probe(probeCtx, path)
		cancel()
		item.CheckedAt = c.now().UnixMilli()
		switch {
		case err != nil:
			item.Error = describeError("usage", err)
		case space == nil || space.TotalBytes <= 0:
			// 卷在、但没有容量（还没就绪/未格式化）：报"读不到"而不是"用了 0"，
			// 免得界面把 0 当成真实用量。
			item.Error = describeError("usage", apperr.New(apperr.CodeUnavailable, http.StatusServiceUnavailable))
		default:
			item.TotalBytes = space.TotalBytes
			item.FreeBytes = space.FreeBytes
			item.UsedBytes = space.TotalBytes - space.FreeBytes
			item.FileSystem = space.FileSystem
		}
		items = append(items, item)
	}
	return items
}

// handleMountVolumeUsage 返回本机各挂载卷的真实空间占用（GET /agent/usage，见 docs/agent-api.md）。
func (a *Agent) handleMountVolumeUsage(w http.ResponseWriter, r *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]any{"items": a.usageSnapshot(r.Context())})
}

// usageSnapshot 取本机挂载卷用量快照（尚未初始化时给空列表，而不是报错）。
func (a *Agent) usageSnapshot(ctx context.Context) []MountVolumeUsage {
	if a.usage == nil {
		return []MountVolumeUsage{}
	}
	return a.usage.snapshot(ctx)
}

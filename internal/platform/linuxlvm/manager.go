//go:build linux

// Package linuxlvm 用 LVM thin LV / thin snapshot 承载"虚拟磁盘"，用 dm-cache(lvmcache)
// 承载"SSD 加速 HDD"，并通过 lsblk/pvs 枚举块设备给前端做"缓存设备选择器"。
//
// 设计约束（详见 internal/platform/backend.go 契约与 docs/implementation.md）：
//   - 零 cgo：所有 LVM/dm 操作都通过 exec 调用官方 CLI；
//   - 引用(ref) 形如 /dev/mapper/<vg>-<lv>，且 VG/LV 名不含 '-'，
//     从而"第一个 '-' 即分隔符"，ref 可逆解析（见 lvNameRe 的说明）；
//   - 一台机器可以有**多个存储池**（多个 VG，各带一个 thin pool），
//     磁盘的池由 ref 里的 VG 反查（resolvePool），存储卷的池由 pool_ref 显式指定；
//     磁盘之间的隔离靠应用层精算 + 本包的水位闸门。
package linuxlvm

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"vault/internal/apperr"
	"vault/internal/platform"
)

const (
	// defaultWatermarkPercent 应用层空间闸门阈值：thin pool 数据/元数据使用率达到即拒绝新建。
	defaultWatermarkPercent = 90.0
	// defaultTimeout 单条 LVM 命令超时；建池、拷盘可能较慢，给得宽松。
	defaultTimeout = 10 * time.Minute
	// probeTimeout 无 ctx 的只读探测（Exists/PhysicalSize/Fingerprint）使用的短超时。
	probeTimeout = 10 * time.Second
	// metadataSnapTimeout 是"元数据快照"三步
	// （reserve_metadata_snap → thin_ls -m → release_metadata_snap）的预算。
	// 比 probeTimeout 宽松：大池的元数据扫描可能较慢，而这一步又必须留有
	// 足够的余量去执行 release——快照被 held 期间池的元数据块无法回收。
	metadataSnapTimeout = 60 * time.Second
)

// Options 是 Manager 的构造参数。
type Options struct {
	// Logger 日志器，nil 时回退 slog.Default()。
	Logger *slog.Logger
	// VG 目标卷组（如 "vg0"），如下几项都不得含 '-'（见 lvNameRe）。
	VG string
	// ThinPool 目标 thin pool（如 "vault"）。
	ThinPool string
	// ChunkSize thin pool 的 chunk（如 "256K"）。
	ChunkSize string
	// MetadataSize thin pool 元数据大小（如 "256M"）；留空表示按池容量自适应
	// （见 poolMetaSizeFor：约 1/500，夹在 64 MiB ~ 4 GiB）。
	MetadataSize string
	// WatermarkPercent 应用层空间闸门阈值，默认 90。
	WatermarkPercent float64
	// Timeout 单条命令超时，默认 10 分钟。
	Timeout time.Duration
}

// Manager 是 Linux 存储后端：同时实现 DiskBackend / VolumeBackend / StorageAdmin。
//
// 线程安全：写操作（lvcreate/lvremove/lvconvert/lvchange）用 mu 串行化，简单优先；
// 只读探测不加锁。
type Manager struct {
	logger *slog.Logger

	vg           string
	thinPool     string
	chunkSize    string
	metadataSize string
	watermark    float64
	timeout      time.Duration

	mu sync.Mutex
}

// 编译期断言：Manager 必须同时满足四个后端接口（含多存储池目录与按池生成引用）。
var (
	_ platform.DiskBackend      = (*Manager)(nil)
	_ platform.VolumeBackend    = (*Manager)(nil)
	_ platform.StorageAdmin     = (*Manager)(nil)
	_ platform.PoolCatalog      = (*Manager)(nil)
	_ platform.PooledDiskBackend = (*Manager)(nil)
)

// New 构造 Manager。
func New(opt Options) *Manager {
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}
	wm := opt.WatermarkPercent
	if wm <= 0 {
		wm = defaultWatermarkPercent
	}
	to := opt.Timeout
	if to <= 0 {
		to = defaultTimeout
	}
	return &Manager{
		logger:       logger,
		vg:           opt.VG,
		thinPool:     opt.ThinPool,
		chunkSize:    opt.ChunkSize,
		metadataSize: opt.MetadataSize,
		watermark:    wm,
		timeout:      to,
	}
}

// Kind 返回平台种类。
func (m *Manager) Kind() platform.Kind { return platform.KindLinux }

// Available 探测 LVM 工具链是否就绪。
//
// 只探测能力，**不要求 thin pool 已存在**（池是否存在由 StorageAdmin.Status 报告）。
func (m *Manager) Available(_ context.Context) error {
	for _, exe := range []string{"lvm", "lvcreate"} {
		if _, err := LookPath(exe); err != nil {
			m.logger.Error("LVM 工具缺失", "exe", exe, "err", err.Error())
			return apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
				WithArg("component", "lvm")
		}
	}
	return nil
}

// run 在单条命令超时约束下执行外部命令。
func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
	if m.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}
	return Run(ctx, m.logger, name, args...)
}

// probeCtx 为无 ctx 的只读方法提供短超时上下文。
func (m *Manager) probeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), probeTimeout)
}

// poolPath 把 (vg, pool) 拼成 lvm 的 "<vg>/<lv>" 形式，并集中校验两者名字合法性。
//
// 多存储池下池名不再是常量：调用方先按 VG 反查（resolvePool，见 poolcatalog.go），
// 或直接来自用户选择的 pool_ref。
func poolPath(vg, pool string) (string, error) {
	if !lvNameRe.MatchString(vg) {
		return "", apperr.InvalidParam("vg")
	}
	if !lvNameRe.MatchString(pool) {
		return "", apperr.InvalidParam("thin_pool")
	}
	return vg + "/" + pool, nil
}

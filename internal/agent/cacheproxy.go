package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"vault/internal/iscsicache"
)

// 客户端本地 iSCSI 读缓存代理：**一个客户端一个代理，一个代理下挂多个 iSCSI 目标**。
//
// 设计要点（见 .trae/documents/客户端iSCSI读缓存实装方案.md）：
//
//   - 门户只有一个，固定监听 127.0.0.1:3261。本机发起端只要连这个地址就够了，
//     每个"启用了缓存的存储库"在门户里注册成自己的目标（按 IQN 路由到各自的 Handler）。
//   - 代理目标的 IQN **必须与服务端下发的 IQN 不同**：Windows 侧的
//     initiator_is_connected / initiator_wait_connected / initiator_disconnect 三个脚本
//     用"短名（IQN 冒号后部分）包含匹配"找会话，两者短名互为子串就会串台。
//   - L1/L2 预算是**客户端总量**（用户在客户端设置里配），按已启用缓存的库**均分**；
//     配额在创建目标时定下，运行中不重算（重算会让已缓存的数据被突然判定为超额）。
//   - 代理不可用（端口占用、后端不可达、CHAP 不支持、配额不足）时挂载流程回退直连
//     服务端目标 —— 缓存是加速手段，绝不能变成挂载的前置条件。
const (
	// cachePortalAddr 门户监听地址。固定端口：本机发起端只配一次门户地址，
	// 换成动态端口只会让"连不上"更难排查。
	cachePortalAddr = "127.0.0.1:3261"

	// cacheIQNPrefix 本地代理目标的 IQN 前缀（见文件头关于短名串台的说明）。
	cacheIQNPrefix = "iqn.2024-01.local.vault:vcache-"

	// 厂商/型号：显式覆盖，保证代理盘与直连盘在 INQUIRY 上可区分。
	//
	// 为什么必须覆盖：volume_find_iscsi_disk.ps1 只按"容量 + BusType=iSCSI"找盘并取编号
	// 最小者，代理盘与直连盘若长得一样（同样的 Vendor/Product/Serial），就可能挂到直连盘上。
	cacheVendor  = "VAULT"
	cacheProduct = "Vault Cache Disk"

	// 缓存预算的默认值与上下界（字节）。缓存只在用户为某个库开启后才真正占用。
	cacheDefaultL1Bytes = 512 << 20
	cacheDefaultL2Bytes = 4 << 30
	// cacheMinL1Bytes 与 cache 包的最小几何对应（64KiB 块 × 256 分片 = 16MiB），留 4 倍余量。
	cacheMinL1Bytes = 64 << 20
	cacheMaxL1Bytes = 64 << 30
	// cacheMinL2Bytes 保证 L2 文件至少能放下超级块与若干槽位。
	cacheMinL2Bytes = 256 << 20
	cacheMaxL2Bytes = 1 << 40
)

// cacheTargetRequest 描述"为某个库挂一个缓存目标"所需的输入。
type cacheTargetRequest struct {
	// AllocationID 同时是门户内的目标 key（同一分配重复挂载时据此幂等复用）。
	AllocationID string
	// BackendAddress/BackendPort/BackendIQN 是服务端下发的目标：代理从它取数据。
	BackendAddress string
	BackendPort    int
	BackendIQN     string
	// AuthMode 是服务端下发的认证方式；chap 暂不支持（后端发起端目前只做 None）。
	AuthMode string
}

// cacheManager 持有客户端唯一的缓存门户，管理其中的多个目标。
type cacheManager struct {
	cfg    *ConfigStore
	logger *slog.Logger

	mu sync.Mutex
	// ctx 是代理的生命周期 context（Agent.Start 传入）。门户是**懒启动**的：
	// 绝大多数用户不会给任何库开缓存，没必要为此常驻一个监听端口；
	// 但一旦启动，门户必须挂在 Agent 的长生命周期 context 上 —— 用挂载请求的 ctx
	// 会让门户在挂载返回后立刻被取消。
	ctx     context.Context
	portal  *iscsicache.Portal
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	lastErr string
}

// newCacheManager 构造缓存代理管理器（此时不打开任何监听）。
func newCacheManager(cfg *ConfigStore, logger *slog.Logger) *cacheManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &cacheManager{cfg: cfg, logger: logger}
}

// Start 记录缓存代理的生命周期 context；门户在首次使用时才真正打开。
func (m *cacheManager) Start(ctx context.Context) {
	if ctx == nil {
		return
	}
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
}

// Close 停止门户并回收所有目标（会 flush L2 文件、logout 后端）。
//
// ⚠️ 必须在 Agent 退出时调用：进程退出（信号 / 自更新）不走 UnmountAll，
// 只有这里是 L2 落盘与后端 logout 的唯一时机。
func (m *cacheManager) Close() error {
	m.mu.Lock()
	p, cancel := m.portal, m.cancel
	m.portal, m.cancel = nil, nil
	m.mu.Unlock()
	if p == nil {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	err := p.Close()
	m.wg.Wait()
	m.logger.Info("本地 iSCSI 缓存代理已停止")
	return err
}

// portalForUse 返回正在运行的门户；未启动时按需打开（懒启动）。
func (m *cacheManager) portalForUse() (*iscsicache.Portal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.portal != nil {
		return m.portal, nil
	}
	if m.ctx == nil {
		return nil, errors.New("cache: 缓存代理尚未启动")
	}
	p, err := iscsicache.NewPortal(iscsicache.PortalConfig{
		ListenAddr: cachePortalAddr,
		Logger:     m.logger.With("component", "iscsi-cache"),
	})
	if err != nil {
		m.lastErr = err.Error()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	m.portal, m.cancel, m.lastErr = p, cancel, ""
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := p.Start(runCtx); err != nil {
			m.logger.Warn("本地 iSCSI 缓存代理已停止", "error", err)
		}
	}()
	m.logger.Info("本地 iSCSI 缓存代理已启动", "addr", p.Addr().String())
	return p, nil
}

// runningPortal 返回已打开的门户（未打开时为 nil），供只读查询使用。
func (m *cacheManager) runningPortal() *iscsicache.Portal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.portal
}

// EnsureTarget 为该库注册/复用缓存目标。幂等：key 已在门户中时直接返回。
//
// 返回错误表示这个库这次用不上缓存（端口占用、后端不可达、CHAP、配额不足等）——
// 调用方据此**回退直连**，而不是让挂载失败。
func (m *cacheManager) EnsureTarget(ctx context.Context, req cacheTargetRequest) error {
	allocationID := strings.TrimSpace(req.AllocationID)
	if allocationID == "" {
		return errors.New("cache: allocation_id is required")
	}
	if strings.EqualFold(strings.TrimSpace(req.AuthMode), "chap") {
		return errors.New("cache: 启用 CHAP 的目标暂不支持本地缓存代理")
	}
	p, err := m.portalForUse()
	if err != nil {
		return err
	}
	l1, l2 := m.quota()
	if l1 < cacheMinL1Bytes {
		return fmt.Errorf("cache: 每库 L1 配额 %d 字节低于下限", l1)
	}

	cc := iscsicache.CacheConfig{}
	cc.L1.SizeBytes = l1
	if l2 >= cacheMinL2Bytes {
		cc.L2.Enabled = true
		cc.L2.Path = cacheL2Path(m.cfg.Get().CacheL2Dir, allocationID)
		cc.L2.SizeBytes = l2
	}
	return p.Add(ctx, iscsicache.TargetConfig{
		Key:       allocationID,
		TargetIQN: cacheTargetIQN(allocationID),
		Backend: iscsicache.BackendConfig{
			Address:   net.JoinHostPort(strings.TrimSpace(req.BackendAddress), strconv.Itoa(cacheBackendPort(req.BackendPort))),
			TargetIQN: strings.TrimSpace(req.BackendIQN),
		},
		Vendor:  cacheVendor,
		Product: cacheProduct,
	})
}

// ReleaseTarget 注销该库的缓存目标并回收其 L2 文件。幂等。
//
// 调用时机：卸载流程在**断开会话之后、回写 release 之前** —— 会话断掉之前回收后端
// 会让仍在飞的命令失败。
func (m *cacheManager) ReleaseTarget(allocationID string) error {
	p := m.runningPortal()
	if p == nil {
		return nil
	}
	return p.Remove(strings.TrimSpace(allocationID))
}

// quota 返回每个启用缓存的库分到的 L1/L2 配额（客户端总量 / 已启用缓存的库数）。
//
// 未启用任何库时按 1 计（避免除零）；实际不会用到，因为只有启用的库才会来申请。
func (m *cacheManager) quota() (l1, l2 int64) {
	cfg := m.cfg.Get()
	enabled := 0
	for _, pref := range cfg.RepoMounts {
		if pref.CacheEnabled {
			enabled++
		}
	}
	if enabled < 1 {
		enabled = 1
	}
	l1 = cfg.CacheL1Bytes / int64(enabled)
	if cfg.CacheL2Bytes > 0 {
		l2 = cfg.CacheL2Bytes / int64(enabled)
	}
	return l1, l2
}

// CacheStatus 是缓存代理的对外状态（GET /agent/cache）。
type CacheStatus struct {
	// Running 门户是否已打开（懒启动：没有任何启用了缓存的库时为空闲状态）。
	Running bool `json:"running"`
	// Addr 门户监听地址。
	Addr string `json:"addr,omitempty"`
	// L1LimitBytes/L2LimitBytes 是客户端总量预算（L2 为 0 表示关闭 L2）。
	L1LimitBytes int64 `json:"l1_limit_bytes"`
	L2LimitBytes int64 `json:"l2_limit_bytes"`
	// L1QuotaBytes/L2QuotaBytes 是当前均分给每个启用缓存的库的配额。
	L1QuotaBytes int64 `json:"l1_quota_bytes"`
	L2QuotaBytes int64 `json:"l2_quota_bytes"`
	// EnabledRepos 是已启用缓存的库数（决定配额分母）。
	EnabledRepos int `json:"enabled_repos"`
	// Targets 是按库（分配）汇总的用量与命中情况。
	Targets []CacheTargetStatus `json:"targets"`
	// Error 是最近一次打开门户失败的原因（如端口被占用）。
	Error string `json:"error,omitempty"`
}

// CacheTargetStatus 是单个库的缓存用量与命中情况。
type CacheTargetStatus struct {
	// AllocationID 与挂载记录、存储库卡片一一对应。
	AllocationID string `json:"allocation_id"`
	// TargetIQN 是本地门户为该库公示的 IQN（挂载链路上实际连接的那个）。
	TargetIQN string `json:"target_iqn"`
	// L1UsedBytes/L1LimitBytes 是内存缓存占用与配额。
	L1UsedBytes  int64 `json:"l1_used_bytes"`
	L1LimitBytes int64 `json:"l1_limit_bytes"`
	// L2UsedBytes/L2LimitBytes 是 L2 文件占用与配额（未启用 L2 时为 0）。
	L2UsedBytes  int64 `json:"l2_used_bytes"`
	L2LimitBytes int64 `json:"l2_limit_bytes"`
	// Reads 是读命令数；RequestHits 是**整条命令**完全由缓存满足的次数。
	Reads       int64 `json:"reads"`
	RequestHits int64 `json:"request_hits"`
	// L1Hits/PartialHits/L2Hits/BackendReads 是分段的细粒度计数。
	L1Hits       int64 `json:"l1_hits"`
	PartialHits  int64 `json:"partial_hits"`
	L2Hits       int64 `json:"l2_hits"`
	BackendReads int64 `json:"backend_reads"`
	Writes       int64 `json:"writes"`
	// HitRate 是 RequestHits/Reads（0..1）；Reads 为 0 时为 0。
	HitRate float64 `json:"hit_rate"`
}

// Status 汇总缓存代理状态，供客户端展示用量与命中情况。
func (m *cacheManager) Status() CacheStatus {
	cfg := m.cfg.Get()
	l1, l2 := m.quota()
	enabled := 0
	for _, pref := range cfg.RepoMounts {
		if pref.CacheEnabled {
			enabled++
		}
	}
	st := CacheStatus{
		L1LimitBytes: cfg.CacheL1Bytes,
		L2LimitBytes: cfg.CacheL2Bytes,
		L1QuotaBytes: l1,
		L2QuotaBytes: l2,
		EnabledRepos: enabled,
		Targets:      []CacheTargetStatus{},
	}

	m.mu.Lock()
	p, lastErr := m.portal, m.lastErr
	m.mu.Unlock()
	if p == nil {
		st.Error = lastErr
		return st
	}
	st.Running = true
	st.Addr = p.Addr().String()
	for _, key := range p.Keys() {
		stats, ok := p.Stats(key)
		if !ok {
			continue
		}
		iqn, _ := p.TargetIQN(key)
		cs := stats.Cache
		ts := CacheTargetStatus{
			AllocationID: key,
			TargetIQN:    iqn,
			L1UsedBytes:  cs.L1UsedBytes,
			L1LimitBytes: cs.L1LimitBytes,
			Reads:        cs.Reads,
			RequestHits:  cs.RequestHits,
			L1Hits:       cs.L1Hits,
			PartialHits:  cs.PartialHits,
			L2Hits:       cs.L2Hits,
			BackendReads: cs.BackendReads,
			Writes:       stats.Proxy.Writes,
		}
		if stats.BlockSize > 0 && stats.L2Slots > 0 {
			slot := int64(stats.BlockSize)
			ts.L2UsedBytes = int64(stats.L2Used) * slot
			ts.L2LimitBytes = int64(stats.L2Slots) * slot
		}
		if cs.Reads > 0 {
			ts.HitRate = float64(cs.RequestHits) / float64(cs.Reads)
		}
		st.Targets = append(st.Targets, ts)
	}
	return st
}

// cachePortalHostPort 拆分门户监听地址：挂载链路要把它当作**生效门户**写进挂载记录。
func cachePortalHostPort() (host string, port int) {
	h, p, err := net.SplitHostPort(cachePortalAddr)
	if err != nil {
		return cachePortalAddr, 3261
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return h, 3261
	}
	return h, n
}

// cacheTargetIQN 返回某个分配在本地门户上的 IQN（见文件头关于短名串台的说明）。
func cacheTargetIQN(allocationID string) string {
	return cacheIQNPrefix + sanitizeIQNSuffix(allocationID)
}

// cacheL2Path 返回某个分配的 L2 数据文件路径。dir 为空时落在默认目录
// `<DataDir>\iscsi-cache`（用户可在客户端设置里改到别的盘）。
func cacheL2Path(dir, allocationID string) string {
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(DataDir(), "iscsi-cache")
	}
	return filepath.Join(dir, "l2-"+sanitizeIQNSuffix(allocationID)+".bin")
}

// cacheBackendPort 返回后端目标的有效端口（服务端未下发时取 iSCSI 默认端口）。
func cacheBackendPort(port int) int {
	if port > 0 {
		return port
	}
	return 3260
}

// sanitizeIQNSuffix 把分配 ID 收敛成 IQN / 文件名都安全的字符集。
//
// 只保留小写字母、数字、'-' 与 '.'：既满足 IQN 的字符要求，也避免生成的路径里出现
// 分隔符（':' 会被 Windows 当成驱动器分隔符与 NTFS 交替数据流）。
func sanitizeIQNSuffix(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

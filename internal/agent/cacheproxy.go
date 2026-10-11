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
	"time"

	"vault/internal/iscsiproxy"
)

// 客户端本地 iSCSI 读缓存代理（v2：透传 TCP + 旁路读缓存）。
//
// v2 与 v1 的终止式代理是两种实现：
//
//   - 门户不再终止 iSCSI 会话，而是把发起端的 TCP 连接逐帧透传到服务端目标；
//     登录、CHAP、协商全部端到端进行，代理不持有任何凭证。
//   - 每个"启用了缓存的库"注册成自己的目标，各自监听一个 127.0.0.1 随机端口：
//     发起端登录的是后端目标的**真实 IQN**，两个目标不能共用一个端口。
//   - 代理只在旁路观测：读未命中就透传并把返回的数据 tee 进缓存；读全命中才吞掉命令、
//     本地合成 Data-In 代答；写命令转发前失效对应范围（见 internal/iscsiproxy）。
//   - L1/L2 预算是**客户端总量**（用户在客户端设置里配），按已启用缓存的库**均分**；
//     配额在创建目标时定下，运行中不重算（重算会让已缓存的数据被突然判定为超额）。
//   - 代理不可用（后端不可达、配额不足、端口占用…）时挂载流程回退直连服务端目标 ——
//     缓存是加速手段，绝不能变成挂载的前置条件。
const (
	// cacheIQNPrefix 是 v1 终止式代理用过、现已废弃的本地目标 IQN 前缀。
	//
	// v2 生效 IQN = 后端目标真实 IQN，不再使用它；保留只为在挂载时把升级前残留的
	// v1 vcache 会话断干净（见 mount.go 的 disconnectStaleTargets）。
	cacheIQNPrefix = "iqn.2024-01.local.vault:vcache-"

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
	//
	// 代理只拨 TCP，不持有凭证：目标认证（CHAP）由发起端与服务端**端到端**完成。
	BackendAddress string
	BackendPort    int
	BackendIQN     string
	// CacheEnabled 该库是否开启缓存：false 时目标只做纯透传（不占 L1/L2 配额），
	// 但仍经过本机代理，从而读写 IOPS 可被监控。
	CacheEnabled bool
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
	portal  *iscsiproxy.Portal
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	lastErr string

	// historyMu 保护 history：采样 goroutine 写、HTTP 读。
	historyMu sync.Mutex
	// history 按库（分配）保留近 5 分钟的采样点，见 CacheHistory。
	history map[string][]CacheSample
}

// 缓存指标历史：近 5 分钟的滑动窗口（60 点 × 5s），供客户端"缓存指标曲线"弹窗展示。
const (
	// cacheHistoryInterval 相邻两个采样点的间隔。
	cacheHistoryInterval = 5 * time.Second
	// cacheHistoryPoints 是每个库保留的采样点数。
	cacheHistoryPoints = 60
)

// CacheSample 是单个库在某一刻的计数快照。速率与命中率由前端按相邻点差分计算，
// 代理只如实记录累计值（重启/重建目标时计数归零，前端对负差分按无数据处理）。
type CacheSample struct {
	// T 是采样时刻（Unix 秒）。
	T int64 `json:"t"`
	// Reads/Hits 是读命令数与整命令命中数的累计值。
	Reads int64 `json:"reads"`
	Hits  int64 `json:"hits"`
	// Writes/BackendReads 是写命令数与回后端读次数的累计值。
	Writes       int64 `json:"writes"`
	BackendReads int64 `json:"backend_reads"`
	// L1UsedBytes/L2UsedBytes 是采样时刻的缓存占用（瞬时值，不做差分）。
	L1UsedBytes int64 `json:"l1_used_bytes"`
	L2UsedBytes int64 `json:"l2_used_bytes"`
}

// CacheHistory 是 GET /agent/cache/history 的响应：按库分组的滑动窗口。
type CacheHistory struct {
	// IntervalSec 是采样间隔；前端换算速率以它为准，而不是硬编码。
	IntervalSec int64 `json:"interval_sec"`
	// Targets 以 allocation_id 为键，值为按时间升序的采样点。
	Targets map[string][]CacheSample `json:"targets"`
}

// newCacheManager 构造缓存代理管理器（此时不打开任何监听）。
func newCacheManager(cfg *ConfigStore, logger *slog.Logger) *cacheManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &cacheManager{cfg: cfg, logger: logger, history: map[string][]CacheSample{}}
}

// Start 记录缓存代理的生命周期 context（门户在首次使用时才真正打开），
// 并启动指标采样：近 5 分钟滑动窗口，供"缓存指标曲线"弹窗使用。
func (m *cacheManager) Start(ctx context.Context) {
	if ctx == nil {
		return
	}
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.sampleLoop(ctx)
	}()
}

// sampleLoop 每 5 秒为门户中的每个库记一个采样点。门户懒启动、库动态增删，
// 因此每轮都现取现算；被移除的库其历史随之丢弃（曲线只对仍在用的库有意义）。
func (m *cacheManager) sampleLoop(ctx context.Context) {
	ticker := time.NewTicker(cacheHistoryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.sample(now)
		}
	}
}

// sample 记录一轮采样。
func (m *cacheManager) sample(now time.Time) {
	p := m.runningPortal()
	if p == nil {
		return
	}
	m.historyMu.Lock()
	defer m.historyMu.Unlock()
	alive := map[string]bool{}
	for _, key := range p.Keys() {
		stats, ok := p.Stats(key)
		if !ok {
			continue
		}
		alive[key] = true
		cs := stats.Cache
		point := CacheSample{
			T:     now.Unix(),
			Reads: stats.Proxy.Reads,
			Hits:  cs.RequestHits,
			// Writes/Reads 都取代理**观测到的命令数**：与缓存是否命中无关，
			// 前端据此画出的 IOPS 反映真实经过代理的负载。
			Writes:       stats.Proxy.Writes,
			BackendReads: cs.BackendReads,
			L1UsedBytes:  cs.L1UsedBytes,
		}
		if stats.BlockSize > 0 {
			point.L2UsedBytes = int64(stats.L2Used) * int64(stats.BlockSize)
		}
		series := append(m.history[key], point)
		if len(series) > cacheHistoryPoints {
			series = series[len(series)-cacheHistoryPoints:]
		}
		m.history[key] = series
	}
	for key := range m.history {
		if !alive[key] {
			delete(m.history, key)
		}
	}
}

// History 返回近 5 分钟的采样窗口（GET /agent/cache/history）。
func (m *cacheManager) History() CacheHistory {
	m.historyMu.Lock()
	defer m.historyMu.Unlock()
	out := make(map[string][]CacheSample, len(m.history))
	for key, series := range m.history {
		cp := make([]CacheSample, len(series))
		copy(cp, series)
		out[key] = cp
	}
	return CacheHistory{IntervalSec: int64(cacheHistoryInterval / time.Second), Targets: out}
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
func (m *cacheManager) portalForUse() (*iscsiproxy.Portal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.portal != nil {
		return m.portal, nil
	}
	if m.ctx == nil {
		return nil, errors.New("cache: 缓存代理尚未启动")
	}
	p, err := iscsiproxy.NewPortal(iscsiproxy.PortalConfig{
		Logger: m.logger.With("component", "iscsi-cache"),
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
	// 版本日志：让用户一眼看出客户端跑的是哪一版缓存实现（v2 = 透传 TCP + 旁路读缓存）。
	m.logger.Info("本地 iSCSI 读缓存代理已启动", "engine", iscsiproxy.Version)
	return p, nil
}

// runningPortal 返回已打开的门户（未打开时为 nil），供只读查询使用。
func (m *cacheManager) runningPortal() *iscsiproxy.Portal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.portal
}

// EnsureTarget 为该库注册/复用代理目标。幂等：key 已在门户中时直接返回。
//
// 开缓存的库注册带缓存的代理目标；**未开缓存的库也走这里**，只是注册成纯透传目标
// （不占 L1/L2 配额），从而每块盘都有一条经过代理的链路、读写 IOPS 都可被观测。
//
// 返回错误表示这个库这次用不上代理（后端不可达、端口占用等）——调用方据此**回退直连**，
// 而不是让挂载失败。
func (m *cacheManager) EnsureTarget(ctx context.Context, req cacheTargetRequest) error {
	allocationID := strings.TrimSpace(req.AllocationID)
	if allocationID == "" {
		return errors.New("cache: allocation_id is required")
	}
	p, err := m.portalForUse()
	if err != nil {
		return err
	}

	// 只有开缓存的库才申请 L1/L2 配额；未开缓存的库走纯透传，不参与配额分母。
	var cc iscsiproxy.CacheConfig
	if req.CacheEnabled {
		l1, l2 := m.quota()
		if l1 < cacheMinL1Bytes {
			return fmt.Errorf("cache: 每库 L1 配额 %d 字节低于下限", l1)
		}
		cc = iscsiproxy.CacheConfig{
			L1Bytes: l1,
			// 与 v1 一致：默认开启顺序扫描准入，连续大顺序读不冲掉热点。
			ScanEnabled:   true,
			ScanThreshold: 32,
		}
		if l2 >= cacheMinL2Bytes {
			cc.L2 = iscsiproxy.L2Config{
				Enabled:   true,
				Path:      cacheL2Path(m.cfg.Get().CacheL2Dir, allocationID),
				SizeBytes: l2,
			}
		}
	}

	addr := net.JoinHostPort(strings.TrimSpace(req.BackendAddress), strconv.Itoa(cacheBackendPort(req.BackendPort)))
	if err := p.Add(ctx, iscsiproxy.TargetConfig{
		Key: allocationID,
		Backend: iscsiproxy.BackendConfig{
			Address:   addr,
			TargetIQN: strings.TrimSpace(req.BackendIQN),
		},
		Cache:   cc,
		NoCache: !req.CacheEnabled,
	}); err != nil {
		return err
	}
	m.logger.Info("本地 iSCSI 代理目标已启用",
		"engine", iscsiproxy.Version, "allocation_id", allocationID,
		"backend", addr, "cache", req.CacheEnabled)
	return nil
}

// TargetAddr 返回某库在本机代理上的监听地址（挂载链路把它当作**生效门户**）。
// 门户未打开或该库未注册时 ok 为 false。
func (m *cacheManager) TargetAddr(allocationID string) (host string, port int, ok bool) {
	p := m.runningPortal()
	if p == nil {
		return "", 0, false
	}
	return p.TargetAddr(strings.TrimSpace(allocationID))
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
	// TargetIQN 是挂载链路上实际登录的 IQN。v2 透传代理用它对应的后端目标真实 IQN。
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
	// v2 每个库各监听一个随机端口，没有单一门户地址：这里回显首个目标的监听地址，
	// 仅作展示；真正的生效门户逐库由 TargetAddr 取。
	if a := p.Addr(); a != nil {
		st.Addr = a.String()
	}
	for _, key := range p.Keys() {
		stats, ok := p.Stats(key)
		if !ok {
			continue
		}
		iqn, _ := p.TargetIQN(key)
		cs := stats.Cache
		// Reads 取代理观测到的读命令数（而非缓存的命中计数）：命中率 = 整条命中 / 读命令，
		// 这样在缓存尚未绑定或首读未命中时读 IOPS 也如实非 0。
		reads := stats.Proxy.Reads
		ts := CacheTargetStatus{
			AllocationID: key,
			TargetIQN:    iqn,
			L1UsedBytes:  cs.L1UsedBytes,
			L1LimitBytes: cs.L1LimitBytes,
			Reads:        reads,
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
		if reads > 0 {
			ts.HitRate = float64(cs.RequestHits) / float64(reads)
		}
		st.Targets = append(st.Targets, ts)
	}
	return st
}

// cacheTargetIQN 返回某个分配在本地门户上的 IQN（v1 遗留命名，见 cacheIQNPrefix 的说明）。
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

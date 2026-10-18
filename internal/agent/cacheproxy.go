package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sort"
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
//   - L1/L2 预算是**客户端总量**（用户在客户端设置里配），所有开了缓存的库在这一个数里
//     按**库容量占比**分摊（见 splitCacheBudget）；配额在创建目标时定下，运行中不重算
//     （重算会让已缓存的数据被突然判定为超额）。
//   - 代理不可用（后端不可达、配额不足、端口占用…）时挂载流程回退直连服务端目标 ——
//     缓存是加速手段，绝不能变成挂载的前置条件。
const (
	// cacheIQNPrefix 是 v1 终止式代理用过、现已废弃的本地目标 IQN 前缀。
	//
	// v2 生效 IQN = 后端目标真实 IQN，不再使用它；保留只为在挂载时把升级前残留的
	// v1 vcache 会话断干净（见 mount.go 的 disconnectStaleTargets）。
	cacheIQNPrefix = "iqn.2024-01.local.vault:vcache-"

	// 缓存预算的默认值与上下界（字节）。缓存只在库挂载且启用缓存后才真正占用。
	cacheDefaultL1Bytes = 512 << 20
	cacheDefaultL2Bytes = 4 << 30
	// cacheMinL1Bytes 与 cache 包的最小几何对应（64KiB 块 × 256 分片 = 16MiB），留 4 倍余量。
	cacheMinL1Bytes = 64 << 20
	cacheMaxL1Bytes = 64 << 30
	// cacheMinL2Bytes 保证 L2 文件至少能放下超级块与若干槽位。
	cacheMinL2Bytes = 256 << 20
	cacheMaxL2Bytes = 1 << 40

	// cacheQuotaGranularity 是配额与推荐值的取整粒度（1 MiB）：分摊出来的数字对齐到 MiB，
	// 界面上不会出现 1048577 字节这种读数。
	cacheQuotaGranularity = 1 << 20

	// cacheRecommendL2Share 是"库容量 → L2 推荐值"的比例：取库容量的 5%。
	//
	// 冷数据不进缓存，收益只在**热**的那部分数据上，而负载的有效工作集经验上只有库容量的
	// 百分之几；给到 5% 已经能覆盖绝大多数热点，再往上加基本不再涨命中率，只是白占磁盘。
	cacheRecommendL2Share = 0.05
	// cacheRecommendL1Divisor 是两级推荐值的比例：L1 = L2 / 8。
	//
	// 两层各司其职：L1 是内存，只放最热的那一层（S3-FIFO 的"频繁"队列）；L2 是磁盘，
	// 放"温"的一层。内存比磁盘贵得多，所以 L1 远小于 L2，热点超出内存的部分自然落到 SSD。
	cacheRecommendL1Divisor = 8
	// cacheRecommendL1MaxBytes 是 L1 推荐值的上限（2 GiB）：库容量继续变大时不该线性抬高
	// 内存占用，客户机上还有别的程序要跑，2 GiB 是"明显加速"与"不影响机器"之间的平衡点。
	cacheRecommendL1MaxBytes = 2 << 30
)

// cacheTargetRequest 描述"为某个库挂一个缓存目标"所需的输入。
type cacheTargetRequest struct {
	// AllocationID 同时是门户内的目标 key（同一分配重复挂载时据此幂等复用）。
	AllocationID string
	// RepoID 是该分配所属的存储库 ID：配额按**库**分摊，它同时是分摊表里的键。
	// 空串（老记录、手工调用）时退化为按分配 ID 单独分摊，等价于这个库独占一份。
	RepoID string
	// RepoSizeBytes 是该库的容量。分摊比例由它决定：1TB 的库分到的份额是 100GB 库的 10 倍。
	//
	// 挂载参数里就带着它（服务端下发的 disk_size_bytes），因此每个库在挂载那一刻即已知；
	// 0 表示未知，此时按"已知库的平均容量"参与分摊（见 splitCacheBudget）。
	RepoSizeBytes int64
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

// key 返回本次请求在配额分摊表里的键（库 ID 优先，缺省退回分配 ID）。
func (r cacheTargetRequest) key() string {
	if id := strings.TrimSpace(r.RepoID); id != "" {
		return id
	}
	return strings.TrimSpace(r.AllocationID)
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

	// repoSizeMu 保护 repoSize：本机**已知的库容量索引**（库 ID → 字节）。
	//
	// 配额按库容量分摊、设置页的推荐值也按它估算，但这两处都不该为了一个数字去打服务端
	// 接口，所以把"已经知道的"记下来：挂载参数里的 disk_size_bytes 与 GET /v1/repos 的
	// capacity_bytes 都会喂进来（见 rememberRepoSize / rememberRepoSizes）。
	//
	// 只增不减：库被删掉后留一条旧记录只会让估算略微偏大，比误删活库的记录安全。
	repoSizeMu sync.Mutex
	repoSize   map[string]int64

	// activeMu 保护 activeTargets：本进程内**已经建立了缓存目标**的分配。
	//
	// 缓存默认开启后，没有单独保存过偏好的库也会开缓存；配额分摊必须把这些库算进去，
	// 否则后挂的库会把同一份总预算再分一遍，多个库一起用就把内存/磁盘吃穿。
	// 映射是 分配 ID → 分摊键（库 ID，缺省为分配 ID）。
	activeMu      sync.Mutex
	activeTargets map[string]string
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
	return &cacheManager{
		cfg:           cfg,
		logger:        logger,
		history:       map[string][]CacheSample{},
		repoSize:      map[string]int64{},
		activeTargets: map[string]string{},
	}
}

// rememberRepoSize 记下某库的容量（<=0 或库 ID 为空时忽略），供配额分摊与推荐值估算使用。
func (m *cacheManager) rememberRepoSize(repoID string, size int64) {
	id := strings.TrimSpace(repoID)
	if id == "" || size <= 0 {
		return
	}
	m.repoSizeMu.Lock()
	defer m.repoSizeMu.Unlock()
	m.repoSize[id] = size
}

// rememberRepoSizes 批量记下一批库容量（GET /v1/repos 返回的整个列表）。
func (m *cacheManager) rememberRepoSizes(sizes map[string]int64) {
	if len(sizes) == 0 {
		return
	}
	m.repoSizeMu.Lock()
	defer m.repoSizeMu.Unlock()
	for repoID, size := range sizes {
		if id := strings.TrimSpace(repoID); id != "" && size > 0 {
			m.repoSize[id] = size
		}
	}
}

// knownRepoSize 返回某库已知的容量（未知时为 0）。
func (m *cacheManager) knownRepoSize(repoID string) int64 {
	m.repoSizeMu.Lock()
	defer m.repoSizeMu.Unlock()
	return m.repoSize[strings.TrimSpace(repoID)]
}

// repoSizeSnapshot 返回已知容量索引的副本。
func (m *cacheManager) repoSizeSnapshot() map[string]int64 {
	m.repoSizeMu.Lock()
	defer m.repoSizeMu.Unlock()
	out := make(map[string]int64, len(m.repoSize))
	for id, size := range m.repoSize {
		out[id] = size
	}
	return out
}

// markCacheTarget 记下"某个分配已经带着缓存目标注册成功"。
func (m *cacheManager) markCacheTarget(allocationID, repoKey string) {
	if m == nil {
		return
	}
	allocationID = strings.TrimSpace(allocationID)
	repoKey = strings.TrimSpace(repoKey)
	if allocationID == "" || repoKey == "" {
		return
	}
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if m.activeTargets == nil {
		m.activeTargets = map[string]string{}
	}
	m.activeTargets[allocationID] = repoKey
}

// unmarkCacheTarget 忘掉某个分配的缓存目标（卸载 / 改回纯透传）。
func (m *cacheManager) unmarkCacheTarget(allocationID string) {
	if m == nil {
		return
	}
	allocationID = strings.TrimSpace(allocationID)
	if allocationID == "" {
		return
	}
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	delete(m.activeTargets, allocationID)
}

// activeRepoKeys 返回当前仍有缓存目标的分摊键（去重、稳定排序）。
func (m *cacheManager) activeRepoKeys() []string {
	if m == nil {
		return nil
	}
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if len(m.activeTargets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(m.activeTargets))
	out := make([]string, 0, len(m.activeTargets))
	for _, key := range m.activeTargets {
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// clearCacheTargets 清空活动目标表（门户关闭后所有目标都已不存在）。
func (m *cacheManager) clearCacheTargets() {
	if m == nil {
		return
	}
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	m.activeTargets = map[string]string{}
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
	m.clearCacheTargets()
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

// ClearTarget 丢弃某个库的本地读缓存内容（L1 与 L2 两级），门户目标与正在进行的会话
// 都保留：读缓存可以在挂载期间清空，不必让客户机重新登录。
//
// 返回 false 表示这个库当前没有缓存目标——没挂载、这次走了直连，或该库注册的是纯透传
// 目标——调用方据此如实告诉用户"没有可清的缓存"，而不是假装清掉了。
func (m *cacheManager) ClearTarget(allocationID string) bool {
	// 零值 cacheManager（单测里直接构造的 Agent，见 TestUsageSnapshotWithoutCache）没有
	// 门户可查：当作没有目标可清，而不是 panic。
	if m == nil {
		return false
	}
	allocationID = strings.TrimSpace(allocationID)
	if allocationID == "" {
		return false
	}
	p := m.runningPortal()
	if p == nil {
		return false
	}
	return p.DropCache(allocationID)
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

	// 只有开缓存的库才申请 L1/L2 配额；未开缓存的库走纯透传，不参与配额分摊。
	var cc iscsiproxy.CacheConfig
	var l1, l2 int64
	if req.CacheEnabled {
		l1, l2 = m.quotaFor(req)
		if l1 < cacheMinL1Bytes {
			return fmt.Errorf("cache: 库 %s 分到的 L1 配额 %d 字节低于下限", req.key(), l1)
		}
		cc = iscsiproxy.CacheConfig{
			L1Bytes: l1,
			// L1 是内存缓存，收尾时落盘、下次启动装回内存：停止代理或重新挂载之后
			// 热数据不必从零预热。它和 L2 独立，关掉 L2 也照样恢复。
			L1Snapshot: cacheL1Path(m.cfg.Get().CacheL2Dir, allocationID),
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
	// 配额分摊要看见"已经存在的缓存目标"：默认开启后，没有单独配置过的库也算一份。
	if req.CacheEnabled {
		m.markCacheTarget(allocationID, req.key())
	} else {
		m.unmarkCacheTarget(allocationID)
	}
	fields := []any{
		"engine", iscsiproxy.Version, "allocation_id", allocationID,
		"backend", addr, "cache", req.CacheEnabled,
	}
	if req.CacheEnabled {
		// 配额是按库容量分摊出来的，日志里带上"分到多少"便于解释"为什么这块盘缓存这么小"。
		fields = append(fields, "l1_bytes", l1, "l2_bytes", l2)
	}
	m.logger.Info("本地 iSCSI 代理目标已启用", fields...)
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
	allocationID = strings.TrimSpace(allocationID)
	p := m.runningPortal()
	if p == nil {
		m.unmarkCacheTarget(allocationID)
		return nil
	}
	if err := p.Remove(allocationID); err != nil {
		return err
	}
	m.unmarkCacheTarget(allocationID)
	return nil
}

// quotaFor 计算本次要开缓存的库分到的 L1/L2 配额。
//
// 分摊集合是**所有已启用缓存的库**，再加本次请求的库（配置可能刚改、还没落盘时以请求为准）：
// 每个开了缓存的库都要占一份，否则先挂载的库会把整份预算吃光，后挂的库一个字节都分不到。
//
// 分摊结果在库挂载那一刻定死，运行中不重算（见文件头）：已经有缓存数据的库不该因为新挂
// 了一个库就被判定超额。代价是"运行中新开的库只能分到剩余的部分"，这份取舍是刻意的。
func (m *cacheManager) quotaFor(req cacheTargetRequest) (l1, l2 int64) {
	cfg := m.cfg.Get()
	key := req.key()
	if key == "" {
		return 0, 0
	}

	ids := cachePoolRepos(cfg, key, m.activeRepoKeys())
	repos := make([]cacheRepoSize, 0, len(ids))
	for _, id := range ids {
		size := m.knownRepoSize(id)
		if id == key && req.RepoSizeBytes > 0 {
			// 挂载参数里的容量是权威值（服务端当场下发），优先于索引，并顺手记下来。
			size = req.RepoSizeBytes
			m.rememberRepoSize(id, size)
		}
		repos = append(repos, cacheRepoSize{ID: id, SizeBytes: size})
	}

	l1 = splitCacheBudget(cfg.CacheL1Bytes, cacheMinL1Bytes, repos)[key]
	if cfg.CacheL2Bytes > 0 {
		l2 = splitCacheBudget(cfg.CacheL2Bytes, cacheMinL2Bytes, repos)[key]
	}
	return l1, l2
}

// cacheRepoSize 是一个参与配额分摊的库。
type cacheRepoSize struct {
	// ID 在本次分摊里唯一即可（库 ID 优先，缺省时是分配 ID）。
	ID string
	// SizeBytes 是库容量；0 表示未知，按已知库的平均容量参与分摊。
	SizeBytes int64
}

// splitCacheBudget 把**总预算**按各库容量分摊给每个库，返回库 ID → 配额。
//
// 客户端设置里配的是总上限（所有开缓存的库在这一个数里分），分摊规则：
//
//   - 按**库容量占比**分：1TB 的库分到的份额是 100GB 库的 10 倍。缓存的价值在热点数据，
//     热点规模与库的大小正相关，按容量分比按库数均分更贴近实际；均分会让 20GB 的小库
//     白占一份和大库一样大的份额，而大库只能覆盖自己极小的一角。
//   - 单个库**不超过自己的容量**：缓存比数据还大没有意义（多出来的部分永远装不满），
//     被削掉的那份转给还没封顶的库。
//   - 预算够时每个库至少 floor：低于下限的缓存放不下块索引，等于白开一个目标。
//   - 容量未知的库按"已知库的平均容量"看待；一个都不知时权重相同，退化为按库数均分
//     （也就是这条规则生效前的行为）。
//
// 结果对齐到 MiB，且**不会超过 total**（上限就是上限）。
func splitCacheBudget(total, floor int64, repos []cacheRepoSize) map[string]int64 {
	out := make(map[string]int64, len(repos))
	for _, r := range repos {
		out[r.ID] = 0
	}
	if total <= 0 || len(repos) == 0 {
		return out
	}

	n := int64(len(repos))
	// 未知容量的库取已知库的平均容量；一个都不知时权重全为 1 → 均分。
	var knownSum int64
	var knownCount int64
	for _, r := range repos {
		if r.SizeBytes > 0 {
			knownSum += r.SizeBytes
			knownCount++
		}
	}
	fill := int64(1)
	if knownCount > 0 {
		fill = knownSum / knownCount
	}

	weights := make([]float64, len(repos))
	caps := make([]int64, len(repos)) // 0 表示不封顶（容量未知）
	for i, r := range repos {
		if r.SizeBytes > 0 {
			weights[i] = float64(r.SizeBytes)
			caps[i] = r.SizeBytes
		} else {
			weights[i] = float64(fill)
		}
	}

	// 保底：预算够时每个库先拿 floor，余额按权重分；不够时连保底也不给，直接按权重分
	// （此时配额可能低于下限，是否降级由调用方决定）。
	base := int64(0)
	if floor > 0 && total >= floor*n {
		base = floor
	}
	left := total - base*n

	assigned := make([]int64, len(repos))
	frozen := make([]bool, len(repos)) // 已经顶到库容量的库不再参与再分配
	for i := range repos {
		if caps[i] > 0 && caps[i] <= base {
			frozen[i] = true
		}
	}
	// 水填法：每轮按剩余权重分剩下的预算，顶到库容量的库被冻结，下一轮把剩下的分给其余库。
	// 实际负载下最多两轮就收敛（封顶需要"总预算 > 库容量"才可能发生），轮数上限只是兜底。
	for pass := 0; pass < 8 && left > 0; pass++ {
		var freeWeight float64
		for i := range weights {
			if !frozen[i] {
				freeWeight += weights[i]
			}
		}
		if freeWeight <= 0 {
			break
		}
		var used int64
		for i := range weights {
			if frozen[i] {
				continue
			}
			share := int64(float64(left) * weights[i] / freeWeight)
			if caps[i] > 0 && base+assigned[i]+share > caps[i] {
				share = caps[i] - base - assigned[i]
				if share < 0 {
					share = 0
				}
				frozen[i] = true
			}
			assigned[i] += share
			used += share
		}
		if used <= 0 {
			break
		}
		left -= used
	}

	for i, r := range repos {
		out[r.ID] = (base + assigned[i]) / cacheQuotaGranularity * cacheQuotaGranularity
	}
	return out
}

// CacheRecommend 是"该配多大缓存"的估算值：客户端设置里显示在两个配置项后面的推荐值。
type CacheRecommend struct {
	// L1Bytes/L2Bytes 是推荐的两级预算；还不知道任何库容量时为 0。
	L1Bytes int64 `json:"l1_bytes"`
	L2Bytes int64 `json:"l2_bytes"`
	// BasisBytes/BasisRepos 是估算依据：本机已知容量的库合计与个数（0 表示还没有数据）。
	//
	// "已知"来自挂载参数里的库容量与 GET /v1/repos 的 capacity_bytes —— 客户端不会为了一个
	// 建议值去额外查询服务端，因此刚装上、还没挂过库的客户端给不出推荐（界面提示"暂无"）。
	BasisBytes int64 `json:"basis_bytes"`
	BasisRepos int   `json:"basis_repos"`
}

// cacheRecommend 按本机已知的库容量估算一份合适的预算（口径见 cacheRecommend* 常量）：
// L2 取总库容量的 5%，L1 取 L2 的 1/8（另有 2 GiB 上限），最后按配置允许的上下界夹紧。
//
// 它只是**建议**：库容量不等于热点规模，实际该给多少要看命中率曲线（见客户端"缓存指标"
// 弹窗里的命中率曲线，涨不动了就是给多了）。
func cacheRecommend(totalBytes int64, repos int) CacheRecommend {
	rec := CacheRecommend{BasisBytes: totalBytes, BasisRepos: repos}
	if totalBytes <= 0 || repos <= 0 {
		return rec
	}

	l2 := int64(float64(totalBytes) * cacheRecommendL2Share)
	if l2 > totalBytes {
		l2 = totalBytes
	}
	if l2 > cacheMaxL2Bytes {
		l2 = cacheMaxL2Bytes
	}
	// 下限放在最后：库比最小缓存还小时推荐值会略大于库容量，那是"缓存的物理下限"，
	// 不是估算错误（这种库本来就不值得开缓存）。
	if l2 < cacheMinL2Bytes {
		l2 = cacheMinL2Bytes
	}

	l1 := l2 / cacheRecommendL1Divisor
	if l1 > cacheRecommendL1MaxBytes {
		l1 = cacheRecommendL1MaxBytes
	}
	if l1 < cacheMinL1Bytes {
		l1 = cacheMinL1Bytes
	}
	if l1 > cacheMaxL1Bytes {
		l1 = cacheMaxL1Bytes
	}

	rec.L1Bytes = l1 / cacheQuotaGranularity * cacheQuotaGranularity
	rec.L2Bytes = l2 / cacheQuotaGranularity * cacheQuotaGranularity
	return rec
}

// cachePoolRepos 返回配额分摊集合：配置里启用缓存的库、当前已经有缓存目标的库（active），
// 外加 extra（本次要挂的那个库，配置可能刚改、还没落盘时以请求为准）。
//
// active 不能省：缓存**默认开启**后，没有单独保存过偏好（不在 cfg.RepoMounts 里）的库也会
// 建立缓存目标；只看配置会让后挂的库把同一份总预算再分一遍。
//
// 去空白、去重、稳定排序 —— 分摊结果不该随 map 迭代顺序变化。
func cachePoolRepos(cfg Config, extra string, active []string) []string {
	ids := make([]string, 0, len(cfg.RepoMounts)+len(active)+1)
	seen := make(map[string]struct{}, len(cfg.RepoMounts)+len(active)+1)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	add(extra)
	for _, id := range active {
		add(id)
	}
	for repoID, pref := range cfg.RepoMounts {
		if pref.cacheEnabledOrDefault() {
			add(repoID)
		}
	}
	sort.Strings(ids)
	return ids
}

// CacheStatus 是缓存代理的对外状态（GET /agent/cache）。
type CacheStatus struct {
	// Running 门户是否已打开（懒启动：没有任何启用了缓存的库时为空闲状态）。
	Running bool `json:"running"`
	// Addr 门户监听地址。
	Addr string `json:"addr,omitempty"`
	// L1LimitBytes/L2LimitBytes 是客户端总量预算（所有开缓存的库在这一个数里按库容量分摊；
	// L2 为 0 表示关闭 L2）。逐库分到多少见 Targets 里的 l1_limit_bytes/l2_limit_bytes。
	L1LimitBytes int64 `json:"l1_limit_bytes"`
	L2LimitBytes int64 `json:"l2_limit_bytes"`
	// EnabledRepos 是已启用缓存的库数（分摊集合的规模）。
	EnabledRepos int `json:"enabled_repos"`
	// KnownRepoBytes/KnownRepos 是本机**已知容量**的库合计与个数。
	//
	// 分摊与推荐值都按它算：挂载过的库（挂载参数带容量）与 GET /v1/repos 返回的库都在其中，
	// 刚装上、还没挂过库时为 0。
	KnownRepoBytes int64 `json:"known_repo_bytes"`
	KnownRepos     int   `json:"known_repos"`
	// Recommend 是"该配多大"的推荐值，界面上显示在两个预算配置项后面。
	Recommend CacheRecommend `json:"recommend"`
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
	// L1UsedBytes/L1LimitBytes 是内存缓存占用与配额。占用只算块数据
	// （blocks × blockSize），配额口径与它一致。
	//
	// L1ResidentBytes 是这份索引**实际**占的堆：块数据之外还有 entry 头、map 桶和
	// 三个 FIFO 队列的槽位。它是估算值（见 cache.ResidentBytes），用来跟
	// GET /agent/memstats 的 heap_alloc 对账：两者之差就是账外部分。
	L1UsedBytes     int64 `json:"l1_used_bytes"`
	L1LimitBytes    int64 `json:"l1_limit_bytes"`
	L1ResidentBytes int64 `json:"l1_resident_bytes"`
	// L2UsedBytes/L2LimitBytes 是 L2 文件占用与配额（未启用 L2 时为 0）。
	L2UsedBytes  int64 `json:"l2_used_bytes"`
	L2LimitBytes int64 `json:"l2_limit_bytes"`
	// L1Restored 是本次绑定缓存时从 L1 内存快照装回的块数；为 0 表示 L1 从空开始
	// （没有快照，或快照属于另一块盘/几何不符而不被信任）。这就是"停止代理或重挂库
	// 之后缓存不归零"的直接读数，与 L2 是否开启无关。
	L1Restored int `json:"l1_restored"`
	// WholeDrops 是"命令形态没看懂"导致整盘清空读缓存的次数。它和失效计数分开：
	// 缓存从几百 MB 掉到几 KB 只可能是这个计数在涨，日志里每条都带 reason。
	WholeDrops int `json:"whole_drops"`
	// L2Restored 是本次打开 L2 文件时从快照恢复的槽数；干净关闭后重开也走这条路。
	// L2Unverified 是其中**还没核实**的槽数：这些槽的索引来自上一次未正常关闭的会话，
	// 要读到它时用槽上的 stamp 核对过才敢用，因此会随读取逐步归零。
	L2Restored   int `json:"l2_restored"`
	L2Unverified int `json:"l2_unverified"`
	// L2Dropped 是核实失败的槽数：索引指向的槽已被后来的写改作他用，读到这里本会拿到
	// 别的块的数据，现已按未命中处理。持续增长说明索引落盘跟不上写入，可以据此调大预算。
	L2Dropped int `json:"l2_dropped"`
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
	// 已知库容量：合计与个数既用于设置页的推荐值，也解释"为什么配额是这个数"。
	var knownBytes int64
	known := m.repoSizeSnapshot()
	for _, size := range known {
		knownBytes += size
	}
	st := CacheStatus{
		L1LimitBytes:   cfg.CacheL1Bytes,
		L2LimitBytes:   cfg.CacheL2Bytes,
		EnabledRepos:   len(cachePoolRepos(cfg, "", m.activeRepoKeys())),
		KnownRepoBytes: knownBytes,
		KnownRepos:     len(known),
		Recommend:      cacheRecommend(knownBytes, len(known)),
		Targets:        []CacheTargetStatus{},
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
	// 一次遍历门户表取回全部（计数 + IQN）：每个 key 分别查两次表会把门户锁拿两遍，
	// 而库数正是这条路径上唯一会增长的量（见 Portal.Snapshots）。
	for key, snap := range p.Snapshots() {
		stats := snap.Stats
		cs := stats.Cache
		// Reads 取代理观测到的读命令数（而非缓存的命中计数）：命中率 = 整条命中 / 读命令，
		// 这样在缓存尚未绑定或首读未命中时读 IOPS 也如实非 0。
		reads := stats.Proxy.Reads
		ts := CacheTargetStatus{
			AllocationID:    key,
			TargetIQN:       snap.IQN,
			L1UsedBytes:     cs.L1UsedBytes,
			L1LimitBytes:    cs.L1LimitBytes,
			L1ResidentBytes: cs.L1ResidentBytes,
			Reads:           reads,
			RequestHits:     cs.RequestHits,
			L1Hits:          cs.L1Hits,
			PartialHits:     cs.PartialHits,
			L2Hits:          cs.L2Hits,
			BackendReads:    cs.BackendReads,
			Writes:          stats.Proxy.Writes,
			L1Restored:      stats.L1Restored,
			WholeDrops:      stats.WholeDrops,
			L2Restored:      stats.L2Restored,
			L2Unverified:    stats.L2Unverified,
			L2Dropped:       stats.L2Dropped,
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

// cacheStorageDir 解析缓存文件的目录。dir 为空时落在默认目录
// `<DataDir>\iscsi-cache`（用户可在客户端设置里改到别的盘）。
func cacheStorageDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return filepath.Join(DataDir(), "iscsi-cache")
	}
	return dir
}

// cacheL2Path 返回某个分配的 L2 数据文件路径。
func cacheL2Path(dir, allocationID string) string {
	return filepath.Join(cacheStorageDir(dir), "l2-"+sanitizeIQNSuffix(allocationID)+".bin")
}

// cacheL1Path 返回某个分配的 L1 内存快照路径，与 L2 同目录。
func cacheL1Path(dir, allocationID string) string {
	return filepath.Join(cacheStorageDir(dir), "l1-"+sanitizeIQNSuffix(allocationID)+".bin")
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

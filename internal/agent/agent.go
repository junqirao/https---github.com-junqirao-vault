package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	"vault/internal/apperr"
	"vault/internal/lock"
	"vault/internal/platform/iscsiinitiator"
	"vault/internal/platform/volume"
	"vault/internal/platform/winps"
)

// 启动与探测相关的时间常量。
const (
	// defaultProbeTimeout 探测 iSCSI 发起端可用性的超时。
	defaultProbeTimeout = 15 * time.Second
	// sessionProbeTimeout 拿到会话后刷新服务端信息的超时。
	sessionProbeTimeout = 30 * time.Second
	// hostProbeInterval 是"主机不就绪"期间的复检间隔。
	//
	// 只在不就绪时复检：管理员手动启动 MSiSCSI 后界面横幅应自动消失（不必重启客户端），
	// 而就绪状态下不做任何后台探测 —— 探测要走一次 PowerShell（约 1s），不能常驻跑。
	hostProbeInterval = 30 * time.Second
)

// Options 是 Agent 的构造参数。
type Options struct {
	// Token 本地访问令牌（必填；由调用方生成或从环境变量取得）。
	Token string
	// Port 本地监听端口；0 表示由系统分配。
	Port int
	// ConfigPath 本地配置文件路径；为空时使用 DefaultConfigPath()。
	ConfigPath string
	// StatePath 本地状态文件路径；为空时使用 DefaultStatePath()。
	StatePath string
	// Logger 日志器，可为 nil。
	Logger *slog.Logger
	// LogSource 日志只读访问（供 GET /agent/log 日志模块）；可为 nil（不提供日志查询）。
	//
	// 由 logging.Logger 实现（见 cmd/vault-agent 的装配）。为什么需要它：挂载失败时
	// 原始报错只进代理日志，界面看不到 —— 用户需要一处能直接看到日志的地方
	// （真实反馈："在客户端上加一个日志模块……不然什么都看不到"）。
	LogSource LogSource
	// Version 代理版本号，为空时取构建注入的 version.Version。
	Version string
}

// LogSource 是对代理日志文件的只读访问接口（logging.Logger 天然满足）。
//
// 日志按天切分（logging 的 dailyRotator），因此查询维度是"日期"而不是行号/偏移：
// 界面可先列可查日期，再取其中一天的尾部。
type LogSource interface {
	// Days 返回存在日志文件的日期（YYYY-MM-DD，升序）。
	Days() []string
	// PathForDay 返回指定日期日志文件路径（可能为空）；day 为空表示当天。
	PathForDay(day string) string
	// ReadDay 读取指定日期日志文件末尾最多 maxBytes 字节；day 为空表示当天。
	ReadDay(day string, maxBytes int64) ([]byte, error)
}

// Agent 是 Vault-Agent 的核心：本地 HTTP + 挂载引擎 + 心跳 + 服务端事件订阅。
type Agent struct {
	version string
	token   string
	port    int
	admin   bool

	logger    *slog.Logger
	logSource LogSource
	cfg       *ConfigStore
	store     *stateStore
	// identity 本地客户端证书身份（免密登录用），与状态文件同目录。
	identity *identityStore
	hub      *EventHub
	ps       *winps.Runner
	iscsi    *iscsiinitiator.Manager
	vol      *volume.Manager
	locks    *lock.Keyed
	engine   *mountEngine
	// cache 是本机 iSCSI 读缓存代理：**一个客户端一个代理，一个代理下挂多个目标**
	// （见 cacheproxy.go）。没有库启用缓存时不监听任何端口（懒启动）。
	cache *cacheManager
	// downloads 管理母盘内容下载任务（内存态）。
	downloads *downloadManager
	// uploads 管理"本地目录 → 存储库"上传任务（内存态 + <DataDir>/uploads.json 续传记录）。
	uploads *uploadManager
	// manualUnmounts 是"本次运行被用户手动卸载过"的库（内存态，进程重启即清空）：
	// 这些库不再被自动挂载，避免用户点了卸载却马上被 auto_mount 挂回来。
	manualUnmounts manualUnmountGuard

	// hostMu / host 是本机就绪状态（当前只有 iSCSI 发起端）：启动时只读探测一次，
	// 未就绪期间低频复检。预检的定位是"提前告知"而非"提前失败"——不阻断启动，界面挂横幅。
	hostMu sync.RWMutex
	host   HostState

	mu       sync.Mutex
	listener net.Listener
	server   *http.Server
	baseCtx  context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	started  bool

	done     chan struct{}
	doneOnce sync.Once

	updateMu         sync.Mutex
	downloading      bool
	availableVersion string
	// updateReceived / updateTotal 是自更新产物下载进度（同时用于 GET /agent/state 与 update 事件）。
	updateReceived int64
	updateTotal    int64
	// updateReason / updateServerReason 记录最近一次"无可用更新"的原因（含服务端返回的具体 reason）。
	updateReason       string
	updateServerReason string
	// web 管理渲染层资源热更（client_web，见 update.go）。
	web *webUpdateManager
	// updateStarted 标记"本次退出由自更新触发"：为 true 时退出流程不得卸载挂载
	// （挂载由 updater 重启后的自动挂载恢复，见 update.go 的说明）。
	updateStarted atomic.Bool

	subMu sync.Mutex
	// subRunning 记录"哪些服务端已经在订阅事件流"：多服务端**每台一条** SSE 长连接，
	// 各自独立重连/降级（见 sse.go）。
	subRunning map[string]bool
	// eventsUnsupported 记录"哪些服务端不支持事件流"（404/501 等）：按台降级，不互相牵连 ——
	// 一台老服务端不支持 SSE 不该让其它台的挂载阶段事件也全丢。
	eventsUnsupported map[string]bool

	// 客户端证书会话自动续期状态（见 session_renew.go）。逐台一份：一台服务端返回
	// "需要重新登录"不该把其它台的续期也停掉。
	renewMu      sync.Mutex
	renewLastAt  map[string]int64
	renewBlocked map[string]bool
	// authRetryLastAt / authRetryInFlight 是"401 触发的证书重登录"的节流与单飞状态（逐台一份）。
	authRetryLastAt   map[string]int64
	authRetryInFlight map[string]*authRetryCall
}

// New 构造 Agent。
func New(opt Options) (*Agent, error) {
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}

	token := strings.TrimSpace(opt.Token)
	if token == "" {
		return nil, fmt.Errorf("agent: 本地令牌不能为空")
	}

	cfg, err := NewConfigStore(opt.ConfigPath, logger)
	if err != nil {
		return nil, err
	}
	state, err := NewStateStore(opt.StatePath, logger)
	if err != nil {
		return nil, err
	}

	ps := winps.NewRunner(winps.Options{Logger: logger})
	a := &Agent{
		version:   defaultVersion(opt.Version),
		token:     token,
		port:      opt.Port,
		admin:     isElevated(),
		logger:    logger,
		logSource: opt.LogSource,
		cfg:       cfg,
		store:     state,
		identity:  newIdentityStore(identityPathFor(state.Path()), logger),
		hub:       NewEventHub(),
		ps:        ps,
		iscsi:     iscsiinitiator.NewManager(ps, logger),
		vol:       volume.NewManager(ps, logger),
		locks:     lock.NewKeyed(),
		done:      make(chan struct{}),

		downloads: newDownloadManager(),
		uploads:   newUploadManager(filepath.Join(filepath.Dir(state.Path()), uploadsFileName)),

		subRunning:        make(map[string]bool),
		eventsUnsupported: make(map[string]bool),
		renewLastAt:       make(map[string]int64),
		renewBlocked:      make(map[string]bool),
		authRetryLastAt:   make(map[string]int64),
		authRetryInFlight: make(map[string]*authRetryCall),
	}
	a.web = newWebUpdateManager(a)
	a.engine = &mountEngine{a: a}
	a.cache = newCacheManager(cfg, logger)
	a.logUpdateKeyStatus()
	return a, nil
}

// Version 返回代理版本号。
func (a *Agent) Version() string { return a.version }

// Token 返回本地访问令牌。
func (a *Agent) Token() string { return a.token }

// Admin 返回是否以管理员权限运行。
func (a *Agent) Admin() bool { return a.admin }

// Done 返回一个通道：当收到 POST /agent/shutdown 时关闭。
//
// 主程序应同时监听该通道与操作系统信号，收到后先卸载全部挂载再调用 Stop。
func (a *Agent) Done() <-chan struct{} { return a.done }

// signalDone 通知主程序「已请求退出」（幂等）。
func (a *Agent) signalDone() {
	a.doneOnce.Do(func() { close(a.done) })
}

// safeGo 启动一个后台 goroutine，并统一兜住其 panic。
//
// 与 cmd/vault-server 的同名小工具语义一致：后台 goroutine 的 panic 不会被 net/http
// 的 handler recover 捕获，会直接终止整个代理进程（用户侧表现为"本地代理未连接"）。
// 这里保证 panic 只记 ERROR（含任务名与完整调用栈）并终止该 goroutine，不影响其余功能。
func safeGo(log *slog.Logger, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if log == nil {
					log = slog.Default()
				}
				log.Error("后台 goroutine panic，已捕获并继续运行",
					"task", name, "panic", r, "stack", string(debug.Stack()))
			}
		}()
		fn()
	}()
}

// Start 启动本地 HTTP 服务与后台循环，返回实际监听地址（host:port）。
func (a *Agent) Start(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return "", apperr.New(apperr.CodeConflict, http.StatusConflict).WithArg("reason", "already_started")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(a.port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		a.mu.Unlock()
		return "", fmt.Errorf("agent: 监听本地端口失败: %w", err)
	}
	a.listener = listener
	a.baseCtx, a.cancel = context.WithCancel(ctx)
	a.server = &http.Server{
		Handler:           a.localMux(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       5 * time.Minute,
	}
	a.started = true
	a.mu.Unlock()

	// 缓存代理拿到 Agent 的长生命周期 context：门户是懒启动的（没有库启用缓存就不监听端口），
	// 但一旦打开，它必须活到进程退出 —— 用挂载请求的 ctx 会让门户在挂载返回后立刻死掉。
	a.cache.Start(a.baseCtx)

	safeGo(a.logger, "local_http_serve", func() {
		if err := a.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("本地 HTTP 服务异常退出", "error", err)
		}
	})

	a.wg.Add(1)
	safeGo(a.logger, "heartbeat", func() {
		defer a.wg.Done()
		a.runHeartbeatLoop(a.baseCtx)
	})

	// 客户端证书会话自动续期：进程常驻，界面关掉也能续（见 session_renew.go）。
	a.wg.Add(1)
	safeGo(a.logger, "session_renewal", func() {
		defer a.wg.Done()
		a.runSessionRenewalLoop(a.baseCtx)
	})

	// 无会话时用本地证书自动重连服务端：代理重启后客户端不会再推会话，缺了这条界面只会
	// 一直显示"未连接"且永不自愈（见 server_connect.go）。
	a.wg.Add(1)
	safeGo(a.logger, "server_connect", func() {
		defer a.wg.Done()
		a.runServerConnectLoop(a.baseCtx)
	})

	// 已挂载记录的会话实测：界面按钮与状态标签以**实际会话**为准，不看记录（见 session_probe.go）。
	a.wg.Add(1)
	safeGo(a.logger, "mount_session_probe", func() {
		defer a.wg.Done()
		a.runMountSessionProbeLoop(a.baseCtx)
	})

	// 上次进程死在卸载中途留下的"卸载中"记录：启动即补完 —— **不等服务端连接、不看全局
	// 自动挂载开关**（见 finishInterruptedUnmounts）。缺了这一步，那条记录会一直显示"卸载中"，
	// 重启多少次都一样（真实反馈："重启之后客户端一直提示在卸载中，这种名存实亡的"）。
	a.wg.Add(1)
	safeGo(a.logger, "finish_unmounts", func() {
		defer a.wg.Done()
		a.finishInterruptedUnmounts(a.baseCtx)
	})

	// 启动即预检本机 iSCSI 发起端（只读）：不就绪时界面会挂横幅，而不是等挂载失败才知道。
	safeGo(a.logger, "iscsi_probe", func() {
		probeCtx, cancel := context.WithTimeout(a.baseCtx, defaultProbeTimeout)
		defer cancel()
		a.refreshHostState(probeCtx)
	})
	a.wg.Add(1)
	safeGo(a.logger, "host_recheck", func() {
		defer a.wg.Done()
		a.runHostRecheckLoop(a.baseCtx)
	})

	// 启动即自愈渲染层资源目录：应用升级后旧热更层会永久失效（客户端只加载比应用版本新的层），
	// 这里自动摘掉失效指针、删掉死资源，用户不需要手工删 %ProgramData%\Vault\webapp。
	// 放后台执行：极端情况下要删几百 MB，不该拖慢本地 HTTP 服务就绪。
	safeGo(a.logger, "web_layer_reconcile", a.reconcileWebLayer)

	a.logger.Info("Vault-Agent 已启动",
		"addr", listener.Addr().String(), "admin", a.admin, "version", a.version)
	if !a.admin {
		a.logger.Warn("代理未以管理员权限运行：挂载类操作将返回 agent.admin_required")
	}
	return listener.Addr().String(), nil
}

// Stop 优雅停机：停止后台循环并关闭本地 HTTP 服务。
//
// 注意：Stop **不会**自动卸载已有挂载（避免打断用户使用）。
// 需要「先卸载再退出」请使用 UnmountAll + Stop（见 POST /agent/shutdown）。
func (a *Agent) Stop(ctx context.Context) error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = false
	if a.cancel != nil {
		a.cancel()
	}
	server := a.server
	a.mu.Unlock()

	if server != nil {
		if err := server.Shutdown(ctx); err != nil {
			a.logger.Warn("本地 HTTP 优雅停机未在超时内完成", "error", err)
			_ = server.Close()
		}
	}
	a.wg.Wait()

	// 停止本地读缓存代理：进程退出（信号 / 自更新）不走 UnmountAll，这里是 L2 落盘与
	// 代理到服务端会话 logout 的唯一时机（见 cacheproxy.go 的 Close）。
	if err := a.cache.Close(); err != nil {
		a.logger.Warn("停止本地 iSCSI 缓存代理失败", "error", err)
	}

	if err := a.store.Persist(); err != nil {
		a.logger.Warn("写入本地状态失败", "error", err)
	}
	a.logger.Info("Vault-Agent 已停止")
	return nil
}

// ActiveMountCount 返回活跃（挂载中/已挂载）的数量。
func (a *Agent) ActiveMountCount() int {
	count := 0
	for _, ms := range a.store.ListMounts() {
		if ms.State == MountStateMounted || ms.State == MountStateMounting {
			count++
		}
	}
	return count
}

// UnmountAll 逐个卸载全部挂载（用于退出流程）；单个失败只记录日志。
func (a *Agent) UnmountAll(ctx context.Context) {
	for _, ms := range a.store.ListMounts() {
		if err := a.engine.unmount(ctx, ms.AllocationID); err != nil {
			a.logger.Warn("退出时卸载失败", "allocation_id", ms.AllocationID, "error", err)
		}
	}
}

// setSession 保存**某台**服务端的会话，并触发该台的信息刷新、事件订阅与自动挂载。
//
// key 为空时按会话自身字段推导（实例 ID/地址）。primary=true 表示客户端把该台指定为主服务端
// （更新源等"没有服务端上下文"的操作默认用它，见 primaryServerKey）。
// 返回会话实际归属的服务端键（可能与传入的 key 不同：同一台的两种键会被合并）。
//
// 会话按"台"覆盖：推**同一台服务端、同一个用户**的会话（客户端每次启动、令牌续期、切回某台
// 都会推一次）不会打断这条已经建好的连接，见 sameSessionPeer。
func (a *Agent) setSession(key string, session *Session, primary bool) string {
	if session == nil {
		return ""
	}
	// 先取"推之前"的那一份：SetSession 会就地覆盖会话（还可能把地址键合并到实例 ID 键上），
	// 迟到一步就只能读到新的了。推一台全新服务端时 ResolveKey 返回空串（给了标识却没匹配
	// 上），此处就当作"没有旧会话"，不会误认成主服务端那一份。
	prevKey := a.store.ResolveKey(key, session.ServerURL)
	var prev *Session
	var hadPrev bool
	if prevKey != "" {
		prev, hadPrev = a.store.Session(prevKey)
	}

	serverKey := a.store.SetSession(key, session)
	if primary {
		a.store.SetPrimary(serverKey)
	}
	a.store.SetUser(serverKey, UserState{ID: session.UserID, Username: session.Username})
	a.setEventsSupported(serverKey, true)
	// 新会话已建立：解除"需要重新登录"标记，重新允许自动续期（只解这一台）。
	a.renewMu.Lock()
	delete(a.renewBlocked, serverKey)
	a.renewMu.Unlock()

	// 这台本来就连着、推的又是同一台服务端的同一个用户：**连接状态一个字都不动**。
	//
	// 否则会把一条正常连接打成"连接中"（Connected=false + phase=connecting），而要回到
	// connected 只能等 SystemInfo 探测或事件流重连——探测恰逢服务端忙（真实场景：刚点了
	// 删除存储库，服务端正在回收磁盘），事件流又一直没断、不会再触发"就绪即确认连接"，
	// 两条路都不走，界面就**一直卡在"连接中"**（工单原话："点删除库会卡在连接中"）。
	//
	// 代价：本机给这台改的名字/别名要等下一次真正重连（或下次启动）才会同步进代理状态。
	if cur, ok := a.store.Server(serverKey); ok && cur.Connected && hadPrev && sameSessionPeer(prev, session) {
		return serverKey
	}

	// 这里只表示"拿到会话了"，真正连上要等 onSessionEstablished 的 SystemInfo 或事件流确认，
	// 因此把阶段显式置回 connecting：若整体覆盖成零值 ServerState（Phase 归空），界面会从
	// "连接中"闪一下"未连接"（真实观感问题）。
	before := a.serverStateBefore(serverKey)
	a.store.UpdateServer(serverKey, func(state *ServerState) {
		state.URL = session.ServerURL
		state.InstanceID = session.ServerInstanceID
		state.Name = session.ServerName
		state.Alias = session.Alias
		state.Connected = false
		state.Phase = ServerPhaseConnecting
	})
	a.publishServerIfChanged(serverKey, before)

	safeGo(a.logger, "session_established", func() { a.onSessionEstablished(serverKey) })
	return serverKey
}

// sameSessionPeer 判断两份会话是不是"同一台服务端的同一个用户"。
//
// 刻意**不比令牌与有效期**：续期只是换令牌，连接本身没断（客户端续期后会把新会话推回来，
// 若按"换了令牌 = 换了会话"处理，每续期一次界面就要闪一次"连接中"）。地址与实例 ID 才是
// "同一台"的判据：两边都有值时必须一致（实例 ID 优先，地址按规范化形式比较）。
func sameSessionPeer(prev, next *Session) bool {
	if prev == nil || next == nil {
		return false
	}
	if prev.UserID != next.UserID {
		return false
	}
	if prev.ServerInstanceID != "" && next.ServerInstanceID != "" &&
		prev.ServerInstanceID != next.ServerInstanceID {
		return false
	}
	prevURL := normalizeServerURL(prev.ServerURL)
	nextURL := normalizeServerURL(next.ServerURL)
	return prevURL == "" || nextURL == "" || prevURL == nextURL
}

// onSessionEstablished 在拿到会话后刷新**该台**服务端信息、拉起事件订阅并尝试自动挂载。
func (a *Agent) onSessionEstablished(key string) {
	ctx, cancel := context.WithTimeout(a.bgContext(), sessionProbeTimeout)
	defer cancel()

	if client, err := a.serverClientFor(key); err == nil {
		clientCtx, clientCancel := context.WithTimeout(ctx, 10*time.Second)
		info, infoErr := client.SystemInfo(clientCtx)
		clientCancel()
		if infoErr != nil {
			a.setServerConnected(key, false, describeError("system_info", infoErr))
			a.logger.Warn("刷新服务端信息失败", "server", key, "error", infoErr)
		} else {
			a.updateServerFromInfo(key, info)
		}
	} else if !errors.Is(err, errNoSession()) {
		a.logger.Warn("构造服务端客户端失败", "server", key, "error", err)
	}

	a.ensureEventSubscriber(key)
	// 自动挂载必须等"服务端确认连上"（见 restoreMountsWhenConnected）：拿到会话不等于连得上，
	// 系统还没连上就挂只会留下一串失败记录。
	safeGo(a.logger, "restore_mounts", func() { a.restoreMountsWhenConnected(a.bgContext(), key) })
	// 静默热更：会话就绪即检查一次渲染层资源（客户端每次启动都会推会话）。
	//
	// 语义：完全静默 —— 后台拉清单 → 下载 → 校验 → 激活，全程只经 web_update 事件
	// 同步进度，不弹窗、不打断；客户端主进程收到"已激活"事件后会**自动切换到新资源**
	// （见 electron/main.ts 的 applyWebLayer），无需用户手动"立即重启"。
	// 只有比本机应用版本更新的资源才会被激活（见 webTargetApplies），因此重复启动
	// 只会命中"已是目标版本"的幂等分支，不重复下载。
	//
	// 多服务端：渲染层资源只能有一个来源（更新源 = 主服务端，见 docs/implementation.md §7.4.1），
	// 因此只有主服务端推来会话时才检查；否则后推会话的那台会把资源层换成它的版本。
	if key == a.store.PrimaryKey() {
		a.startWebUpdate()
	}
}

// updateServerFromInfo 用系统信息补全**某台**服务端的展示字段并标记已连接。
func (a *Agent) updateServerFromInfo(key string, info *SystemInfo) {
	before := a.serverStateBefore(key)
	a.store.UpdateServer(key, func(state *ServerState) {
		if info.ServerInstanceID != "" {
			state.InstanceID = info.ServerInstanceID
		}
		if info.ServerName != "" {
			state.Name = info.ServerName
		}
		state.Connected = true
		state.LastError = ""
		state.Phase = ServerPhaseConnected
		state.FailCount = 0
	})
	a.publishServerIfChanged(key, before)
}

// serverStateBefore 取某台服务端的状态快照（用于"变更前后比较"；不存在时为零值）。
func (a *Agent) serverStateBefore(key string) ServerState {
	state, _ := a.store.Server(key)
	return state
}

// setServerConnected 更新**某台**的"服务端连接状态"，并仅在状态真正变化时广播 server 事件。
//
// 集中在这里做"变化比较 + 发布"，避免 sse.go / lease.go 等调用点各自判断（值未变时不发，
// 防止心跳/事件重连把 server 事件刷成风暴）。
func (a *Agent) setServerConnected(key string, connected bool, lastError string) {
	before := a.serverStateBefore(key)
	a.store.SetServerConnected(key, connected, lastError)
	a.publishServerIfChanged(key, before)
}

// setServerPhase 更新某台的连接阶段并广播 server 事件（值未变时不发）。
func (a *Agent) setServerPhase(key, phase string) {
	before := a.serverStateBefore(key)
	a.store.SetServerPhase(key, phase)
	a.publishServerIfChanged(key, before)
}

// publishServerIfChanged 比较**某台**服务端的状态快照，仅在 connected / phase / fail_count /
// last_error 任一变化时广播 server 事件（事件带 server_key，界面据此只更新那一台）。
//
// 为什么把 phase 与 fail_count 也算进"变化"：界面靠它们区分"正在连"与"连不上"
// （connecting 显示转圈的"连接中"，fail_count 达上限才显示"未连接 + 重试"）。
// 只比 connected 的话，一个"没连上、但一直在重连"的代理在界面上永远不动。
func (a *Agent) publishServerIfChanged(key string, before ServerState) {
	after, ok := a.store.Server(key)
	if !ok {
		return
	}
	if before.Connected == after.Connected && before.LastError == after.LastError &&
		before.Phase == after.Phase && before.FailCount == after.FailCount {
		return
	}
	a.hub.Publish(Event{Type: "server", Data: map[string]any{
		"server_key": key,
		"url":        after.URL,
		"name":       after.Name,
		"connected":  after.Connected,
		"phase":      after.Phase,
		"fail_count": after.FailCount,
		"last_error": after.LastError,
	}})
}

// ---- 主机就绪状态（iSCSI 发起端） ----

// hostState 返回当前主机就绪状态的副本。
func (a *Agent) hostState() HostState {
	a.hostMu.RLock()
	defer a.hostMu.RUnlock()
	return a.host
}

// hostStateFromAvailability 把一次只读探测映射为主机状态（纯函数，便于单测）。
func hostStateFromAvailability(avail iscsiinitiator.Availability, probeErr error) HostState {
	now := time.Now().UnixMilli()
	if probeErr != nil {
		// 探测失败：状态未知。不猜"服务在跑"——猜错的代价是用户点挂载才发现问题。
		return HostState{
			ISCSIService: iscsiinitiator.ServiceStateUnknown,
			Reason:       HostReasonProbeFailed,
			Error:        apperr.CodeOf(probeErr),
			CheckedAt:    now,
		}
	}
	state := HostState{
		ISCSIAvailable: avail.ModuleAvailable,
		ISCSIService:   avail.ServiceState(),
		ISCSIReady:     avail.Ready(),
		CheckedAt:      now,
	}
	switch {
	case !avail.ModuleAvailable:
		state.Reason = HostReasonModuleMissing
	case !avail.Ready():
		state.Reason = HostReasonServiceStopped
	}
	return state
}

// refreshHostState 重新只读探测 iSCSI 发起端状态，并在状态变化时广播 host 事件。
//
// 只读：绝不启动服务（那是挂载路径里 EnsureService 的职责）。
func (a *Agent) refreshHostState(ctx context.Context) {
	avail, err := a.iscsi.Probe(ctx)
	a.updateHostState(hostStateFromAvailability(avail, err))
}

// markISCSIServiceRunning 在"服务已确认可运行"时直接置为就绪（不再跑 PowerShell）。
//
// 调用点：挂载链路的 EnsureService 成功之后 —— 此时 MSiSCSI 必定在运行，再探测一次
// 纯属浪费，而挂载路径对延迟敏感。
func (a *Agent) markISCSIServiceRunning() {
	a.updateHostState(HostState{
		ISCSIAvailable: true,
		ISCSIService:   iscsiinitiator.ServiceStateRunning,
		ISCSIReady:     true,
		CheckedAt:      time.Now().UnixMilli(),
	})
}

// updateHostState 写入主机状态，并在**语义字段**变化时广播 host 事件。
//
// CheckedAt 每次探测都会变，不参与比较（否则会每 30s 白推一条事件）。
func (a *Agent) updateHostState(next HostState) {
	a.hostMu.Lock()
	prev := a.host
	a.host = next
	a.hostMu.Unlock()

	changed := prev.ISCSIAvailable != next.ISCSIAvailable ||
		prev.ISCSIService != next.ISCSIService ||
		prev.ISCSIReady != next.ISCSIReady ||
		prev.Reason != next.Reason ||
		prev.Error != next.Error
	if !changed {
		return
	}
	if next.ISCSIReady {
		a.logger.Info("本机 iSCSI 发起端已就绪", "service", next.ISCSIService)
	} else {
		a.logger.Warn("本机 iSCSI 发起端未就绪：挂载类操作将失败",
			"reason", next.Reason, "service", next.ISCSIService, "error", next.Error)
	}
	a.hub.Publish(Event{Type: "host", Data: next})
}

// runHostRecheckLoop 在未就绪期间低频复检（就绪后不再探测）。
//
// 目的：管理员按提示 `Start-Service MSiSCSI` 后，界面横幅能自动消失 —— 而不是
// "重启客户端才消失"（这类提示最容易变成噪音）。
func (a *Agent) runHostRecheckLoop(ctx context.Context) {
	ticker := time.NewTicker(hostProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if a.hostState().ISCSIReady {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
		a.refreshHostState(probeCtx)
		cancel()
	}
}

// ensureEventSubscriber 确保**该台**服务端有一个事件订阅协程在运行（每台各自一条 SSE）。
func (a *Agent) ensureEventSubscriber(key string) {
	if key == "" {
		return
	}
	a.subMu.Lock()
	if a.subRunning[key] || a.eventsUnsupported[key] {
		a.subMu.Unlock()
		return
	}
	a.subRunning[key] = true
	a.subMu.Unlock()

	safeGo(a.logger, "server_events", func() {
		defer func() {
			a.subMu.Lock()
			delete(a.subRunning, key)
			a.subMu.Unlock()
		}()
		a.runServerEvents(a.bgContext(), key)
	})
}

// eventsUnsupportedBy 返回该台服务端是否已确认不支持事件流。
func (a *Agent) eventsUnsupportedBy(key string) bool {
	a.subMu.Lock()
	defer a.subMu.Unlock()
	return a.eventsUnsupported[key]
}

// setEventsSupported 记录该台服务端对事件流的支持情况（false 即停止订阅，见 sse.go）。
func (a *Agent) setEventsSupported(key string, supported bool) {
	a.subMu.Lock()
	defer a.subMu.Unlock()
	if supported {
		delete(a.eventsUnsupported, key)
		return
	}
	a.eventsUnsupported[key] = true
}

// serverClientFor 返回**指定服务端**的客户端；该台没有会话时返回 agent.no_session。
//
// 返回的客户端带"会话失效即重登录"的兜底：服务端重启会清空内存会话表，此后所有带令牌的
// 请求都会 401；有本地证书身份时会自动用证书换一个新会话并重试一次（见 serverClient.do 与
// Agent.refreshSessionTokenFor）—— 重登录必须发回**同一台**，否则会拿 A 的会话去顶 B。
func (a *Agent) serverClientFor(key string) (*serverClient, error) {
	session, ok := a.store.Session(key)
	if !ok || strings.TrimSpace(session.ServerURL) == "" {
		return nil, errNoSession()
	}
	client, err := newServerClient(session.ServerURL, session.Token, session.CertSHA256, a.logger)
	if err != nil {
		return nil, err
	}
	resolved := key
	if resolved == "" {
		resolved = a.store.ResolveKey("", session.ServerURL)
	}
	return client.withAuthRetry(func(ctx context.Context) (string, error) {
		return a.refreshSessionTokenFor(ctx, resolved)
	}), nil
}

// primaryServerKey 返回主服务端键：更新源、自更新产物下载等"没有服务端上下文"的操作用它。
//
// 都没有时返回空串（调用方按"无会话"处理）。
func (a *Agent) primaryServerKey() string {
	return a.store.PrimaryKey()
}

// primaryServerClient 返回主服务端的客户端（无主服务端时退化为"唯一的那台"，再没有就报无会话）。
func (a *Agent) primaryServerClient() (*serverClient, error) {
	return a.serverClientFor(a.primaryServerKey())
}

// serverDisplayName 返回**某台**服务端的展示名（无名称时退回 URL）。
func (a *Agent) serverDisplayName(key string) string {
	server, ok := a.store.Server(key)
	if !ok {
		return ""
	}
	if strings.TrimSpace(server.Name) != "" {
		return server.Name
	}
	return server.URL
}

// bgContext 返回后台循环使用的上下文（未启动时退化为 Background）。
func (a *Agent) bgContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.baseCtx != nil {
		return a.baseCtx
	}
	return context.Background()
}

// isElevated 判断当前进程是否以管理员（提升）权限运行。
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

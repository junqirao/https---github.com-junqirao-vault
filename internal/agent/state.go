package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// 挂载状态（见 docs/agent-api.md MountState.state）。
const (
	// MountStateMounting 挂载中。
	MountStateMounting = "mounting"
	// MountStateMounted 已挂载。
	MountStateMounted = "mounted"
	// MountStateUnmounting 卸载中。
	MountStateUnmounting = "unmounting"
	// MountStateError 出错（如心跳连续失败超过租约 TTL）。
	MountStateError = "error"
	// MountStateRevoked 被服务端踢下线。
	MountStateRevoked = "revoked"
)

// 挂载阶段（MountState.Phase）：state 是粗粒度结果，phase 是挂载**过程中**的细粒度进度。
//
// 取值必须与 internal/app 的 MountPhase*（服务端侧）以及 docs/agent-api.md「挂载阶段」一致：
//   - 服务端侧阶段（allocating / preparing_disk / configuring_target）由服务端在该同步
//     请求内发事件（SSE event: mount），代理订阅后转发（见 handleServerEvent）；
//   - 本机阶段（connecting / online / mount_point / post_script）由挂载引擎直接写入。
//
// 为什么要做这个：挂载在服务端是同步重活（等差异盘 + PowerShell 下发 iSCSI 目标，
// 实测数十秒），期间界面只有一个转圈的按钮，用户无从知道"卡在哪一步"
// （真实反馈：希望看到"正在创建虚拟磁盘 / 正在创建 iSCSI 目标"）。
const (
	// MountPhaseRequesting 已向服务端申请挂载，正在等服务端受理。
	MountPhaseRequesting = "requesting"
	// MountPhaseAllocating 服务端正在分配资源（进入发布流程）。
	MountPhaseAllocating = "allocating"
	// MountPhasePreparingDisk 服务端正在准备虚拟磁盘（等差异盘/VHDX 就绪）。
	MountPhasePreparingDisk = "preparing_disk"
	// MountPhaseConfiguringTarget 服务端正在创建/下发 iSCSI 目标（PowerShell，最慢的一步）。
	MountPhaseConfiguringTarget = "configuring_target"
	// MountPhaseConnecting 本机正在建立 iSCSI 会话（服务/门户/CHAP）。
	MountPhaseConnecting = "connecting"
	// MountPhaseOnline 本机正在让磁盘上线（去离线、去只读）。
	MountPhaseOnline = "online"
	// MountPhaseMountPoint 本机正在挂载到盘符/目录。
	MountPhaseMountPoint = "mount_point"
	// MountPhasePostScript 本机正在执行后置脚本。
	MountPhasePostScript = "post_script"
)

// mountPhaseRank 给阶段定序，保证阶段只前进不回退。
//
// 服务端阶段事件经 SSE 异步到达，可能晚于本机阶段（例如 configuring_target 在本地已进入
// connecting 之后才被消费）。没有定序时界面会出现"阶段倒着走"的闪烁。
func mountPhaseRank(phase string) int {
	switch phase {
	case MountPhaseRequesting:
		return 1
	case MountPhaseAllocating:
		return 2
	case MountPhasePreparingDisk:
		return 3
	case MountPhaseConfiguringTarget:
		return 4
	case MountPhaseConnecting:
		return 5
	case MountPhaseOnline:
		return 6
	case MountPhaseMountPoint:
		return 7
	case MountPhasePostScript:
		return 8
	default:
		return 0
	}
}

// 主机不就绪的稳定原因码（HostState.Reason），界面据此给出对应的可执行建议。
const (
	// HostReasonModuleMissing 本机没有 iSCSI 发起程序（IscsiInitiator 模块/cmdlet 缺失）。
	HostReasonModuleMissing = "module_missing"
	// HostReasonServiceStopped iSCSI 发起程序服务（MSiSCSI）已安装但未运行。
	HostReasonServiceStopped = "service_stopped"
	// HostReasonProbeFailed 探测本身失败（如 PowerShell 不可用），状态未知。
	HostReasonProbeFailed = "probe_failed"
)

// HostState 是代理所在主机的就绪状态（当前只有 iSCSI 发起端）。
//
// 为什么要有它：MSiSCSI 未运行时挂载必定失败在 connect 阶段，而界面当时只能显示
// "挂载失败（阶段：connect）"——用户拿到的信息没有任何可执行性（真实反馈：应该在
// 客户端启动时就提示）。把只读探测结果提前暴露，界面即可在启动时挂横幅。
type HostState struct {
	// ISCSIAvailable 是否找得到 iSCSI 发起端（模块 + cmdlet）。
	ISCSIAvailable bool `json:"iscsi_available"`
	// ISCSIService 归一化服务状态：running / stopped / unknown。
	ISCSIService string `json:"iscsi_service"`
	// ISCSIReady 现在能否发起点 iSCSI 连接（模块可用且服务在运行）。
	ISCSIReady bool `json:"iscsi_ready"`
	// Reason 不就绪的稳定原因码（见 HostReason*）；就绪时为空。
	Reason string `json:"reason,omitempty"`
	// Error 探测失败时的稳定错误码（如 system.unavailable）。
	Error string `json:"error,omitempty"`
	// CheckedAt 最近一次探测时间（毫秒时间戳）。
	CheckedAt int64 `json:"checked_at,omitempty"`
}

// Session 是前端推送的服务端会话。
//
// ⚠️ Token 只在内存中保留，绝不落盘、绝不写日志。
type Session struct {
	// ServerURL 服务端根地址，如 https://10.0.0.1:8443。
	ServerURL string `json:"server_url"`
	// ServerInstanceID 服务端实例 ID。
	ServerInstanceID string `json:"server_instance_id"`
	// ServerName 服务端名称。
	ServerName string `json:"server_name"`
	// Alias 是客户端为**本机**设置的服务端别名（可选）。
	//
	// 用途：目录模式的挂载点命名 `<服务端别名>_<存储库名称>`（见 mountEngine.serverAlias）。
	// 多服务端下每台各自带自己的别名，否则两台服务端的挂载目录会撞进同一个命名空间。
	// 空串时退回服务端名称。
	Alias string `json:"alias,omitempty"`
	// Token 会话令牌（Bearer）。
	Token string `json:"token"`
	// CertSHA256 服务端证书 DER 的 SHA-256（小写十六进制）。
	//
	// 服务端使用自建 CA，首次连接无法用公信 CA 验证，因此由前端在连通性探测成功后
	// 把探测到的指纹随会话一起推来，代理据此**固定**该指纹（TOFU），
	// 后续心跳/挂载/事件流都按指纹校验服务端证书。为空时退回默认的链路校验。
	CertSHA256 string `json:"cert_sha256,omitempty"`
	// UserID / Username 当前登录用户。
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	// ExpiresAt 会话过期时间（毫秒时间戳）。
	ExpiresAt int64 `json:"expires_at"`
	// receivedAt 是本地收到该会话（或续期成功写回）的时刻（毫秒时间戳）。
	//
	// 仅供代理内部计算"观测到的会话寿命"（用于自动续期窗口），不序列化、不对外暴露。
	receivedAt int64
}

// 服务端连接的阶段（ServerState.Phase）。
//
// 为什么需要它：界面此前只有 connected 这个布尔量，于是"代理正在连/重连"与"真的连不上"
// 长得一模一样 —— 进程刚起来、会话还没推来、正在重连时，界面都显示"未连接"，用户读到的是
// "连不上服务端"（真实反馈）。把阶段显式暴露，界面才能在连接中显示转圈的"连接中"。
const (
	// ServerPhaseConnecting 正在连接/重连服务端（界面显示"连接中"，不显示"未连接"）。
	ServerPhaseConnecting = "connecting"
	// ServerPhaseConnected 已连接。
	ServerPhaseConnected = "connected"
	// ServerPhaseDisconnected 未连接，且自动重连次数已用尽（等界面手动重试）。
	ServerPhaseDisconnected = "disconnected"
)

// maxServerConnectAttempts 是"无会话时自动重连"的连续失败上限。
//
// 达到上限即停手并把 Phase 置为 disconnected（界面显示"未连接" + 手动重试按钮）：
// 无休止重试只会把日志刷满，用户也永远分不清"还在连"和"连不上"（真实诉求：
// "连接失败 2 次之后不再重试，边上加一个按钮让用户手动重试，手动重试会刷新计数"）。
const maxServerConnectAttempts = 2

// ServerState 描述代理连接的一台服务端。
//
// 多服务端下代理同时维护多份：每台一份（键见 stateStore.servers），互不影响。
type ServerState struct {
	URL        string `json:"url"`
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	// Alias 是客户端下发的本机侧别名（见 Session.Alias）；目录命名优先用它。
	Alias     string `json:"alias,omitempty"`
	Connected bool   `json:"connected"`
	// Phase 是连接阶段（见 ServerPhase*）：connecting / connected / disconnected。
	Phase string `json:"phase,omitempty"`
	// FailCount 是**连续**连接失败次数（任意一次成功即归零）。
	//
	// 达到 maxServerConnectAttempts 后代理不再自动重连，等界面手动重试
	// （POST /agent/server/reconnect 会把计数清零并立即重试一次）。
	FailCount int    `json:"fail_count,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// UserState 描述当前登录用户。
type UserState struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// MountState 是单个分配的挂载状态（对外契约类型）。
type MountState struct {
	RepoID       string `json:"repo_id"`
	RepoName     string `json:"repo_name"`
	AllocationID string `json:"allocation_id"`
	LeaseID      string `json:"lease_id"`
	TargetIQN    string `json:"target_iqn"`
	Portal       string `json:"portal"`
	// ServerKey / ServerURL / ServerName 标识这条挂载属于**哪台**服务端。
	//
	// 多服务端下这是必须的：心跳、挂载点回写、租约释放都必须发给"当初受理这次挂载的那台"
	// （见 Agent.serverClientFor）。ServerKey 与身份存储、前端 ServerEntry.key 同算法：
	// 实例 ID 优先，其次规范化地址（见 serverKeyOf）。
	//
	// ⚠️ 为空只可能是"单服务端时代留下的旧记录"：那时会被归给唯一的那台（见 NewStateStore），
	// 运行期未归类的记录才按主服务端兜底（见 Agent.mountClient）。
	ServerKey  string `json:"server_key,omitempty"`
	ServerURL  string `json:"server_url,omitempty"`
	ServerName string `json:"server_name,omitempty"`
	MountMode  string `json:"mount_mode"`
	MountPath  string `json:"mount_path"`
	DiskNumber int    `json:"disk_number,omitempty"`
	State      string `json:"state"`
	// Phase 是 state=mounting 期间的细粒度阶段（见 MountPhase* 常量）；非挂载中为空。
	// 界面据此显示"服务端正在创建 iSCSI 目标"这类即时反馈。
	Phase           string `json:"phase,omitempty"`
	MountedAt       int64  `json:"mounted_at,omitempty"`
	LastHeartbeatAt int64  `json:"last_heartbeat_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	// SessionActive 是**实测**的本机 iSCSI 会话状态：本机发起端当前是否还有到
	// TargetIQN 的活动连接（Get-IscsiSession 里 IsConnected 为真的会话）。
	//
	// 为什么记录里要存"实测值"：state 只是**代理的意图/记录**，磁盘是否真的挂着由会话决定。
	// 会话被系统或用户断开（服务重启、网络长时间中断、手动在 iSCSI 发起程序里断开）后，
	// 记录仍写着 mounted —— 界面于是显示"已挂载"并给出"卸载"按钮，而用户既用不了这块盘、
	// 点卸载还会因为"挂载点不存在"失败（真实诉求："挂载和卸载的按钮应以实际为准，
	// 看对应的 iSCSI 连接是否在活动中"）。
	//
	// ⚠️ 只有**实测为真**时才是 true；探测失败一律不猜（保持原值，见 probeMountSession）。
	SessionActive bool `json:"session_active,omitempty"`
	// SessionCheckedAt 是最近一次**得出结论**的实测时刻（毫秒时间戳）。
	//
	// 只在结论变化时刷新（每 20s 一轮都写盘没必要，见 recordSessionProbe）。
	// 0 表示**尚未核对**（代理刚启动、记录刚从状态文件加载）：界面此时不应把
	// session_active=false 当成"断线"，而应按记录状态展示 —— 见 docs/agent-api.md。
	SessionCheckedAt int64 `json:"session_checked_at,omitempty"`
	// LastErrorDetail 是最近一次失败的**原始报错文本**（如 PowerShell 脚本自报的 .NET 异常），
	// 供本机诊断展示（见 mountErrorDetailOf 的说明：这是"原始报错只进日志"的刻意例外，
	// 只出现在本状态里，不进任何 API 错误响应）。
	LastErrorDetail string `json:"last_error_detail,omitempty"`
}

// mountRuntime 是挂载的运行时信息，**只在内存中保留**。
//
// 其中包含 CHAP 密钥：绝不写入状态文件、绝不写日志；卸载后随挂载状态一并清除。
type mountRuntime struct {
	PortalAddress string
	PortalPort    int
	AuthMode      string
	ChapUser      string
	ChapSecret    string
	PostScript    string

	HeartbeatSeconds int
	LeaseTTLSeconds  int

	HeartbeatFailCount int
	HeartbeatFailSince int64
	NextHeartbeatAt    int64
	// SessionProbeFailures 是会话实测连续失败次数（只在内存中）。
	//
	// 用途：探测走一次 PowerShell，宿主机上 PowerShell 出问题时会**每轮**都失败；
	// 只在首次失败与恢复时各记一条日志，避免 20s 一次的报错把日志刷满。
	SessionProbeFailures int
	// HeartbeatError 表示当前 state=error 是"心跳连续失败超过 TTL"造成的（磁盘其实还挂着）。
	//
	// 它把这类 error 与"挂载失败后保留的 error 记录"区分开：前者要继续发心跳（服务端恢复后
	// 自动回到 mounted），后者不该被心跳误复位。
	HeartbeatError bool

	DiskNumber int
	DiskKnown  bool
}

// heartbeatInterval 返回心跳间隔（服务端未下发时默认 30s）。
func (r *mountRuntime) heartbeatInterval() time.Duration {
	if r.HeartbeatSeconds > 0 {
		return time.Duration(r.HeartbeatSeconds) * time.Second
	}
	return defaultHeartbeatSeconds
}

// leaseTTL 返回租约 TTL（服务端未下发时默认 120s）。
func (r *mountRuntime) leaseTTL() time.Duration {
	if r.LeaseTTLSeconds > 0 {
		return time.Duration(r.LeaseTTLSeconds) * time.Second
	}
	return defaultLeaseTTL
}

// persistedServer 是状态文件里**一台**服务端的持久化形态（不含会话令牌）。
type persistedServer struct {
	Key   string      `json:"key"`
	State ServerState `json:"state"`
	User  UserState   `json:"user"`
}

// persistedState 是本地状态文件的形状。
//
// 注意：只持久化「服务端展示信息 + 挂载状态」，不含会话令牌与 CHAP 密钥。
type persistedState struct {
	ClientID string `json:"client_id"`
	// PrimaryKey 是主服务端（更新源等"没有服务端上下文"的操作用它，见 Agent.primaryServerKey）。
	PrimaryKey string            `json:"primary_key,omitempty"`
	Servers    []persistedServer `json:"servers,omitempty"`
	// Server / User 是单服务端时代的字段，**只用于读取旧文件**：读到即迁移进 Servers。
	Server *ServerState `json:"server,omitempty"`
	User   *UserState   `json:"user,omitempty"`
	Mounts []MountState `json:"mounts"`
}

// serverEntry 是**一台**服务端的本地状态：连接状态 + 登录用户 + 会话（会话仅内存）。
type serverEntry struct {
	state   ServerState
	user    UserState
	session *Session
}

// ServerSnapshot 是一台服务端的完整状态快照（对外只读）。
type ServerSnapshot struct {
	Key     string
	State   ServerState
	User    UserState
	Session *Session
}

// stateStore 是代理的内存态 + 本地持久化。
//
// 多服务端：servers 里每台一份（键见 serverKeyOf），挂载状态按 MountState.ServerKey 归属到台。
// 这是"多服务端各自自动登录、各自自动挂载"的存储前提（见 docs/implementation.md §3.4.1）。
type stateStore struct {
	path   string
	logger *slog.Logger

	mu       sync.Mutex
	clientID string
	// servers 各服务端状态；primary 是主服务端键（可能为空：还没有任何服务端）。
	servers map[string]*serverEntry
	primary string
	mounts  map[string]*MountState
	runtime map[string]*mountRuntime
}

// sameServerRef 判断两组"服务端引用"（实例 ID + 地址）是否指向同一个服务端实例。
//
// 两边都有实例 ID 时以实例 ID 为准；否则退回规范化地址比较（与 sameServer 同口径）。
func sameServerRef(instanceID, serverURL, otherInstanceID, otherURL string) bool {
	mine := strings.ToLower(strings.TrimSpace(instanceID))
	other := strings.ToLower(strings.TrimSpace(otherInstanceID))
	if mine != "" && other != "" {
		return mine == other
	}
	url := normalizeServerURL(serverURL)
	return url != "" && normalizeServerURL(otherURL) == url
}

// NewStateStore 加载本地状态；文件不存在时新建（并生成稳定的 client_id）。
func NewStateStore(path string, logger *slog.Logger) (*stateStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if strings.TrimSpace(path) == "" {
		path = DefaultStatePath()
	}
	s := &stateStore{
		path:    path,
		logger:  logger,
		servers: make(map[string]*serverEntry),
		mounts:  make(map[string]*MountState),
		runtime: make(map[string]*mountRuntime),
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var loaded persistedState
		if err := json.Unmarshal(data, &loaded); err != nil {
			logger.Warn("本地状态文件无法解析，已忽略其内容", "path", path, "error", err)
		} else {
			s.clientID = loaded.ClientID
			legacyKey := s.loadServersLocked(&loaded)
			for i := range loaded.Mounts {
				m := loaded.Mounts[i]
				if m.AllocationID == "" {
					continue
				}
				// 上次进程实测的会话状态一律作废：进程重启不等于会话消失（iSCSI 会话归操作
				// 系统管），沿用旧结论会在两个方向上撒谎。复位成"尚未核对"，由会话实测
				// 循环（约 20s 内）或启动时的自动挂载写入真实值。
				m.SessionActive = false
				m.SessionCheckedAt = 0
				// 旧文件（单服务端时代）的挂载记录没有服务端归属：归给当时唯一的那台。
				// 不归类的话多服务端下心跳会按"主服务端"兜底发出去 —— 发给了根本没受理过
				// 这次挂载的服务端（见 Agent.mountClient）。
				if legacyKey != "" && strings.TrimSpace(m.ServerKey) == "" {
					m.ServerKey = legacyKey
				}
				// 归属三件套（键/地址/名称）补齐：挂载记录自带服务端标识，界面与日志直接可读，
				// 不必再回查状态表（旧记录里这三样都缺）。
				if m.ServerKey == legacyKey {
					if entry := s.servers[legacyKey]; entry != nil {
						if m.ServerURL == "" {
							m.ServerURL = entry.state.URL
						}
						if m.ServerName == "" {
							m.ServerName = entry.state.Name
						}
					}
				}
				s.mounts[m.AllocationID] = &m
			}
		}
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("agent: 读取本地状态失败: %w", err)
	}

	if s.clientID == "" {
		s.clientID = uuid.NewString()
		if err := s.persistLocked(); err != nil {
			logger.Warn("写入本地状态失败（将仅使用内存状态）", "path", path, "error", err)
		}
	}
	return s, nil
}

// loadServersLocked 把状态文件里的服务端装载进内存（连接状态复位），并确定主服务端。
//
// 返回"旧格式挂载记录可归属的唯一服务端键"：文件里恰好一台时才有值，否则为空（那说明文件
// 本来就带归属信息，或压根没有服务端）。
func (s *stateStore) loadServersLocked(loaded *persistedState) string {
	entries := loaded.Servers
	if len(entries) == 0 && loaded.Server != nil {
		// 单服务端时代的文件：一份 server/user 迁移成一台。
		user := UserState{}
		if loaded.User != nil {
			user = *loaded.User
		}
		entries = []persistedServer{{
			Key:   serverKeyOf(loaded.Server.InstanceID, loaded.Server.URL),
			State: *loaded.Server,
			User:  user,
		}}
	}
	for i := range entries {
		item := entries[i]
		key := strings.TrimSpace(item.Key)
		if key == "" {
			key = serverKeyOf(item.State.InstanceID, item.State.URL)
		}
		if key == "" {
			continue
		}
		state := item.State
		// 进程重启后真实会话已不存在：连接状态一律复位。
		// 阶段复位成"连接中"而不是"未连接"：进程刚起来还没试过，界面该显示转圈的
		// "连接中"；真试过 maxServerConnectAttempts 次都失败才轮到"未连接"（真实诉求）。
		state.Connected = false
		state.Phase = ServerPhaseConnecting
		state.FailCount = 0
		state.LastError = ""
		s.servers[key] = &serverEntry{state: state, user: item.User}
	}

	legacyKey := ""
	if len(s.servers) == 1 {
		for k := range s.servers {
			legacyKey = k
		}
	}
	s.primary = strings.TrimSpace(loaded.PrimaryKey)
	if s.primary == "" || s.servers[s.primary] == nil {
		// 主服务端没记（或记的那台已经没了）：单台时就是它自己；多台时先留空，
		// 由第一台推来会话的服务端顶上（见 SetSession）。
		s.primary = legacyKey
	}
	return legacyKey
}

// Path 返回状态文件路径。
func (s *stateStore) Path() string { return s.path }

// ClientID 返回全局唯一且稳定的客户端标识（不使用 MAC/IP）。
func (s *stateStore) ClientID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientID
}

// entryLocked 返回（必要时创建）某台服务端的内存条目；键为空时返回 nil（调用方跳过即可）。
func (s *stateStore) entryLocked(key string) *serverEntry {
	if key == "" {
		return nil
	}
	entry := s.servers[key]
	if entry == nil {
		// 阶段先按"连接中"起步：这台刚登记、一次都还没试过，留空会被界面读成"未连接"
		// 并挂出手动重试按钮（与首次运行同一条零值路径）。
		entry = &serverEntry{state: ServerState{Phase: ServerPhaseConnecting}}
		s.servers[key] = entry
	}
	return entry
}

// primaryKeyLocked 返回主服务端键；主服务端没登记时退回"唯一的那台"，都没有则为空串。
func (s *stateStore) primaryKeyLocked() string {
	if s.primary != "" && s.servers[s.primary] != nil {
		return s.primary
	}
	if len(s.servers) == 1 {
		for key := range s.servers {
			return key
		}
	}
	return ""
}

// orderedKeysLocked 返回服务端键（主服务端排最前，其余按键排序，保证输出稳定）。
func (s *stateStore) orderedKeysLocked() []string {
	keys := make([]string, 0, len(s.servers))
	for key := range s.servers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if primary := s.primaryKeyLocked(); primary != "" {
		for i, key := range keys {
			if key != primary {
				continue
			}
			keys = append(keys[:i], keys[i+1:]...)
			keys = append([]string{primary}, keys...)
			break
		}
	}
	return keys
}

// matchKeyLocked 在已知服务端里找与"键、实例 ID 或地址"匹配的条目，返回其键（找不到为空串）。
func (s *stateStore) matchKeyLocked(reference string) string {
	ref := strings.TrimSpace(reference)
	if ref == "" {
		return ""
	}
	if _, ok := s.servers[ref]; ok {
		return ref
	}
	for existing, entry := range s.servers {
		if strings.EqualFold(strings.TrimSpace(entry.state.InstanceID), ref) ||
			normalizeServerURL(entry.state.URL) == normalizeServerURL(ref) {
			return existing
		}
	}
	return ""
}

// resolveExistingKeyLocked 把键解析成已登记的条目键（空键 -> 主服务端），找不到返回空串。
func (s *stateStore) resolveExistingKeyLocked(key string) string {
	if k := strings.TrimSpace(key); k != "" {
		return s.matchKeyLocked(k)
	}
	return s.primaryKeyLocked()
}

// SetSession 保存**某台**服务端的会话（仅内存），返回该会话实际归属的服务端键。
//
// key 为空时按会话字段推导（实例 ID 优先，其次规范化地址，见 serverKeyOf）。同一台若已存在
// "另一把键"，这里会合并成一把（见 rekeyLocked）。
func (s *stateStore) SetSession(key string, session *Session) string {
	if session == nil {
		s.ClearSession(key)
		return key
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionKey := s.rekeyLocked(s.resolveSessionKeyLocked(key, session), session)
	entry := s.entryLocked(sessionKey)
	if entry == nil {
		return sessionKey
	}
	cp := *session
	cp.receivedAt = time.Now().UnixMilli()
	entry.session = &cp
	// 会话里带的服务端信息同步进状态（名称/别名等）；连接状态不动 —— 那要等真实探活
	// （Agent.onSessionEstablished）之后才作数。
	if session.ServerURL != "" {
		entry.state.URL = session.ServerURL
	}
	if session.ServerInstanceID != "" {
		entry.state.InstanceID = session.ServerInstanceID
	}
	if session.ServerName != "" {
		entry.state.Name = session.ServerName
	}
	if session.Alias != "" {
		entry.state.Alias = session.Alias
	}
	if s.primary == "" || s.servers[s.primary] == nil {
		s.primary = sessionKey
	}
	_ = s.persistLocked()
	return sessionKey
}

// Session 返回指定服务端的会话副本；未推送时 ok=false。key 为空时取主服务端。
func (s *stateStore) Session(key string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil || entry.session == nil {
		return nil, false
	}
	cp := *entry.session
	return &cp, true
}

// Sessions 返回全部会话副本（服务端键 -> 会话），供逐台续期/订阅使用。
func (s *stateStore) Sessions() map[string]*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*Session, len(s.servers))
	for key, entry := range s.servers {
		if entry.session == nil {
			continue
		}
		cp := *entry.session
		out[key] = &cp
	}
	return out
}

// resolveSessionKeyLocked 算出会话应落到的键：显式键优先（能匹配既有条目就复用），
// 否则按会话自身的实例 ID/地址推导（见 serverKeyOf）。
func (s *stateStore) resolveSessionKeyLocked(key string, session *Session) string {
	if k := strings.TrimSpace(key); k != "" {
		if matched := s.matchKeyLocked(k); matched != "" {
			return matched
		}
		return k
	}
	return serverKeyOf(session.ServerInstanceID, session.ServerURL)
}

// rekeyLocked 保证"同一台服务端只有一把键"：把指向同一台的其它条目并进 key。
//
// 键有两种来源：实例 ID 与规范化地址。客户端先只知道地址、拿到 system/info 之后才知道实例 ID，
// 于是同一台可能先以地址为键、后以实例 ID 为键出现。不合并的话同一台在 servers 里会有两份，
// 挂载归属与心跳各认一半。
func (s *stateStore) rekeyLocked(key string, session *Session) string {
	if key == "" {
		return key
	}
	for old, entry := range s.servers {
		if old == key {
			continue
		}
		if !sameServerRef(session.ServerInstanceID, session.ServerURL, entry.state.InstanceID, entry.state.URL) {
			continue
		}
		s.mergeEntryLocked(old, key)
	}
	return key
}

// mergeEntryLocked 把 from 这台服务端的本地状态并进 to（挂载归属一并改键），然后删掉 from。
//
// 只在两者确认为同一台服务端时调用（见 rekeyLocked）；已存在的字段优先保留 to 的。
func (s *stateStore) mergeEntryLocked(from, to string) {
	src := s.servers[from]
	if from == to || src == nil {
		return
	}
	dst := s.entryLocked(to)
	if dst == nil {
		return
	}
	if dst.state.URL == "" {
		dst.state.URL = src.state.URL
	}
	if dst.state.InstanceID == "" {
		dst.state.InstanceID = src.state.InstanceID
	}
	if dst.state.Name == "" {
		dst.state.Name = src.state.Name
	}
	if dst.state.Alias == "" {
		dst.state.Alias = src.state.Alias
	}
	if dst.state.LastError == "" {
		dst.state.LastError = src.state.LastError
	}
	if dst.user.ID == "" {
		dst.user = src.user
	}
	if dst.session == nil {
		dst.session = src.session
	}
	delete(s.servers, from)
	for _, ms := range s.mounts {
		if ms.ServerKey == from {
			ms.ServerKey = to
		}
	}
	if s.primary == from {
		s.primary = to
	}
}

// ClearSession 清除**某台**服务端的会话（退出登录/移除该台），并把该台连接状态复位。
//
// 刻意把失败计数顶到上限：退出登录后本地证书身份**仍在**，若让自动重连循环继续跑，它会
// 立刻用证书把人"登回来"，用户看到的是"登出没生效"。顶到上限后阶段即为 disconnected，
// 自动重连停手，只有界面手动重试（POST /agent/server/reconnect）才会重新连接。
func (s *stateStore) ClearSession(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil {
		return
	}
	entry.session = nil
	entry.state.Connected = false
	entry.state.LastError = ""
	entry.state.FailCount = maxServerConnectAttempts
	entry.state.Phase = ServerPhaseDisconnected
	_ = s.persistLocked()
}

// ClearAllSessions 清除全部服务端会话（客户端"退出登录"）。
//
// 服务端条目本身保留：身份（客户端证书）是本机的，删条目会让下次启动丢掉"这台是谁"，
// 而退出登录只是不再持有令牌（见 ClearSession 注释）。
func (s *stateStore) ClearAllSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.servers {
		entry.session = nil
		entry.state.Connected = false
		entry.state.LastError = ""
		entry.state.FailCount = maxServerConnectAttempts
		entry.state.Phase = ServerPhaseDisconnected
	}
	_ = s.persistLocked()
}

// RemoveServer 移除一台服务端的本地状态（客户端删掉该服务端条目时调用）。
//
// 挂载记录**保留**：盘还挂在本机，仍要能卸载、能回写挂载点；只是没人给它发心跳了
// （它归属的那台已经没有会话），租约到期由服务端自行回收。
func (s *stateStore) RemoveServer(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved := s.resolveExistingKeyLocked(key)
	if resolved == "" {
		return
	}
	delete(s.servers, resolved)
	if s.primary == resolved {
		s.primary = ""
		for existing := range s.servers {
			s.primary = existing
			break
		}
	}
	_ = s.persistLocked()
}

// ServerKeys 返回已知服务端的键（主服务端在前）。
func (s *stateStore) ServerKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.orderedKeysLocked()
}

// PrimaryKey 返回主服务端键（没有则为空串）。
func (s *stateStore) PrimaryKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.primaryKeyLocked()
}

// SetPrimary 指定主服务端（键没登记时忽略）。
func (s *stateStore) SetPrimary(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved := s.matchKeyLocked(key)
	if resolved == "" || resolved == s.primary {
		return
	}
	s.primary = resolved
	_ = s.persistLocked()
}

// ResolveKey 把外部给的"服务端引用"（键或地址）映射成代理内部的服务端键。
//
// 两者都为空时返回主服务端键（老客户端不带服务端标识时的兜底）；给了却没匹配上则返回空串，
// 由调用方决定是报错还是兜底。
func (s *stateStore) ResolveKey(key, serverURL string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if matched := s.matchKeyLocked(key); matched != "" {
		return matched
	}
	if matched := s.matchKeyLocked(serverURL); matched != "" {
		return matched
	}
	if strings.TrimSpace(key) != "" || strings.TrimSpace(serverURL) != "" {
		return ""
	}
	return s.primaryKeyLocked()
}

// Servers 返回全部服务端状态快照（主服务端在前）。
func (s *stateStore) Servers() []ServerSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ServerSnapshot, 0, len(s.servers))
	for _, key := range s.orderedKeysLocked() {
		out = append(out, s.snapshotLocked(key))
	}
	return out
}

// snapshotLocked 组装一台服务端的状态快照（调用方持锁）。
func (s *stateStore) snapshotLocked(key string) ServerSnapshot {
	entry := s.servers[key]
	if entry == nil {
		return ServerSnapshot{Key: key}
	}
	snap := ServerSnapshot{Key: key, State: entry.state, User: entry.user}
	if entry.session != nil {
		cp := *entry.session
		snap.Session = &cp
	}
	return snap
}

// Server 返回指定服务端的状态；不存在时 ok=false。key 为空时取主服务端。
func (s *stateStore) Server(key string) (ServerState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil {
		return ServerState{}, false
	}
	return entry.state, true
}

// SetServer 覆盖某台服务端状态并持久化（键为空时按主服务端处理；键不存在则新建）。
func (s *stateStore) SetServer(key string, state ServerState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if resolved := s.resolveExistingKeyLocked(key); resolved != "" {
		key = resolved
	}
	entry := s.entryLocked(key)
	if entry == nil {
		return
	}
	entry.state = state
	_ = s.persistLocked()
}

// UpdateServer 在锁内变更某台服务端状态并持久化；键不存在时忽略。
func (s *stateStore) UpdateServer(key string, mutate func(*ServerState)) {
	if mutate == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil {
		return
	}
	mutate(&entry.state)
	_ = s.persistLocked()
}

// SetServerConnected 更新连接状态与最近错误（无变化时不写盘）。
//
// 连接成功时顺带把阶段置为 connected 并清空失败计数：FailCount 统计的是"连续失败"，
// 任何一次成功都该让它归零（手动重试、心跳成功、重新推会话都走这里）。
func (s *stateStore) SetServerConnected(key string, connected bool, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil {
		return
	}
	phase := entry.state.Phase
	if connected {
		phase = ServerPhaseConnected
	}
	if entry.state.Connected == connected && entry.state.LastError == lastError &&
		entry.state.Phase == phase && (!connected || entry.state.FailCount == 0) {
		return
	}
	entry.state.Connected = connected
	entry.state.LastError = lastError
	entry.state.Phase = phase
	if connected {
		entry.state.FailCount = 0
	}
	_ = s.persistLocked()
}

// SetServerPhase 更新某台的连接阶段（无变化时不写盘）。
func (s *stateStore) SetServerPhase(key, phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil || entry.state.Phase == phase {
		return
	}
	entry.state.Phase = phase
	_ = s.persistLocked()
}

// RecordServerConnectFailure 记某台一次自动重连失败，返回是否已达上限（上限后不再自动重连）。
func (s *stateStore) RecordServerConnectFailure(key, lastError string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil {
		return true
	}
	entry.state.FailCount++
	entry.state.Connected = false
	entry.state.LastError = lastError
	exhausted := entry.state.FailCount >= maxServerConnectAttempts
	if exhausted {
		entry.state.Phase = ServerPhaseDisconnected
	} else {
		entry.state.Phase = ServerPhaseConnecting
	}
	_ = s.persistLocked()
	return exhausted
}

// ResetServerConnectFailures 清零某台的失败计数并回到"连接中"（界面手动重试时调用）。
func (s *stateStore) ResetServerConnectFailures(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil {
		return
	}
	entry.state.FailCount = 0
	entry.state.LastError = ""
	entry.state.Phase = ServerPhaseConnecting
	_ = s.persistLocked()
}

// User 返回某台服务端的登录用户（key 为空时取主服务端）。
func (s *stateStore) User(key string) UserState {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.servers[s.resolveExistingKeyLocked(key)]
	if entry == nil {
		return UserState{}
	}
	return entry.user
}

// SetUser 覆盖某台服务端的登录用户并持久化。
func (s *stateStore) SetUser(key string, user UserState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if resolved := s.resolveExistingKeyLocked(key); resolved != "" {
		key = resolved
	}
	entry := s.entryLocked(key)
	if entry == nil {
		return
	}
	entry.user = user
	_ = s.persistLocked()
}

// MountsForServer 返回归属于某台服务端的挂载状态（按键为空时返回未归类 + 主服务端的）。
func (s *stateStore) MountsForServer(key string) []MountState {
	s.mu.Lock()
	defer s.mu.Unlock()
	primary := s.primaryKeyLocked()
	out := make([]MountState, 0, len(s.mounts))
	for _, ms := range s.mounts {
		if mountBelongsTo(ms, key, primary) {
			out = append(out, *ms)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AllocationID < out[j].AllocationID })
	return out
}

// mountBelongsTo 判断挂载是否归属某台服务端：有显式归属按显式归属，未归类的（旧记录）算主服务端的。
func mountBelongsTo(ms *MountState, key, primary string) bool {
	if ms == nil {
		return false
	}
	if strings.TrimSpace(ms.ServerKey) == "" {
		return key == primary
	}
	return ms.ServerKey == key
}

// PutMount 写入/覆盖挂载状态与运行时信息并持久化。
func (s *stateStore) PutMount(ms *MountState, rt *mountRuntime) {
	if ms == nil || ms.AllocationID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *ms
	s.mounts[ms.AllocationID] = &cp
	if rt != nil {
		rtCopy := *rt
		s.runtime[ms.AllocationID] = &rtCopy
	} else if _, ok := s.runtime[ms.AllocationID]; !ok {
		s.runtime[ms.AllocationID] = &mountRuntime{}
	}
	_ = s.persistLocked()
}

// GetMount 返回挂载状态与运行时信息副本（都不存在时 ok=false）。
func (s *stateStore) GetMount(allocationID string) (MountState, mountRuntime, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.mounts[allocationID]
	if !ok {
		return MountState{}, mountRuntime{}, false
	}
	var rt mountRuntime
	if r, ok := s.runtime[allocationID]; ok && r != nil {
		rt = *r
	}
	return *ms, rt, true
}

// UpdateMount 在锁内变更挂载状态与运行时信息并持久化。
func (s *stateStore) UpdateMount(allocationID string, mutate func(*MountState, *mountRuntime)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.mounts[allocationID]
	if !ok {
		return false
	}
	rt := s.runtime[allocationID]
	if rt == nil {
		rt = &mountRuntime{}
		s.runtime[allocationID] = rt
	}
	mutate(ms, rt)
	_ = s.persistLocked()
	return true
}

// DeleteMount 删除挂载状态（并清除内存中的 CHAP 密钥）并持久化。
func (s *stateStore) DeleteMount(allocationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mounts, allocationID)
	delete(s.runtime, allocationID)
	_ = s.persistLocked()
}

// ListMounts 返回全部挂载状态副本（按 allocation_id 排序，保证输出稳定）。
func (s *stateStore) ListMounts() []MountState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MountState, 0, len(s.mounts))
	for _, ms := range s.mounts {
		out = append(out, *ms)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AllocationID < out[j].AllocationID })
	return out
}

// MountByLease 按 lease_id 反查挂载状态。
func (s *stateStore) MountByLease(leaseID string) (MountState, bool) {
	if strings.TrimSpace(leaseID) == "" {
		return MountState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ms := range s.mounts {
		if ms.LeaseID == leaseID {
			return *ms, true
		}
	}
	return MountState{}, false
}

// UsedDiskNumbers 返回本代理已占用的磁盘号（排除 exclude 指定的分配）。
func (s *stateStore) UsedDiskNumbers(exclude string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.runtime))
	for id, rt := range s.runtime {
		if id == exclude || rt == nil || !rt.DiskKnown {
			continue
		}
		out = append(out, rt.DiskNumber)
	}
	sort.Ints(out)
	return out
}

// Persist 强制写盘（退出前调用）。
func (s *stateStore) Persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

// persistLocked 写盘。调用方需持有锁。
func (s *stateStore) persistLocked() error {
	state := persistedState{
		ClientID:   s.clientID,
		PrimaryKey: s.primaryKeyLocked(),
		Servers:    make([]persistedServer, 0, len(s.servers)),
		Mounts:     make([]MountState, 0, len(s.mounts)),
	}
	for _, key := range s.orderedKeysLocked() {
		entry := s.servers[key]
		if entry == nil {
			continue
		}
		state.Servers = append(state.Servers, persistedServer{Key: key, State: entry.state, User: entry.user})
	}
	for _, ms := range s.mounts {
		state.Mounts = append(state.Mounts, *ms)
	}
	sort.Slice(state.Mounts, func(i, j int) bool {
		return state.Mounts[i].AllocationID < state.Mounts[j].AllocationID
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

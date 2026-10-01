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

// ServerState 描述代理当前连接的服务端。
type ServerState struct {
	URL        string `json:"url"`
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Connected  bool   `json:"connected"`
	LastError  string `json:"last_error,omitempty"`
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
	MountMode    string `json:"mount_mode"`
	MountPath    string `json:"mount_path"`
	DiskNumber   int    `json:"disk_number,omitempty"`
	State        string `json:"state"`
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

// persistedState 是本地状态文件的形状。
//
// 注意：只持久化「服务端展示信息 + 挂载状态」，不含会话令牌与 CHAP 密钥。
type persistedState struct {
	ClientID string       `json:"client_id"`
	Server   ServerState  `json:"server"`
	User     UserState    `json:"user"`
	Mounts   []MountState `json:"mounts"`
}

// stateStore 是代理的内存态 + 本地持久化。
type stateStore struct {
	path   string
	logger *slog.Logger

	mu       sync.Mutex
	clientID string
	server   ServerState
	user     UserState
	mounts   map[string]*MountState
	runtime  map[string]*mountRuntime
	session  *Session
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
			s.server = loaded.Server
			s.user = loaded.User
			// 进程重启后真实会话已不存在：连接状态一律复位。
			s.server.Connected = false
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

// Path 返回状态文件路径。
func (s *stateStore) Path() string { return s.path }

// ClientID 返回全局唯一且稳定的客户端标识（不使用 MAC/IP）。
func (s *stateStore) ClientID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientID
}

// SetSession 保存服务端会话（仅内存），并记录收到时刻（用于计算会话寿命）。
func (s *stateStore) SetSession(session *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == nil {
		s.session = nil
		return
	}
	cp := *session
	cp.receivedAt = time.Now().UnixMilli()
	s.session = &cp
}

// Session 返回当前会话副本；未推送时 ok=false。
func (s *stateStore) Session() (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil, false
	}
	cp := *s.session
	return &cp, true
}

// ClearSession 清除会话（退出登录），并把连接状态复位。
func (s *stateStore) ClearSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.session = nil
	s.server.Connected = false
	s.server.LastError = ""
	_ = s.persistLocked()
}

// Server 返回服务端状态。
func (s *stateStore) Server() ServerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server
}

// SetServer 覆盖服务端状态并持久化。
func (s *stateStore) SetServer(state ServerState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.server = state
	_ = s.persistLocked()
}

// SetServerConnected 更新连接状态与最近错误（无变化时不写盘）。
func (s *stateStore) SetServerConnected(connected bool, lastError string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.server.Connected == connected && s.server.LastError == lastError {
		return
	}
	s.server.Connected = connected
	s.server.LastError = lastError
	_ = s.persistLocked()
}

// User 返回当前用户。
func (s *stateStore) User() UserState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.user
}

// SetUser 覆盖当前用户并持久化。
func (s *stateStore) SetUser(user UserState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.user = user
	_ = s.persistLocked()
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
		ClientID: s.clientID,
		Server:   s.server,
		User:     s.user,
		Mounts:   make([]MountState, 0, len(s.mounts)),
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

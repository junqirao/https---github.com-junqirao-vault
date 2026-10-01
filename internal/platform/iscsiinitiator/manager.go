// Package iscsiinitiator 封装 Windows iSCSI **发起端**（客户端侧）能力：
// 门户添加、目标连接（含单向 CHAP）、会话等待/查询/断开、持久化注销与目标缓存清理。
//
// 与服务端侧的 internal/platform/iscsitarget（目标服务器）不同，这里使用的是 Windows 的
// IscsiInitiator 模块（MSiSCSI 服务），即发起端。
//
// 三条硬性约束：
//   - CHAP 密钥绝不能出现在日志或错误信息中（日志一律脱敏为 ***）；
//   - 卸载时必须先把磁盘 Offline 再断开，否则 Disconnect-IscsiTarget 会返回
//     HRESULT 0xefff0040（会话上有在线设备）—— 该情况映射为 platform.iscsi_device_in_use；
//   - 参数一律通过 `-File <脚本> -Name value` 传递，不做字符串拼接（防命令注入）。
package iscsiinitiator

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// DefaultPortalPort 是 iSCSI 目标门户默认端口。
const DefaultPortalPort = 3260

// DefaultConnectTimeout 是等待会话建立的默认超时（见 docs/implementation.md 5.5）。
const DefaultConnectTimeout = 60 * time.Second

// 客户端 iSCSI 鉴权模式。
const (
	// AuthModeNone 不鉴权（服务端侧按 initiator 白名单过滤时也走此模式）。
	AuthModeNone = "none"
	// AuthModeCHAP 单向 CHAP。
	AuthModeCHAP = "chap"
)

// Manager 是 iSCSI 发起端操作入口。
type Manager struct {
	ps     *winps.Runner
	logger *slog.Logger
}

// NewManager 构造 Manager。runner 为 nil 时使用默认配置的执行器。
func NewManager(runner *winps.Runner, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if runner == nil {
		runner = winps.NewRunner(winps.Options{Logger: logger})
	}
	return &Manager{ps: runner, logger: logger}
}

// 服务状态归一化取值（Availability.ServiceState）。
const (
	ServiceStateRunning = "running"
	ServiceStateStopped = "stopped"
	ServiceStateUnknown = "unknown"
)

// Availability 是 iSCSI 发起端的**只读**探测结果。
//
// 为什么要把 service_status 带出来：探测早就跑了，但结果只写日志、无人消费 ——
// 于是"本机 MSiSCSI 没启动"这件事只在挂载失败（阶段：connect）时才暴露，而且错误里
// 没有任何可执行信息（真实反馈：希望客户端启动时就提示）。
type Availability struct {
	// ModuleAvailable 是否找得到 IscsiInitiator 模块 / 发起端 cmdlet。
	ModuleAvailable bool
	// ModuleVersion 模块版本（可能为空，仅用于诊断）。
	ModuleVersion string
	// ServiceStatus MSiSCSI 服务状态原文（如 Running / Stopped / StartPending）；
	// 为空表示脚本没有报出状态（异常路径），按"未知"处理。
	ServiceStatus string
}

// Ready 判断"现在就能发起点 iSCSI 连接"：模块可用且服务正在运行。
func (a Availability) Ready() bool {
	return a.ModuleAvailable && strings.EqualFold(strings.TrimSpace(a.ServiceStatus), "Running")
}

// ServiceState 把服务状态归一化为界面可用的三值：running / stopped / unknown。
func (a Availability) ServiceState() string {
	status := strings.TrimSpace(a.ServiceStatus)
	switch {
	case status == "":
		return ServiceStateUnknown
	case strings.EqualFold(status, "Running"):
		return ServiceStateRunning
	default:
		return ServiceStateStopped
	}
}

// Probe 只读探测 iSCSI 发起端能力（模块 + MSiSCSI 服务状态）。
//
// ⚠️ 与 EnsureService 的关键区别：**绝不启动或修改服务**。预检（启动时、后台复检）
// 必须无副作用 —— 擅自启动系统服务属于部署决策，只能由用户点挂载时的 EnsureService 触发。
func (m *Manager) Probe(ctx context.Context) (Availability, error) {
	params := []winps.Param{winps.String("ServiceName", "MSiSCSI")}
	var result struct {
		ModuleVersion string `json:"module_version"`
		ServiceStatus string `json:"service_status"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorAvailable, params, &result); err != nil {
		var scriptErr *winps.ScriptError
		if errors.As(err, &scriptErr) && scriptErr.Reason == "initiator_unavailable" {
			return Availability{}, apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
				WithArg("component", "iscsiinitiator")
		}
		return Availability{}, err
	}
	avail := Availability{
		ModuleAvailable: true,
		ModuleVersion:   result.ModuleVersion,
		ServiceStatus:   result.ServiceStatus,
	}
	m.logger.Info("iSCSI 发起端探测完成",
		"module_version", result.ModuleVersion,
		"service_status", result.ServiceStatus,
		"ready", avail.Ready())
	return avail, nil
}

// Available 探测 iSCSI 发起端是否可用（模块与 cmdlet 是否存在）。
//
// 语义（保持不变，服务端能力上报依赖它）：**服务未运行不算"不可用"** —— 挂载前的
// EnsureService 会启动它；只有模块/cmdlet 缺失才算不可用（system.unavailable）。
// 需要服务状态请用 Probe。
func (m *Manager) Available(ctx context.Context) error {
	_, err := m.Probe(ctx)
	return err
}

// EnsureService 确保 iSCSI 发起端服务（MSiSCSI）处于运行状态（幂等：已在运行不做改动）。
//
// ⚠️ 为什么挂载前必须调用：Connect-IscsiTarget / Get-IscsiSession 等 cmdlet 依赖该服务的
// WMI 提供程序。服务未运行时调用会直接抛 CIM 异常，而挂载链路只把错误压成
// reason=connect_failed（界面仅显示"挂载失败（阶段：connect）"），真因被埋进日志、极难排查
// （真实工单）。设计文档 5.5 步骤 ③a 本就要求 "Start-Service msiscsi / 确保服务运行"，
// 此前实现缺失。这里只"启动"，不修改启动类型（宿主机策略交由部署固化）。
func (m *Manager) EnsureService(ctx context.Context) error {
	params := []winps.Param{winps.String("ServiceName", "MSiSCSI")}
	var result struct {
		Action        string `json:"action"`
		ServiceName   string `json:"service_name"`
		ServiceStatus string `json:"service_status"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorEnsureService, params, &result); err != nil {
		return err
	}
	m.logger.Info("iSCSI 发起端服务已就绪",
		"service", result.ServiceName, "action", result.Action, "status", result.ServiceStatus)
	return nil
}

// EnsurePortal 幂等添加目标门户（已存在同一「地址 + 端口」时跳过）。
func (m *Manager) EnsurePortal(ctx context.Context, address string, port int) error {
	if strings.TrimSpace(address) == "" {
		return apperr.InvalidParam("portal_address")
	}
	if port <= 0 {
		port = DefaultPortalPort
	}
	params := []winps.Param{
		winps.String("Address", address),
		winps.String("Port", strconv.Itoa(port)),
	}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorEnsurePortal, params, &result); err != nil {
		return err
	}
	m.logger.Info("已确保 iSCSI 门户存在", "address", address, "port", port, "action", result.Action)
	return nil
}

// ConnectOptions 描述一次 iSCSI 目标连接。
type ConnectOptions struct {
	// TargetIQN 目标 IQN。
	TargetIQN string
	// PortalAddress 门户地址。
	PortalAddress string
	// PortalPort 门户端口，<=0 时使用 3260。
	PortalPort int
	// AuthMode none | chap。
	AuthMode string
	// ChapUser / ChapSecret 单向 CHAP 凭据；仅 AuthMode=chap 时使用。
	// ⚠️ ChapSecret 绝不写入日志。
	ChapUser   string
	ChapSecret string
	// Persistent 是否持久化（机器重启后自动重连）。
	Persistent bool
}

// Connect 连接目标（幂等：已连接时直接返回）。支持单向 CHAP。
func (m *Manager) Connect(ctx context.Context, opt ConnectOptions) error {
	if strings.TrimSpace(opt.TargetIQN) == "" {
		return apperr.InvalidParam("target_iqn")
	}
	if strings.TrimSpace(opt.PortalAddress) == "" {
		return apperr.InvalidParam("portal_address")
	}
	mode := strings.ToLower(strings.TrimSpace(opt.AuthMode))
	if mode == "" {
		mode = AuthModeNone
	}
	if mode != AuthModeNone && mode != AuthModeCHAP {
		return apperr.InvalidParam("auth_mode")
	}
	if mode == AuthModeCHAP && (opt.ChapUser == "" || opt.ChapSecret == "") {
		return apperr.InvalidParam("chap")
	}
	port := opt.PortalPort
	if port <= 0 {
		port = DefaultPortalPort
	}

	params := []winps.Param{
		winps.String("TargetIQN", opt.TargetIQN),
		winps.String("PortalAddress", opt.PortalAddress),
		winps.String("PortalPort", strconv.Itoa(port)),
		winps.String("AuthMode", mode),
		winps.Bool("Persistent", opt.Persistent),
	}
	// 日志脱敏：CHAP 密钥绝不能出现在日志中。
	logArgs := []any{
		"target_iqn", opt.TargetIQN,
		"portal", opt.PortalAddress,
		"port", port,
		"auth_mode", mode,
		"persistent", opt.Persistent,
	}
	if mode == AuthModeCHAP {
		params = append(params,
			winps.String("ChapUser", opt.ChapUser),
			winps.String("ChapSecret", opt.ChapSecret))
		logArgs = append(logArgs, "chap_user", opt.ChapUser, "chap_secret", "***")
	}

	m.logger.Info("连接 iSCSI 目标", logArgs...)
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorConnect, params, &result); err != nil {
		return err
	}
	m.logger.Info("iSCSI 目标连接完成", "target_iqn", opt.TargetIQN, "action", result.Action)
	return nil
}

// WaitConnected 轮询直到会话建立；超时返回 platform.iscsi_session_not_established。
func (m *Manager) WaitConnected(ctx context.Context, targetIQN string, timeout time.Duration) error {
	if strings.TrimSpace(targetIQN) == "" {
		return apperr.InvalidParam("target_iqn")
	}
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	params := []winps.Param{
		winps.String("TargetIQN", targetIQN),
		winps.String("TimeoutSeconds", strconv.Itoa(int(timeout.Seconds()))),
	}
	var result struct {
		Connected bool `json:"connected"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorWaitConnected, params, &result); err != nil {
		var scriptErr *winps.ScriptError
		if errors.As(err, &scriptErr) && scriptErr.Reason == "not_connected" {
			return apperr.New(CodeSessionNotEstablished, http.StatusGatewayTimeout).
				WithArg("target_iqn", targetIQN).
				WithArg("timeout_seconds", int(timeout.Seconds()))
		}
		return err
	}
	return nil
}

// IsConnected 查询指定目标是否已连接。
func (m *Manager) IsConnected(ctx context.Context, targetIQN string) (bool, error) {
	if strings.TrimSpace(targetIQN) == "" {
		return false, apperr.InvalidParam("target_iqn")
	}
	params := []winps.Param{winps.String("TargetIQN", targetIQN)}
	var result struct {
		Connected bool `json:"connected"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorIsConnected, params, &result); err != nil {
		return false, err
	}
	return result.Connected, nil
}

// Disconnect 断开指定目标的所有会话（幂等）。
//
// ⚠️ 调用方必须先把该会话对应的磁盘置为 Offline，否则会返回
// platform.iscsi_device_in_use（HRESULT 0xefff0040）。
func (m *Manager) Disconnect(ctx context.Context, targetIQN string) error {
	if strings.TrimSpace(targetIQN) == "" {
		return apperr.InvalidParam("target_iqn")
	}
	params := []winps.Param{winps.String("TargetIQN", targetIQN)}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorDisconnect, params, &result); err != nil {
		var scriptErr *winps.ScriptError
		if errors.As(err, &scriptErr) && scriptErr.Reason == "device_in_use" {
			return ErrDeviceInUse()
		}
		return err
	}
	m.logger.Info("已断开 iSCSI 会话", "target_iqn", targetIQN, "action", result.Action)
	return nil
}

// Unregister 取消会话持久化（避免重启后自动重连）。幂等。
func (m *Manager) Unregister(ctx context.Context, targetIQN string) error {
	if strings.TrimSpace(targetIQN) == "" {
		return apperr.InvalidParam("target_iqn")
	}
	params := []winps.Param{winps.String("TargetIQN", targetIQN)}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorUnregister, params, &result); err != nil {
		return err
	}
	m.logger.Info("已取消 iSCSI 会话持久化", "target_iqn", targetIQN, "action", result.Action)
	return nil
}

// RemoveTarget 清理已发现的目标缓存（避免重名冲突）。幂等。
//
// 部分系统未提供对应 cmdlet，此时脚本返回 action=unsupported 且视为成功（尽力而为）。
func (m *Manager) RemoveTarget(ctx context.Context, targetIQN string) error {
	if strings.TrimSpace(targetIQN) == "" {
		return apperr.InvalidParam("target_iqn")
	}
	params := []winps.Param{winps.String("TargetIQN", targetIQN)}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptInitiatorRemoveTarget, params, &result); err != nil {
		return err
	}
	m.logger.Info("已清理 iSCSI 目标缓存", "target_iqn", targetIQN, "action", result.Action)
	return nil
}

// Package iscsitarget 封装 Windows IscsiTarget 模块（iSCSI 目标服务器角色），
// 提供虚拟盘登记、目标/授权/CHAP 配置、以及「虚拟盘 ↔ 目标」映射的管理能力。
//
// 两条硬性约束：
//   - `InitiatorIds` 是全量替换语义，模块没有增量追加的 cmdlet，合并由上层负责；
//   - CHAP 密钥绝不能出现在错误信息或日志中（日志一律脱敏为 ***）。
package iscsitarget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// CodeTargetExists 目标已存在（CreateTarget 专用）。
const CodeTargetExists = "iscsi.target_exists"

// Manager 是 iSCSI 目标服务器操作入口。
type Manager struct {
	ps     *winps.Runner
	logger *slog.Logger

	// importedMu / importedPaths 记住本进程内**已登记**过的虚拟盘路径（见 ImportVirtualDisk
	// 关于性能与失效的说明）：避免每次发布/对账都花约 10 秒重跑一次登记脚本。
	importedMu    sync.Mutex
	importedPaths map[string]bool
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

// stringList 兼容"PowerShell 把单元素数组序列化为字符串"以及 null 的情况。
type stringList []string

// UnmarshalJSON 同时接受字符串、字符串数组与 null。
//
// ⚠️ 形状不符时**必须报错，绝不能静默置空**。
// 旧实现在"数组元素不是字符串"时把字段置为 nil 并返回成功，于是上层看到的授权列表 /
// 映射列表凭空为空，而调用方毫不知情（实测成因：脚本曾把 initiator 输出成
// {Method,Value} 对象数组，授权列表就这样"消失"了，排查代价极高）。
// 现在直接返回错误，让脚本与上层的格式不匹配在第一时间暴露。
func (s *stringList) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*s = nil
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = stringList{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err == nil {
		*s = many
		return nil
	}
	return fmt.Errorf("平台脚本返回的列表字段形状非法（既不是字符串也不是字符串数组）：%s",
		truncateForError(raw))
}

// truncateForError 截断错误信息里回显的原始 JSON，避免把整段脚本输出塞进日志与响应。
func truncateForError(raw string) string {
	const max = 200
	if len(raw) <= max {
		return raw
	}
	return raw[:max] + "...（截断）"
}

// CodeInvalidTargetName 目标名非法（含 Windows 无法接受的字符）。
const CodeInvalidTargetName = "iscsi.invalid_target_name"

// validateTargetName 校验目标名能否被 Windows iSCSI 目标服务器接受。
//
// ⚠️ 这条校验是**用事故换来的**：目标名会被 Windows 当作 IQN 后缀参与语法校验，
// RFC 3720 的 IQN 只允许字母、数字、'.'、'-'、':'。名字里出现 '_' 时，
// New-IscsiServerTarget 只会抛出一句"Unable to create the iSCSI target."，
// 现场完全看不出是命名问题（正是"创建磁盘必然失败"被长期误判的原因之一）。
// 提前拦下后，错误信息直接点名非法字符，且不会让请求白跑一趟 PowerShell
// （该调用在真实服务器上约 10 秒）。
func validateTargetName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return apperr.InvalidParam("target_name")
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == ':':
		default:
			return apperr.New(CodeInvalidTargetName, http.StatusBadRequest).
				WithArg("target_name", trimmed).
				WithArg("invalid_char", string(r))
		}
	}
	return nil
}

// Available 探测 iSCSI 目标服务器能力（IscsiTarget 模块 + WinTarget 服务）。
//
// 未安装角色/服务时返回 system.unavailable，便于系统能力探测直接使用。
func (m *Manager) Available(ctx context.Context) error {
	params := []winps.Param{winps.String("ServiceName", "WinTarget")}
	var result struct {
		ModuleVersion string `json:"module_version"`
		ServiceStatus string `json:"service_status"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiAvailable, params, &result); err != nil {
		var scriptErr *winps.ScriptError
		if errors.As(err, &scriptErr) && scriptErr.Reason == "iscsi_unavailable" {
			return apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).
				WithArg("component", "iscsitarget")
		}
		return err
	}
	m.logger.Info("iSCSI 目标服务器可用",
		"module_version", result.ModuleVersion,
		"service_status", result.ServiceStatus)
	return nil
}

// ImportVirtualDisk 幂等地把已有 VHDX 纳入 iSCSI 虚拟盘登记（已登记则跳过）。
//
// 调用前 VHDX 必须处于本机未挂载状态（同一份文件不能被本地挂载与 iSCSI 发布同时持有）。
//
// 性能：本调用每次都要跑一遍 PowerShell（脚本内要枚举 Get-IscsiVirtualDisk 判断是否已登记），
// **实测约 10 秒**，而每次发布、每次对账都会走到它。登记是"只增不减"的幂等操作，
// 因此进程内记住已登记过的路径即可安全跳过；进程重启后缓存为空 → 首次仍会真正检查一次
// （安全侧）。记忆在 RemoveVirtualDisk 时清除，后续步骤失败时由 winbackend 调
// ForgetImported 主动失效 —— 绝不留下"以为登记过其实没有"的隐性故障。
func (m *Manager) ImportVirtualDisk(ctx context.Context, vhdxPath, description string) error {
	path := strings.TrimSpace(vhdxPath)
	if path == "" {
		return apperr.InvalidParam("vhdx_path")
	}
	key := normalizePathForCache(path)
	if m.importedAlready(key) {
		m.logger.Debug("虚拟盘本进程内已登记，跳过重复登记（省一次 PowerShell）", "path", path)
		return nil
	}
	params := []winps.Param{
		winps.String("Path", path),
		winps.String("Description", description),
	}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiEnsureVirtualDisk, params, &result); err != nil {
		return err
	}
	m.rememberImported(key)
	m.logger.Info("已登记 iSCSI 虚拟盘", "path", path, "action", result.Action)
	return nil
}

// ForgetImported 丢弃"已登记"记忆（后续步骤失败时调用，迫使下次真正重新登记一次）。
func (m *Manager) ForgetImported(vhdxPath string) {
	key := normalizePathForCache(vhdxPath)
	if key == "" {
		return
	}
	m.importedMu.Lock()
	defer m.importedMu.Unlock()
	delete(m.importedPaths, key)
}

func (m *Manager) importedAlready(key string) bool {
	m.importedMu.Lock()
	defer m.importedMu.Unlock()
	return m.importedPaths[key]
}

func (m *Manager) rememberImported(key string) {
	m.importedMu.Lock()
	defer m.importedMu.Unlock()
	if m.importedPaths == nil {
		m.importedPaths = make(map[string]bool)
	}
	m.importedPaths[key] = true
}

// normalizePathForCache 归一路径（Windows 语义：大小写与分隔符不敏感）。
func normalizePathForCache(path string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(path), "/", `\`))
}

// RemoveVirtualDisk 移除 iSCSI 虚拟盘登记（**不删除 .vhdx 文件**，文件由上层另行删除）。幂等。
func (m *Manager) RemoveVirtualDisk(ctx context.Context, vhdxPath string) error {
	if strings.TrimSpace(vhdxPath) == "" {
		return apperr.InvalidParam("vhdx_path")
	}
	params := []winps.Param{winps.String("Path", vhdxPath)}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiRemoveVirtualDisk, params, &result); err != nil {
		return err
	}
	m.ForgetImported(vhdxPath)
	m.logger.Info("已移除 iSCSI 虚拟盘登记", "path", vhdxPath, "action", result.Action)
	return nil
}

// VirtualDiskExists 判断 VHDX 是否已登记为 iSCSI 虚拟盘。
func (m *Manager) VirtualDiskExists(ctx context.Context, vhdxPath string) (bool, error) {
	if strings.TrimSpace(vhdxPath) == "" {
		return false, apperr.InvalidParam("vhdx_path")
	}
	paths, err := m.listVirtualDisks(ctx, vhdxPath)
	if err != nil {
		return false, err
	}
	return len(paths) > 0, nil
}

// ListVirtualDisks 列出全部已登记的虚拟盘路径。
func (m *Manager) ListVirtualDisks(ctx context.Context) ([]string, error) {
	return m.listVirtualDisks(ctx, "")
}

// virtualDiskDTO 是 iscsi_get_virtual_disks.ps1 中单条虚拟盘的 JSON 结构。
type virtualDiskDTO struct {
	Path        string     `json:"path"`
	Description string     `json:"description"`
	TargetNames stringList `json:"target_names"`
}

func (m *Manager) listVirtualDisks(ctx context.Context, path string) ([]string, error) {
	params := []winps.Param{winps.String("Path", path)}
	var result struct {
		Disks []virtualDiskDTO `json:"disks"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiGetVirtualDisks, params, &result); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(result.Disks))
	for _, disk := range result.Disks {
		if disk.Path != "" {
			paths = append(paths, disk.Path)
		}
	}
	return paths, nil
}

// TargetInfo 描述一个 iSCSI 目标。
type TargetInfo struct {
	// Name 目标名。
	Name string
	// Enabled 目标是否启用。
	Enabled bool
	// InitiatorIDs 已授权的 initiator 列表（形如 "IQN:iqn.xxx"）。
	InitiatorIDs []string
	// MappedDevices 映射到该目标的虚拟盘路径。
	MappedDevices []string
}

// targetDTO 是 iscsi_get_targets.ps1 中单条目标的 JSON 结构。
type targetDTO struct {
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	InitiatorIDs  stringList `json:"initiator_ids"`
	MappedDevices stringList `json:"mapped_devices"`
}

// GetTarget 查询单个目标；不存在时返回 iscsi.target_not_found。
func (m *Manager) GetTarget(ctx context.Context, name string) (*TargetInfo, error) {
	if err := validateTargetName(name); err != nil {
		return nil, err
	}
	targets, err := m.listTargets(ctx, name)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		if strings.EqualFold(targets[i].Name, name) {
			return &targets[i], nil
		}
	}
	return nil, apperr.IscsiTargetNotFound()
}

// ListTargets 列出全部目标。
func (m *Manager) ListTargets(ctx context.Context) ([]TargetInfo, error) {
	return m.listTargets(ctx, "")
}

func (m *Manager) listTargets(ctx context.Context, name string) ([]TargetInfo, error) {
	params := []winps.Param{winps.String("TargetName", name)}
	var result struct {
		Targets []targetDTO `json:"targets"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiGetTargets, params, &result); err != nil {
		return nil, err
	}
	targets := make([]TargetInfo, 0, len(result.Targets))
	for _, target := range result.Targets {
		targets = append(targets, TargetInfo{
			Name:          target.Name,
			Enabled:       target.Enabled,
			InitiatorIDs:  target.InitiatorIDs,
			MappedDevices: target.MappedDevices,
		})
	}
	return targets, nil
}

// CreateTarget 创建目标并授权初始 initiator 列表，同时启用目标。
//
// 目标已存在时返回 iscsi.target_exists（不会静默覆盖已有配置）。
func (m *Manager) CreateTarget(ctx context.Context, name string, initiatorIDs []string) error {
	if err := validateTargetName(name); err != nil {
		return err
	}
	existing, err := m.listTargets(ctx, name)
	if err != nil {
		return err
	}
	for _, target := range existing {
		if strings.EqualFold(target.Name, name) {
			return apperr.New(CodeTargetExists, http.StatusConflict).WithArg("target_name", name)
		}
	}

	ids := initiatorIDs
	if ids == nil {
		ids = []string{}
	}
	enabled := true
	return m.SetTarget(ctx, name, SetTargetOptions{InitiatorIDs: &ids, Enabled: &enabled})
}

// RemoveTarget 删除目标（会先解除其名下全部映射）。幂等。
func (m *Manager) RemoveTarget(ctx context.Context, name string) error {
	if err := validateTargetName(name); err != nil {
		return err
	}
	params := []winps.Param{winps.String("TargetName", name)}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiRemoveTarget, params, &result); err != nil {
		return err
	}
	m.logger.Info("已删除 iSCSI 目标", "target", name, "action", result.Action)
	return nil
}

// SetTargetOptions 描述 SetTarget 的更新内容。
type SetTargetOptions struct {
	// InitiatorIDs 非 nil 时**全量替换**授权列表。
	//
	// ⚠️ 关键语义：IscsiTarget 模块没有 Add-InitiatorId / Remove-InitiatorId 这类增量 cmdlet，
	// `-InitiatorIds` 是整体覆盖。要"追加一个授权"，调用方必须先 GetTarget 读出
	// 现有列表、合并后整体回写（并自行加锁，避免并发覆盖）。
	// 指向空切片表示清空全部授权；nil 表示不改动授权。
	InitiatorIDs *[]string

	// Enabled 非 nil 时启用/停用目标。停用后 initiator 无法继续访问。
	Enabled *bool

	// ChapUser 与 ChapSecret 同时非空时启用单向 CHAP。
	ChapUser   string
	ChapSecret string

	// EnableReverseChap 为 true 时启用反向 CHAP（要求同时提供单向 CHAP 与反向 CHAP 凭据）。
	EnableReverseChap bool
	ReverseChapUser   string
	ReverseChapSecret string
}

// SetTarget 更新目标配置（目标不存在时由脚本创建，即 upsert）。
func (m *Manager) SetTarget(ctx context.Context, name string, opt SetTargetOptions) error {
	if err := validateTargetName(name); err != nil {
		return err
	}
	if (opt.ChapUser == "") != (opt.ChapSecret == "") {
		return apperr.InvalidParam("chap")
	}
	if opt.EnableReverseChap {
		if opt.ChapUser == "" || opt.ChapSecret == "" {
			return apperr.InvalidParam("chap")
		}
		if opt.ReverseChapUser == "" || opt.ReverseChapSecret == "" {
			return apperr.InvalidParam("reverse_chap")
		}
	}

	params := []winps.Param{winps.String("TargetName", name)}
	logArgs := []any{"target", name}

	if opt.InitiatorIDs != nil {
		ids := *opt.InitiatorIDs
		if ids == nil {
			ids = []string{}
		}
		param, err := winps.JSONParam("InitiatorIdsJson", ids)
		if err != nil {
			return apperr.InvalidParam("initiator_ids")
		}
		params = append(params, param)
		logArgs = append(logArgs, "initiator_ids", strings.Join(ids, ","))
	}
	if opt.Enabled != nil {
		params = append(params, winps.Bool("Enabled", *opt.Enabled))
		logArgs = append(logArgs, "enabled", *opt.Enabled)
	}
	if opt.ChapUser != "" {
		params = append(params,
			winps.String("ChapUser", opt.ChapUser),
			winps.String("ChapSecret", opt.ChapSecret))
		// 日志脱敏：CHAP 密钥绝不能出现在日志中。
		logArgs = append(logArgs, "chap_user", opt.ChapUser, "chap_secret", "***")
	}
	if opt.EnableReverseChap {
		params = append(params,
			winps.Bool("EnableReverseChap", true),
			winps.String("ReverseChapUser", opt.ReverseChapUser),
			winps.String("ReverseChapSecret", opt.ReverseChapSecret))
		logArgs = append(logArgs, "reverse_chap_user", opt.ReverseChapUser, "reverse_chap_secret", "***")
	}

	// 注意：绝不能把 params 原文写入日志（其中含 CHAP 密钥）。
	m.logger.Info("更新 iSCSI 目标", logArgs...)

	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiSetTarget, params, &result); err != nil {
		return err
	}
	m.logger.Info("iSCSI 目标已更新", "target", name, "action", result.Action)
	return nil
}

// AddMapping 建立「虚拟盘 ↔ 目标」映射。幂等（已映射则跳过）。
func (m *Manager) AddMapping(ctx context.Context, targetName, devicePath string) error {
	if err := validateTargetName(targetName); err != nil {
		return err
	}
	if strings.TrimSpace(devicePath) == "" {
		return apperr.InvalidParam("device_path")
	}
	params := []winps.Param{
		winps.String("TargetName", targetName),
		winps.String("DevicePath", devicePath),
	}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiAddMapping, params, &result); err != nil {
		return err
	}
	m.logger.Info("已建立 iSCSI 映射", "target", targetName, "device", devicePath, "action", result.Action)
	return nil
}

// RemoveMapping 解除「虚拟盘 ↔ 目标」映射。幂等（未映射视为成功）。
func (m *Manager) RemoveMapping(ctx context.Context, targetName, devicePath string) error {
	if err := validateTargetName(targetName); err != nil {
		return err
	}
	if strings.TrimSpace(devicePath) == "" {
		return apperr.InvalidParam("device_path")
	}
	params := []winps.Param{
		winps.String("TargetName", targetName),
		winps.String("DevicePath", devicePath),
	}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptIscsiRemoveMapping, params, &result); err != nil {
		return err
	}
	m.logger.Info("已解除 iSCSI 映射", "target", targetName, "device", devicePath, "action", result.Action)
	return nil
}

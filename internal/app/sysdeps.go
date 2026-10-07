package app

import (
	"context"
	"time"
)

// SysDepsItem 是一项系统依赖的探测结果。
//
// 这是**平台中立的 DTO**：Linux 侧由 internal/platform/sysdeps 填充；
// Windows 没有这类"进程外依赖"（能力由 Hyper-V/IscsiTarget 角色承担），
// 报告里 Supported=false、Items 为空，前端据此不显示横幅。
type SysDepsItem struct {
	// Key 稳定标识，如 configfs_mount / kernel_modules / iscsi_fabric / lvm_tools。
	Key string `json:"key"`
	// Title 人类可读标题（前端横幅直接展示）。
	Title string `json:"title"`
	// Required 是否为核心功能必需（必需项缺失 = 服务端部分功能不可用）。
	Required bool `json:"required"`
	// Status 状态：ok | fixed | missing。
	Status string `json:"status"`
	// Detail 现状描述（"已加载 iscsi_target_mod" / "缺失 lvcreate"）。
	Detail string `json:"detail,omitempty"`
	// Fixed 该项本次执行的修复动作说明（Status=fixed 时非空）。
	Fixed string `json:"fixed,omitempty"`
	// Hint 缺失时的人工处置建议（含可直接复制的命令）。
	Hint string `json:"hint,omitempty"`
}

// 依赖项状态常量（与 sysdeps.Status 取值一一对应）。
const (
	// SysDepsOK 已满足。
	SysDepsOK = "ok"
	// SysDepsFixed 本次已自动修复。
	SysDepsFixed = "fixed"
	// SysDepsMissing 缺失且未能自动修复——**横幅要展示的就是这一类的 Required 项**。
	SysDepsMissing = "missing"
)

// SysDepsReport 是系统依赖自检报告。
//
// 生命周期：启动期做一次「探测 → 自动修复 → 复检」（见 cmd/vault-server/platform_linux.go），
// 之后 HTTP 查询走的是**实时探测**，这样运维在机器上补齐缺项后，前端横幅刷新一次就消失，
// 不需要重启服务端。
type SysDepsReport struct {
	// Supported 当前平台是否支持系统依赖自检（仅 Linux 为 true）。
	Supported bool `json:"supported"`
	// OK 必需项是否全部就绪（ok 或 fixed）。false 表示服务端有功能不可用。
	OK bool `json:"ok"`
	// CheckedAt 本次探测完成时间。
	CheckedAt time.Time `json:"checked_at"`
	// ElapsedMS 本次探测耗时。
	ElapsedMS int64 `json:"elapsed_ms"`
	// Root 服务端是否以 root 运行（非 root 时无法自动修复，只能提示）。
	Root bool `json:"root"`
	// RepairEnabled / InstallEnabled 是否允许自动修复 / 自动装包（配置开关）。
	//
	// 两者都为 false 时缺失项是"被配置明确关掉了自动处理"，不是"修不了"，
	// 前端文案要区分开，否则会误导运维去怀疑权限。
	RepairEnabled  bool `json:"repair_enabled"`
	InstallEnabled bool `json:"install_enabled"`
	// ConfigFSRoot 本次使用的 LIO configfs 根（排障用）。
	ConfigFSRoot string `json:"configfs_root,omitempty"`
	// Items 逐项结果（顺序固定）。
	Items []SysDepsItem `json:"items"`
	// Missing 仍缺失的**必需项** Key：横幅的核心内容。
	Missing []string `json:"missing"`
	// Fixed 最近一次自动修复动作解决的项 Key（启动期或 doctor）。
	Fixed []string `json:"fixed"`
}

// MissingItems 返回仍缺失的项（含可选），供前端逐条展示"缺了什么东西"。
func (r *SysDepsReport) MissingItems() []SysDepsItem {
	if r == nil {
		return nil
	}
	out := make([]SysDepsItem, 0, len(r.Items))
	for _, it := range r.Items {
		if it.Status == SysDepsMissing {
			out = append(out, it)
		}
	}
	return out
}

// SysDeps 返回系统依赖自检报告。
//
// 探针由平台层注入：Linux 是 internal/platform/sysdeps 的**只读实时探测**（不触发修复，
// 修复只在启动期与 `vault-server doctor` 里做——API 不该悄悄改宿主机）；
// Windows 未注入，返回 Supported=false 的空报告。
func (a *App) SysDeps(ctx context.Context) *SysDepsReport {
	if a.SysDepsProbe == nil {
		return &SysDepsReport{Supported: false, OK: true, CheckedAt: time.Now()}
	}
	if r := a.SysDepsProbe(ctx); r != nil {
		return r
	}
	return &SysDepsReport{Supported: false, OK: true, CheckedAt: time.Now()}
}

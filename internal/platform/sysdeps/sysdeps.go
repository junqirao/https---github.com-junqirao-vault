//go:build linux

// Package sysdeps 负责 Linux 服务端的**系统依赖自检与自愈**。
//
// 为什么需要它：vault-server 在 Linux 上依赖三类"进程外"的东西，缺任何一类都会在业务
// 路径上才暴露（而且报错位置离真正原因很远）：
//
//  1. 内核能力：configfs 已挂载，且 LIO 三个模块已加载
//     （target_core_mod / iscsi_target_mod / target_core_iblock）；
//     ⚠️ lsmod 里有模块 ≠ configfs 里注册成功——模块在、fabric 目录不在是真实发生过的故障；
//  2. configfs 目录树：<configfs_root>/iscsi（iSCSI fabric）与
//     <configfs_root>/core/iblock_0（块设备 backstore 插件）；
//  3. 命令行工具：LVM2（lvm/lvcreate/lvs/dmsetup）、文件系统工具
//     （mkfs.ext4|mkfs.xfs、resize2fs|xfs_growfs）等。
//
// 本包只做「探测 → 按需修复 → 复检」，两个调用点：
//   - 启动期：cmd/vault-server/platform_linux.go，受 platform.auto_repair / auto_install 控制；
//   - 命令行：`Vault-Server doctor`，运维手工执行，输出完整报告（可用 -json 供脚本消费）。
//
// 修复只做"可以安全自动化"的事：挂载 configfs、modprobe、重载 fabric 模块、
// 写 /etc/modules-load.d/vault-lio.conf、调发行版包管理器装缺失的工具包。
// 绝不改动宿主机已有的 LVM/lvm.conf 等配置，也绝不删除 configfs 里的任何对象。
package sysdeps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"
)

// Status 是单项依赖的最终状态。
type Status string

const (
	// StatusOK 已满足（无需动作）。
	StatusOK Status = "ok"
	// StatusFixed 原本缺失，本次已自动修复。
	StatusFixed Status = "fixed"
	// StatusMissing 缺失且未能自动修复（需要人工介入）。
	StatusMissing Status = "missing"
)

// Item 是一项系统依赖的检测/修复结果。
type Item struct {
	// Key 稳定标识（供脚本消费与日志检索），如 kernel_modules、configfs_mount。
	Key string `json:"key"`
	// Title 人类可读的标题。
	Title string `json:"title"`
	// Required 为 true 表示服务端核心功能必需（任一 Required 项 missing 即视为自检不通过）。
	Required bool `json:"required"`
	// Status 最终状态。
	Status Status `json:"status"`
	// Detail 现状描述（"已加载 xxx"/"缺失 lvcreate"）。
	Detail string `json:"detail,omitempty"`
	// Fixed 本次执行的修复动作说明（Status=fixed 时非空为佳）。
	Fixed string `json:"fixed,omitempty"`
	// Hint 未修复时的人工处置建议。
	Hint string `json:"hint,omitempty"`
}

// Report 是一次完整自检（含修复）的结果。
type Report struct {
	// CheckedAt 复检完成时间。
	CheckedAt time.Time `json:"checked_at"`
	// ElapsedMS 本次自检耗时（含修复与复检）。
	ElapsedMS int64 `json:"elapsed_ms"`
	// Root 当前进程是否以 root（uid=0）运行。
	Root bool `json:"root"`
	// RepairEnabled / InstallEnabled 本次是否允许修复 / 装包。
	RepairEnabled  bool `json:"repair_enabled"`
	InstallEnabled bool `json:"install_enabled"`
	// ConfigFSRoot 本次使用的 LIO configfs 根。
	ConfigFSRoot string `json:"configfs_root"`
	// Items 各项结果（顺序固定，便于 diff 与阅读）。
	Items []Item `json:"items"`
}

// OK 返回是否所有必需项都已满足（ok 或 fixed）。
func (r *Report) OK() bool {
	for _, it := range r.Items {
		if it.Required && it.Status == StatusMissing {
			return false
		}
	}
	return true
}

// MissingKeys 返回仍缺失的必需项 Key（供日志/接口摘要使用）。
func (r *Report) MissingKeys() []string {
	var out []string
	for _, it := range r.Items {
		if it.Required && it.Status == StatusMissing {
			out = append(out, it.Key)
		}
	}
	return out
}

// FixedKeys 返回本次被自动修复的 Key。
func (r *Report) FixedKeys() []string {
	var out []string
	for _, it := range r.Items {
		if it.Status == StatusFixed {
			out = append(out, it.Key)
		}
	}
	return out
}

// JSON 以 JSON 序列化报告（doctor -json 使用）。
func (r *Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// Render 写出人类可读报告：每行一项，最后给结论与下一步。
func (r *Report) Render(w io.Writer) {
	fmt.Fprintf(w, "系统依赖自检（configfs_root=%s，root=%v，自动修复=%v，自动装包=%v）\n",
		r.ConfigFSRoot, r.Root, r.RepairEnabled, r.InstallEnabled)
	for _, it := range r.Items {
		label := map[Status]string{
			StatusOK:      "[  OK   ]",
			StatusFixed:   "[ 已修复 ]",
			StatusMissing: "[ 缺失  ]",
		}[it.Status]
		if label == "" {
			label = "[  ?    ]"
		}
		req := ""
		if it.Required {
			req = "（必需）"
		} else {
			req = "（可选）"
		}
		fmt.Fprintf(w, "%s %s%s\n", label, it.Title, req)
		if it.Detail != "" {
			fmt.Fprintf(w, "          现状: %s\n", it.Detail)
		}
		if it.Fixed != "" {
			fmt.Fprintf(w, "          已执行: %s\n", it.Fixed)
		}
		if it.Status == StatusMissing && it.Hint != "" {
			fmt.Fprintf(w, "          处置: %s\n", it.Hint)
		}
	}
	fmt.Fprintln(w)
	if r.OK() {
		fmt.Fprintln(w, "结论: 系统依赖已就绪，服务端功能可用。")
		return
	}
	fmt.Fprintf(w, "结论: 仍缺 %s —— 服务端对应功能不可用；按上面的「处置」逐项处理后重跑本命令。\n",
		strings.Join(r.MissingKeys(), ", "))
}

// Options 是自检/修复的参数。
type Options struct {
	// Logger 日志器；nil 时使用 slog.Default()。
	Logger *slog.Logger
	// ConfigFSRoot LIO 在 configfs 中的根；空时使用 /sys/kernel/config/target。
	ConfigFSRoot string
	// Repair 允许自动修复（挂载 configfs、加载/重载模块、写 modules-load.d）。
	Repair bool
	// Install 允许在工具缺失时自动安装对应软件包（需要 root 与可用包管理器/网络）。
	Install bool
	// Timeout 单条外部命令（mount/modprobe）的超时；空/<=0 时用 30s。
	Timeout time.Duration
	// InstallTimeout 单次装包的超时；空/<=0 时用 5min。
	InstallTimeout time.Duration
}

// normalize 补默认值，返回内部使用的副本。
func (o Options) normalize() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if strings.TrimSpace(o.ConfigFSRoot) == "" {
		o.ConfigFSRoot = defaultConfigFSRoot
	}
	o.ConfigFSRoot = strings.TrimSuffix(o.ConfigFSRoot, "/")
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.InstallTimeout <= 0 {
		o.InstallTimeout = defaultInstallTimeout
	}
	return o
}

// Detect 只探测、不做任何修复（配置开关与 flag 都关掉时的路径）。
func Detect(_ context.Context, opt Options) *Report {
	o := opt.normalize()
	o.Repair = false
	o.Install = false
	return newReport(o, probeAll(o), time.Now())
}

// Ensure 探测 → 修复 → 复检，返回最终报告。
//
// Repair/Install 为 true 时才做修复；修复动作全部幂等，重复调用安全。
func Ensure(ctx context.Context, opt Options) *Report {
	o := opt.normalize()
	start := time.Now()

	items := probeAll(o)
	wasMissing := map[string]bool{}
	for _, it := range items {
		if it.Status == StatusMissing {
			wasMissing[it.Key] = true
		}
	}

	var fixes, errs map[string]string
	if o.Repair && len(wasMissing) > 0 {
		fixes, errs = repairAll(ctx, o, items)
		items = probeAll(o)
	}

	for i := range items {
		key := items[i].Key
		if wasMissing[key] && items[i].Status == StatusOK {
			items[i].Status = StatusFixed
			act := fixes[key]
			if act == "" {
				act = "已由前置修复动作（挂载 configfs / 加载模块）一并解决"
			}
			items[i].Fixed = act
		}
		// 尝试过修复但复检仍缺失：把失败原因挂在 Hint 前面，避免运维只能看到"还是缺"。
		if wasMissing[key] && items[i].Status == StatusMissing {
			if reason := errs[key]; reason != "" {
				items[i].Hint = "本次自动修复失败: " + reason + "；" + items[i].Hint
			}
		}
	}
	return newReport(o, items, start)
}

func newReport(o Options, items []Item, start time.Time) *Report {
	return &Report{
		CheckedAt:      time.Now(),
		ElapsedMS:      time.Since(start).Milliseconds(),
		Root:           isRoot(),
		RepairEnabled:  o.Repair,
		InstallEnabled: o.Install,
		ConfigFSRoot:   o.ConfigFSRoot,
		Items:          items,
	}
}

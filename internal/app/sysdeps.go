package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
)

// SysDepsItem 是一项系统依赖的探测结果。
//
// 这是**平台中立的 DTO**：Linux 侧由 internal/platform/sysdeps 填充；
// Windows 没有这类"进程外依赖"（能力由 Hyper-V/IscsiTarget 角色承担），
// 报告里 Supported=false、Items 为空，前端据此不显示横幅。
type SysDepsItem struct {
	// Key 稳定标识，如 configfs_mount / kernel_modules / lio_tools / lvm_tools。
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
	// Installable 该项是否支持"由服务端按需安装"（前端据此决定是否显示安装按钮）。
	//
	// 由平台层一次性判定（含"是否 root"与"配置是否允许装包"），app 层不重复判断：
	// 判据只留一处，避免出现"按钮可点、一点就失败"。
	Installable bool `json:"installable"`
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

// ---- 按需安装（管理端"安装"按钮）----

// SysDepsInstaller 是"按需安装系统依赖"的平台能力。
//
// 由平台层注入（Linux 见 cmd/vault-server/platform_linux.go 的适配器）；Windows 为 nil——
// 那边没有这类"进程外依赖"，相关能力由 Hyper-V / iSCSI 目标角色承担。
//
// Check 与 Install 必须来自**同一份判据**（同一个 sysdeps.Options 快照），
// 否则会出现"横幅说能装、点下去被拒"的自相矛盾。
type SysDepsInstaller interface {
	// Check 提交任务前的前置条件检查（只读，不改动系统）。
	// 不可安装 / 未开启装包 / 非 root / 无包管理器时返回带错误码的 error。
	Check(key string) error
	// Install 执行安装，返回本次执行的安装命令（进审计与任务日志）。
	Install(ctx context.Context, key string) (string, error)
}

// SysDepsInstallPayload 是 install_deps 任务的参数。
type SysDepsInstallPayload struct {
	// Key 依赖项标识，与 GET /v1/system/deps 的 items[].key 一致（如 lio_tools）。
	Key string `json:"key"`
}

// InstallSysDeps 提交"按需安装系统依赖"任务。
//
// 前置检查刻意放在**提交前**，而不是留给任务失败：任务的对外视图只有一个 failed 布尔
// （见 internal/api 的 toJobDTO，底层错误文本只进日志），若把"装包开关被关掉"这种此刻就能
// 确定的理由拖到任务里，用户只剩一句"任务失败"，无从判断是权限、网络还是配置。
// 这里直接以稳定错误码返回，前端可翻译成"需要 root"或"配置已关闭自动安装"。
func (a *App) InstallSysDeps(ctx context.Context, key string) (*domain.Job, error) {
	if a.SysDepsInstall == nil {
		return nil, apperr.PlatformUnsupported().WithArg("key", key)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, apperr.InvalidParam("key")
	}
	if err := a.SysDepsInstall.Check(key); err != nil {
		return nil, err
	}

	// 幂等键**留空**：idem_key 上是永久唯一索引（store 的 idx_jobs_idem），用固定键的话，
	// 第一次装失败后用户再点只会一遍遍拿回那条历史失败任务，永远装不上。
	// 重复提交由两处兜住：前端在任务进行中禁用按钮；后端用宿主机级锁让并发装包串行
	// （包管理器自身也只有一把锁，并行只会互相撞锁、留下装到一半的状态）。
	j, _, err := a.Jobs.EnqueueWith(ctx, domain.JobInstallDeps, key, "",
		lock.HostKey("sysdeps"), SysDepsInstallPayload{Key: key})
	if err != nil {
		return nil, err
	}
	a.Log.Info("已提交系统依赖安装任务", "key", key, "job_id", j.ID)
	return j, nil
}

// registerSysDepsHandlers 注册系统依赖安装任务处理器。
func (a *App) registerSysDepsHandlers(w *job.Worker) {
	w.Register(job.HandlerFunc{T: domain.JobInstallDeps, F: a.runInstallSysDeps})
}

// runInstallSysDeps 执行安装任务。
func (a *App) runInstallSysDeps(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	key := j.RefID
	if j.Payload != "" {
		var payload SysDepsInstallPayload
		if err := json.Unmarshal([]byte(j.Payload), &payload); err != nil {
			return apperr.Wrap(err, apperr.CodeInvalidParam, http.StatusBadRequest)
		}
		if payload.Key != "" {
			key = payload.Key
		}
	}
	if a.SysDepsInstall == nil {
		return apperr.PlatformUnsupported().WithArg("key", key)
	}
	// 进度只给"阶段"：装包耗时集中在拉取与解包，中途没有可信的百分比可报。
	rep.Progress(10)
	act, err := a.SysDepsInstall.Install(ctx, key)
	if act != "" {
		a.Log.Info("系统依赖安装命令已执行", "key", key, "cmd", act, "job_id", j.ID)
	}
	if err != nil {
		return err
	}
	rep.Progress(100)
	return nil
}

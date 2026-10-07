//go:build linux

package sysdeps

import (
	"context"
	"errors"

	"vault/internal/apperr"
)

// 安装前置条件的稳定原因。
//
// 做成哨兵错误而不是直接返回文案：同一份判定有两个消费方，口径必须一致——
//   - 启动期自动修复（repairAll）把 err.Error() 拼进报告的 Hint（给人看）；
//   - 管理端"安装"按钮的提交前检查要把原因映射成**稳定错误码**（给前端 i18n）。
//
// 若两处各写一份判定，迟早漂移（例如某天只在一处补上"非 root 也要拒绝"），
// 表现为"横幅显示能装、点了却失败"。
var (
	// ErrInstallDisabled 配置明确关闭了装包（platform.auto_install=false）。
	ErrInstallDisabled = errors.New("未开启自动安装（platform.auto_install=false 或 doctor -install=false）")
	// ErrInstallNotRoot 非 root 调不动发行版包管理器。
	ErrInstallNotRoot = errors.New("需要 root 才能安装软件包")
	// ErrInstallNoPkgManager 宿主机上没有受支持的包管理器。
	ErrInstallNoPkgManager = errors.New("未找到受支持的包管理器（apt-get/dnf/yum/zypper/apk/pacman）")
)

// installSpecsForKey 返回某个依赖项 Key 当前需要的工具清单；第二个返回值表示该 Key 是否支持按需安装。
//
// 只覆盖**工具类**项：内核模块与 configfs 挂载（kernel_modules / configfs_mount）不是
// "装个包"能解决的，它们由 repairAll 里的 modprobe / mount 分支处理。
//
// 与 repairAll 有一处刻意差异：misc_tools（可选工具）**不在自动修复范围内**——
// 不替使用者决定给机器装"可有可无"的包；但用户在管理端点"安装"是一次明确授权，故这里允许。
func installSpecsForKey(key string) ([]toolSpec, bool) {
	switch key {
	case "lio_tools":
		return lioTools, true
	case "lvm_tools":
		return lvmTools, true
	case "fs_tools":
		// 按机器现状选族（已有 xfs 就不再多装 e2fsprogs），与自动修复同源。
		return fsRepairSpecs(), true
	case "misc_tools":
		return miscTools, true
	default:
		return nil, false
	}
}

// Installable 报告该依赖项是否支持"按需安装"（前端据此决定是否显示安装按钮）。
func Installable(key string) bool {
	_, ok := installSpecsForKey(key)
	return ok
}

// CheckInstallable 做安装前的**只读**前置检查，不改动系统。
//
// 单独暴露它，是为了让管理端在提交任务前就拿到确定的拒绝理由（带稳定错误码、可翻译），
// 而不是提交一个必然失败的任务、再让用户对着"任务失败"猜原因：任务的对外视图只有
// failed 布尔（见 internal/api 的 toJobDTO，刻意不外泄底层错误文本）。
func CheckInstallable(opt Options, key string) error {
	o := opt.normalize()
	specs, ok := installSpecsForKey(key)
	if !ok {
		return apperr.SysDepsNotInstallable(key)
	}
	// 已经齐了：不是错误——安装任务会直接空转成功（幂等），没必要为它拒绝请求。
	if len(missingTools(specs)) == 0 {
		return nil
	}
	// 顺序有意：先报"被配置关掉"再报权限。反过来的话，用户会去查权限、
	// 而根因其实是自己把 platform.auto_install 设成了 false。
	if !o.Install {
		return apperr.SysDepsInstallDisabled()
	}
	if !isRoot() {
		return apperr.SysDepsInstallNotRoot()
	}
	if detectPkgManager() == nil {
		return apperr.SysDepsNoPackageManager()
	}
	return nil
}

// InstallByKey 安装某个依赖项当前缺失的工具包，返回本次执行的安装命令（供审计与展示）。
//
// 供管理端"安装"按钮经后台任务调用（domain.JobInstallDeps）。ctx 取消会连带终止正在运行的
// 包管理器进程（runCmdEnv 基于 exec.CommandContext），不必空等装包的 5 分钟超时。
func InstallByKey(ctx context.Context, opt Options, key string) (string, error) {
	o := opt.normalize()
	specs, ok := installSpecsForKey(key)
	if !ok {
		return "", apperr.SysDepsNotInstallable(key)
	}
	act, err := installPackages(ctx, o, missingTools(specs))
	if err == nil {
		return act, nil
	}
	// 前置条件类失败已是稳定文案，转成错误码；其余是装包命令本身失败（源不可达、
	// 包名与发行版不匹配…），底层输出只进日志（runCmdEnv 负责），对外只给一个可翻译的码，
	// 避免把宿主机的软件源地址、包名等细节泄露给客户端（见 6.5）。
	switch {
	case errors.Is(err, ErrInstallDisabled):
		return act, apperr.SysDepsInstallDisabled()
	case errors.Is(err, ErrInstallNotRoot):
		return act, apperr.SysDepsInstallNotRoot()
	case errors.Is(err, ErrInstallNoPkgManager):
		return act, apperr.SysDepsNoPackageManager()
	default:
		return act, apperr.SysDepsInstallFailed().WithCause(err)
	}
}

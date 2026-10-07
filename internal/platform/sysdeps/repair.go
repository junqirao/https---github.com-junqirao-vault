//go:build linux

package sysdeps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 包管理器族：决定"某个工具该装哪个包"。
const (
	familyAPT    = "apt"
	familyRPM    = "rpm"
	familyAPK    = "apk"
	familyPacman = "pacman"
)

// pkgManager 描述一种受支持的包管理器及其安装命令前缀。
type pkgManager struct {
	family string
	exe    string
	args   []string
	env    []string
}

// pkgManagers 按探测优先级排列（同一台机器上通常只有一个存在）。
var pkgManagers = []pkgManager{
	{family: familyAPT, exe: "apt-get", args: []string{"install", "-y", "--no-install-recommends"},
		env: []string{"DEBIAN_FRONTEND=noninteractive"}},
	{family: familyRPM, exe: "dnf", args: []string{"install", "-y"}},
	{family: familyRPM, exe: "yum", args: []string{"install", "-y"}},
	{family: familyRPM, exe: "zypper", args: []string{"--non-interactive", "install", "--no-recommends"}},
	{family: familyAPK, exe: "apk", args: []string{"add", "--no-cache"}},
	{family: familyPacman, exe: "pacman", args: []string{"-S", "--noconfirm", "--needed"}},
}

// detectPkgManager 返回当前机器可用的包管理器；都没有时返回 nil。
func detectPkgManager() *pkgManager {
	for i := range pkgManagers {
		if _, err := exec.LookPath(pkgManagers[i].exe); err == nil {
			return &pkgManagers[i]
		}
	}
	return nil
}

// repairAll 按**探测顺序**逐项尝试修复，返回 (各项的修复动作说明, 各项的失败原因)。
//
// 顺序很重要：先挂 configfs、再加载缺失模块，最后才是"跑不通/缺包"这类动作；反过来只会白试。
//
// 只做**幂等且不会拆掉在线目标**的动作。曾经这里还有一条"重载 iscsi_target_mod 让 configfs
// 目录重新注册"的修复，现已删除：那类目录是 rtslib 按需创建的内部细节（见 probeLioTools 的说明），
// 而 modprobe -r iscsi_target_mod 会把**正在给客户端服务的整棵目标配置抹掉**，代价远大于收益。
// 需要重载时由运维手工执行（scripts/setup.sh 也已不再自动重载），并接受在线目标的短暂中断。
func repairAll(ctx context.Context, o Options, items []Item) (map[string]string, map[string]string) {
	fixes := map[string]string{}
	errs := map[string]string{}
	for _, it := range items {
		if it.Status != StatusMissing {
			continue
		}
		var (
			act string
			err error
		)
		switch it.Key {
		case "kernel_modules":
			act, err = repairKernelModules(ctx, o)
		case "configfs_mount":
			act, err = repairConfigFSMount(ctx, o)
		case "lio_tools":
			act, err = repairTools(ctx, o, lioTools)
		case "lvm_tools":
			act, err = repairTools(ctx, o, lvmTools)
		case "fs_tools":
			act, err = repairFSTools(ctx, o)
		case "misc_tools":
			// 可选工具**不自动安装**：不为"可有可无"的能力给机器装包，
			// Hint 里已经给出可直接复制的安装命令。
			continue
		default:
			// root_privilege 之类无法自动修复，也不该"修复"。
			continue
		}
		if act != "" {
			fixes[it.Key] = act
		}
		if err != nil {
			errs[it.Key] = err.Error()
		}
	}
	return fixes, errs
}

// repairKernelModules 加载缺失的 LIO 模块，并写入开机自动加载文件。
func repairKernelModules(ctx context.Context, o Options) (string, error) {
	loaded := readLoadedModules()
	rel := readOSRelease()

	var done, failed []string
	for _, m := range lioModules {
		if loaded[m] {
			continue
		}
		if !moduleAvailable(rel, m) {
			failed = append(failed, m+"（内核未提供模块文件）")
			continue
		}
		if out, err := runCmd(ctx, o, o.Timeout, "modprobe", m); err != nil {
			failed = append(failed, fmt.Sprintf("%s（%s）", m, firstLine(out)))
			continue
		}
		done = append(done, m)
	}

	var acts []string
	if len(done) > 0 {
		acts = append(acts, "modprobe "+strings.Join(done, " "))
	}
	if changed, err := ensureModulesLoadFile(); err != nil {
		o.Logger.Warn("写入 modules-load.d 失败（本次加载仍已生效，但重启后不会自动加载）",
			"file", modulesLoadFile, "error", err.Error())
	} else if changed {
		acts = append(acts, "已写入 "+modulesLoadFile+"（开机自动加载）")
	}

	if len(failed) > 0 {
		return strings.Join(acts, "；"), fmt.Errorf("加载失败: %s", strings.Join(failed, ", "))
	}
	return strings.Join(acts, "；"), nil
}

// repairConfigFSMount 挂载 configfs 到 <configfs_root> 的父目录。
func repairConfigFSMount(ctx context.Context, o Options) (string, error) {
	mp := mountPointFor(o.ConfigFSRoot)
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return "", fmt.Errorf("创建挂载点 %s 失败: %w", mp, err)
	}
	act := fmt.Sprintf("mount -t configfs none %s", mp)
	out, err := runCmd(ctx, o, o.Timeout, "mount", "-t", "configfs", "none", mp)
	if err == nil {
		return act, nil
	}
	// mount 在"已经挂好了"时会返回失败，这里按幂等处理：复检会给出真实结论。
	if _, fstype, ok := findMountFor(o.ConfigFSRoot); ok && fstype == "configfs" {
		return act + "（configfs 原本已挂载）", nil
	}
	return act, fmt.Errorf("%s 失败: %s", act, firstLine(out))
}

// repairTools 安装缺失工具对应的软件包（需要 o.Install 与 root）。
func repairTools(ctx context.Context, o Options, specs []toolSpec) (string, error) {
	missing := missingTools(specs)
	if len(missing) == 0 {
		return "", nil
	}
	return installPackages(ctx, o, missing)
}

// repairFSTools 文件系统工具的自愈（走 fsRepairSpecs 的选族结论）。
func repairFSTools(ctx context.Context, o Options) (string, error) {
	return installPackages(ctx, o, missingTools(fsRepairSpecs()))
}

// fsRepairSpecs 返回"该补哪一族文件系统工具"的清单；机器已够用时返回 nil。
//
// 抽成独立函数是因为有两个消费方，判据必须同源：
//   - 启动期/doctor 的自动修复（repairFSTools）；
//   - 管理端"安装"按钮的按需安装（installSpecsForKey 的 fs_tools 分支）。
//
// 默认文件系统是 ext4，因此优先补 e2fsprogs；已有 xfs 一族时不动它，
// 免得为了"能建盘"顺手给机器装上并不需要的 xfsprogs。
func fsRepairSpecs() []toolSpec {
	has := func(exe string) bool { _, err := exec.LookPath(exe); return err == nil }
	if (has("mkfs.ext4") && has("resize2fs")) || (has("mkfs.xfs") && has("xfs_growfs")) {
		return nil
	}
	// 哪一族"部分存在"就先补哪一族；都没有时补 e2fsprogs。
	if (has("mkfs.xfs") || has("xfs_growfs")) && !has("mkfs.ext4") && !has("resize2fs") {
		return []toolSpec{
			{exe: "mkfs.xfs", pkg: "xfsprogs"},
			{exe: "xfs_growfs", pkg: "xfsprogs"},
		}
	}
	return []toolSpec{
		{exe: "mkfs.ext4", pkg: "e2fsprogs"},
		{exe: "resize2fs", pkg: "e2fsprogs"},
	}
}

// installPackages 调发行版包管理器安装一批工具（按 pkg 去重）。
func installPackages(ctx context.Context, o Options, specs []toolSpec) (string, error) {
	if len(specs) == 0 {
		return "", nil
	}
	// 前置条件用哨兵错误：文案不变（启动期自动修复直接把 err.Error() 展示给人看），
	// 同时让"按需安装"路径能 errors.Is 出稳定原因并映射成错误码（见 install.go）。
	if !o.Install {
		return "", ErrInstallDisabled
	}
	if !isRoot() {
		return "", ErrInstallNotRoot
	}
	pm := detectPkgManager()
	if pm == nil {
		return "", ErrInstallNoPkgManager
	}

	pkgs := uniquePackages(specs, pm.family)
	args := append(append([]string{}, pm.args...), pkgs...)
	act := pm.exe + " " + strings.Join(args, " ")
	out, err := runCmdEnv(ctx, o, o.InstallTimeout, pm.env, pm.exe, args...)
	if err != nil {
		return act, fmt.Errorf("%s 失败: %s", act, firstLine(out))
	}
	return act, nil
}

// missingTools 过滤出 PATH 里找不到的那些工具。
func missingTools(specs []toolSpec) []toolSpec {
	var out []toolSpec
	for _, s := range specs {
		if _, err := exec.LookPath(s.exe); err != nil {
			out = append(out, s)
		}
	}
	return out
}

// uniquePackages 按包管理器族把工具映射成去重后的包名（保序）。
func uniquePackages(specs []toolSpec, family string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range specs {
		pkg := s.pkgFor(family)
		if pkg == "" || seen[pkg] {
			continue
		}
		seen[pkg] = true
		out = append(out, pkg)
	}
	return out
}

// installHint 生成"人工怎么装"的提示（自动装包不可用时展示给运维）。
func installHint(specs []toolSpec) string {
	pm := detectPkgManager()
	if pm == nil {
		return "未找到受支持的包管理器；请手工安装这些工具：" + strings.Join(toolNames(specs), ", ")
	}
	pkgs := uniquePackages(specs, pm.family)
	if len(pkgs) == 0 {
		return "无需安装（包名未知，请手工确认）"
	}
	return fmt.Sprintf("%s %s %s", pm.exe, strings.Join(pm.args, " "), strings.Join(pkgs, " "))
}

// toolNames 提取工具名（用于提示文案）。
func toolNames(specs []toolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.exe)
	}
	return out
}

// ensureModulesLoadFile 写入/纠正开机自动加载模块的文件；返回是否发生了改动。
func ensureModulesLoadFile() (bool, error) {
	const header = "# 由 vault-server 生成：LIO（iSCSI target）所需内核模块，请勿删改本文件。\n"
	want := header + strings.Join(lioModules, "\n") + "\n"

	if raw, err := os.ReadFile(modulesLoadFile); err == nil {
		present := map[string]bool{}
		for _, line := range strings.Split(string(raw), "\n") {
			present[strings.TrimSpace(line)] = true
		}
		complete := true
		for _, m := range lioModules {
			if !present[m] {
				complete = false
				break
			}
		}
		if complete {
			return false, nil // 三个模块都已列出：绝不重写运维的文件
		}
	}
	if err := os.MkdirAll(filepath.Dir(modulesLoadFile), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(modulesLoadFile, []byte(want), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// ---- 命令执行 ----

// runCmd 在超时约束下执行一条外部命令，返回合并输出（已 TrimSpace）。
func runCmd(ctx context.Context, o Options, timeout time.Duration, name string, args ...string) (string, error) {
	return runCmdEnv(ctx, o, timeout, nil, name, args...)
}

// runCmdEnv 同 runCmd，可附加环境变量（如 apt 的 DEBIAN_FRONTEND=noninteractive）。
//
// 参数一律以切片传给 exec.CommandContext，**绝不拼接 shell 字符串**。
func runCmdEnv(ctx context.Context, o Options, timeout time.Duration, env []string, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		o.Logger.Warn("系统依赖修复命令失败",
			"cmd", name, "args", args, "error", err.Error(), "output", limitText(text))
		return text, err
	}
	o.Logger.Info("系统依赖修复命令成功", "cmd", name, "args", args, "output", limitText(text))
	return text, nil
}

// logTextLimit 进入日志的命令输出上限，避免装包日志把日志文件刷爆。
const logTextLimit = 2048

// limitText 截断命令输出。
func limitText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= logTextLimit {
		return s
	}
	return s[:logTextLimit] + "...(truncated)"
}

// firstLine 取输出的第一行（错误提示里只带关键信息，不带整段输出）。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 300 {
				return line[:300] + "..."
			}
			return line
		}
	}
	return ""
}

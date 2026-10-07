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

const (
	// defaultConfigFSRoot configfs 中 target 子系统的默认位置（与 liotarget 保持一致）。
	defaultConfigFSRoot = "/sys/kernel/config/target"
	// modulesLoadFile 开机自动加载模块的持久化文件（本工具负责写入/纠正）。
	modulesLoadFile = "/etc/modules-load.d/vault-lio.conf"
	// procModulesPath 已加载模块列表（比调 lsmod 更省事，容器里也不一定有 lsmod）。
	procModulesPath = "/proc/modules"
	// procMountsPath 挂载表（用于判定 configfs 是否已挂载及其挂载点）。
	procMountsPath = "/proc/mounts"
	// osReleasePath 当前内核版本（用于定位 /lib/modules/<ver>/modules.dep）。
	osReleasePath = "/proc/sys/kernel/osrelease"

	// defaultTimeout 单条外部命令（mount/modprobe）的超时。
	defaultTimeout = 30 * time.Second
	// probeCmdTimeout 只读探针命令的超时；targetcli version 正常是毫秒级，
	// 卡住时必须很快放弃，不能让 /v1/system/deps 跟着一起卡。
	probeCmdTimeout = 10 * time.Second
	// defaultInstallTimeout 单次装包的超时（apt/dnf 首次装包可能很慢）。
	defaultInstallTimeout = 5 * time.Minute
)

// lioModules 是 LIO 服务端所需的三个内核模块，顺序即加载顺序：
// target_core_mod 提供 core 子系统（backstore 根），iscsi_target_mod 提供 iscsi fabric，
// target_core_iblock 提供 iblock_0 块设备 backstore 插件（缺它建盘必失败）。
var lioModules = []string{"target_core_mod", "iscsi_target_mod", "target_core_iblock"}

// toolSpec 是一个外部命令依赖，以及提供它的发行版包名。
//
// pkg 取"跨发行版同名"的包（lvm2/util-linux/e2fsprogs/xfsprogs/kmod/ntfs-3g）；
// 少数 rpm 系命名不同的用 pkgRPM 覆盖（如 thin_ls 在 RHEL 属于 device-mapper-persistent-data）。
type toolSpec struct {
	exe    string
	pkg    string
	pkgRPM string
}

// pkgFor 返回在给定包管理器族下应安装的包名。
func (t toolSpec) pkgFor(family string) string {
	if family == familyRPM && t.pkgRPM != "" {
		return t.pkgRPM
	}
	return t.pkg
}

// 必需工具：缺任何一项，"存储/磁盘"功能都不完整。
var lvmTools = []toolSpec{
	{exe: "lvm", pkg: "lvm2"},
	{exe: "lvcreate", pkg: "lvm2"},
	{exe: "lvchange", pkg: "lvm2"},
	{exe: "lvs", pkg: "lvm2"},
	{exe: "dmsetup", pkg: "lvm2"},
}

// LIO iSCSI 目标的命令行工具：**必需**。
//
// targetcli（及其 rtslib）是本后端实际的执行体：建目标/ACL/LUN 映射、写 TPG 属性与
// 只读属性，全部由它下发到 configfs。它**不是**可有可无的便利包装——缺了它 iSCSI 目标
// 功能完全不可用，所以列为必需项，让 doctor 与前端横幅能提前把它报出来，
// 而不是等业务路径上才失败。
//
// 包名跨发行版不同：Debian/Ubuntu 走 targetcli-fb（上游自由分支），RHEL/Fedora 走 targetcli。
var lioTools = []toolSpec{
	{exe: "targetcli", pkg: "targetcli-fb", pkgRPM: "targetcli"},
}

// 可选工具：只在特定场景用到（NTFS 卷、thin 元数据检查），缺失不影响主流程。
var miscTools = []toolSpec{
	{exe: "thin_ls", pkg: "thin-provisioning-tools", pkgRPM: "device-mapper-persistent-data"},
	{exe: "thin_check", pkg: "thin-provisioning-tools", pkgRPM: "device-mapper-persistent-data"},
	{exe: "ntfsfix", pkg: "ntfs-3g"},
	{exe: "ntfslabel", pkg: "ntfs-3g"},
	{exe: "fstrim", pkg: "util-linux"},
	{exe: "wipefs", pkg: "util-linux"},
}

// probeAll 按固定顺序探测所有项。顺序有意义：修复时也按此顺序执行，
// 保证"先挂 configfs、再加载模块、最后看 LIO 执行体能不能跑通"。
func probeAll(o Options) []Item {
	return []Item{
		probeRoot(o),
		probeKernelModules(o),
		probeConfigFSMount(o),
		probeLioTools(o),
		probeTools("lvm_tools", "LVM2 工具链", lvmTools, true),
		probeFSTools(),
		probeTools("misc_tools", "其它辅助工具", miscTools, false),
	}
}

// probeRoot 检查是否以 root 运行：configfs 写入、modprobe、挂载、装包都需要 root。
func probeRoot(_ Options) Item {
	it := Item{Key: "root_privilege", Title: "以 root 运行", Required: true, Status: StatusOK}
	uid := os.Geteuid()
	if uid == 0 {
		it.Detail = "uid=0"
		return it
	}
	it.Status = StatusMissing
	it.Detail = fmt.Sprintf("uid=%d", uid)
	it.Hint = "以 root 运行（systemd 单元的 User=root），否则无法挂载 configfs、加载模块与写 /etc"
	return it
}

// probeKernelModules 检查 LIO 三个内核模块是否已加载、模块文件是否存在。
func probeKernelModules(_ Options) Item {
	it := Item{Key: "kernel_modules", Title: "LIO 内核模块", Required: true, Status: StatusOK}
	loaded := readLoadedModules()
	rel := readOSRelease()

	var okNames, missing, absent []string
	for _, m := range lioModules {
		if loaded[m] {
			okNames = append(okNames, m)
			continue
		}
		missing = append(missing, m)
		if !moduleAvailable(rel, m) {
			absent = append(absent, m)
		}
	}

	switch {
	case len(missing) == 0:
		it.Detail = "已加载: " + strings.Join(okNames, ", ")
	case len(okNames) == 0:
		it.Detail = "三个模块均未加载: " + strings.Join(missing, ", ")
	default:
		it.Detail = fmt.Sprintf("已加载: %s；未加载: %s",
			strings.Join(okNames, ", "), strings.Join(missing, ", "))
	}

	if len(missing) == 0 {
		return it
	}
	it.Status = StatusMissing
	if len(absent) > 0 {
		// 这种情况**本进程修不了**：模块文件根本不存在，自动装包只覆盖命令行工具对应的包。
		// 所以必须把"人工/脚本兜底"的路子写清楚——发行版包名 + 包内的 setup.sh。
		it.Hint = fmt.Sprintf("内核未提供模块文件（%s）：Debian/Ubuntu 可试 apt-get install linux-modules-extra-$(uname -r)、"+
			"RHEL 系试 dnf install kernel-modules-extra；或直接执行包内 sudo ./scripts/setup.sh 一键装齐；"+
			"容器部署必须共享宿主机的 /lib/modules 且与宿主内核版本一致", strings.Join(absent, ", "))
	} else {
		it.Hint = "执行 modprobe " + strings.Join(missing, " ")
	}
	return it
}

// probeConfigFSMount 检查 configfs 是否挂载在 <configfs_root> 的父目录上。
func probeConfigFSMount(o Options) Item {
	it := Item{Key: "configfs_mount", Title: "configfs 已挂载", Required: true, Status: StatusOK}
	mp := mountPointFor(o.ConfigFSRoot)
	mounted, fstype, ok := findMountFor(o.ConfigFSRoot)
	if ok && fstype == "configfs" {
		it.Detail = fmt.Sprintf("configfs 已挂载于 %s（覆盖 %s）", mounted, o.ConfigFSRoot)
		return it
	}
	it.Status = StatusMissing
	if ok {
		it.Detail = fmt.Sprintf("%s 不在 configfs 挂载点内：最近的挂载点是 %s（类型 %s）", o.ConfigFSRoot, mounted, fstype)
	} else {
		it.Detail = fmt.Sprintf("%s 没有任何覆盖它的挂载点", o.ConfigFSRoot)
	}
	it.Hint = fmt.Sprintf("mount -t configfs none %s（写入 /etc/fstab 可开机自动挂载；容器需从宿主机 bind 进去）", mp)
	return it
}

// configfsInstanceMismatch 判定"configfs 已挂载，但连 <configfs_root> 这个 LIO 子系统根都不存在"。
//
// 这一条能**直接定性**：target_core_mod 的 init 会向内核注册 target 子系统，注册成功才会
// 出现 <configfs_root>（注册失败模块就加载不进来，modprobe 会报错）。因此
// 「模块在 /proc/modules 里 + configfs 已挂载 + <configfs_root> 整个不存在」只有一个解释：
// 本进程看到的 configfs 与内核注册 LIO 的那一份**不是同一个实例**——容器里自己 mount 的
// 那份 configfs 是空的，宿主机那份才有 target。
//
// 这类故障重载模块永远修不好（注册一直是成功的，只是落在另一份实例上），反而会把宿主机
// 正在对外服务的目标一并拆掉。因此它只作为 targetcli 跑不通时的**辅助判据**出现在 hint 里，
// 不再作为任何一项的就绪条件，也没有任何自动修复动作。
func configfsInstanceMismatch(o Options) bool {
	if isDir(o.ConfigFSRoot) {
		return false
	}
	_, fstype, ok := findMountFor(o.ConfigFSRoot)
	return ok && fstype == "configfs"
}

// hintInstanceMismatch 给出"configfs 实例错位"的处置建议。
//
// 刻意把"重载模块没用"写进文案：现场最容易的误判就是按旧提示反复 modprobe -r / 重跑脚本，
// 而那一步在宿主机正在服务 iSCSI 时是**破坏性**的（会拆掉正在被客户端使用的目标）。
func hintInstanceMismatch(o Options) string {
	mp := mountPointFor(o.ConfigFSRoot)
	return "configfs 已挂载、target_core_mod 也已加载，但连 " + o.ConfigFSRoot +
		" 都不存在——模块注册是成功的（否则该目录不会出现），所以这不是内核模块或 targetcli 的问题：" +
		"本进程看到的 configfs 与内核注册 LIO 的那一份不是同一个实例（容器里自己 mount 的那份是空的，宿主机那份才有 target）。" +
		"重载模块修不好这一点，且会拆掉宿主机正在服务的目标，请勿再试。" +
		"处置：容器以 --privileged 运行并把宿主机的 " + mp + " bind 进容器（docker run -v " + mp + ":" + mp + ":rslave），" +
		"或把服务端直接装在宿主机（裸机/虚拟机）上；非容器时确认只有一份 configfs 挂载（mount | grep configfs）"
}

// probeLioTools 检查 LIO iSCSI 目标的执行体：targetcli 既要在 PATH 里，也要能真正跑通。
//
// 判据刻意选"行为"，而不是"configfs 里的某个路径在不在"。
// 曾经检查 <configfs_root>/iscsi 与 <configfs_root>/core/iblock_0 是否存在，结果是把正常机器
// 当成缺项：这两个目录都是靶场外的内部细节——core/iblock_0 要等第一个 iblock backstore 建出来
// 才由 rtslib 创建，内核注册时机也随版本不同——拿它们当就绪条件，横幅就会一直报红，
// 而 targetcli 明明是好的。用户看到的"装了还是不可用"有一半来自这里。
//
// targetcli 启动时就会构造 RTSRoot() 读整棵 configfs 树，所以"它能跑通"已经覆盖了
// 「configfs 挂上了 + rtslib 能读到 LIO 树」两件事——它才是这套栈的真正判据。
func probeLioTools(o Options) Item {
	it := Item{Key: "lio_tools", Title: "LIO 目标命令行（targetcli）", Required: true, Status: StatusOK}
	exe, err := exec.LookPath("targetcli")
	if err != nil {
		it.Status = StatusMissing
		it.Detail = "PATH 中没有 targetcli"
		it.Hint = "安装 targetcli（Debian/Ubuntu: targetcli-fb，RHEL/Fedora/SUSE: targetcli）后重试"
		return it
	}
	out, err := probeCmd(o, exe, "version")
	if err != nil {
		it.Status = StatusMissing
		it.Detail = "targetcli 存在但跑不通：" + textOrNone(lastMeaningfulLine(out))
		if configfsInstanceMismatch(o) {
			it.Hint = hintInstanceMismatch(o)
		} else {
			mp := filepath.Dir(o.ConfigFSRoot)
			it.Hint = "确认 configfs 已挂载（mount -t configfs none " + mp + "），再按 targetcli 自己的报错修" +
				"（rtslib 缺失、Python 环境损坏、权限不足等）；iSCSI 就绪与否只看它能不能跑通"
		}
		return it
	}
	it.Detail = "targetcli 可执行且能读到 configfs：" + textOrNone(meaningfulLine(out))
	return it
}

// meaningfulLine 从探针输出里挑一行**有信息量**的：跳过空行与 rtslib 的无害告警。
//
// 必须跳过的原因（现场实测）：targetcli 首次运行会往 stderr 打
//
//	Warning: Could not load preferences file /root/.targetcli/prefs.bin.
//
// 它与"能不能用"毫无关系，却排在版本号前面——直接取首行等于把这句噪声当成检测结果贴给用户，
// 报告里那行 ok 看着像警告，反而让人以为 targetcli 有问题。
func meaningfulLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line == "" || probeNoise(line) {
			continue
		}
		return line
	}
	return ""
}

// lastMeaningfulLine 取**最后**一行有信息量的输出。
//
// 失败路径用它：Python 异常栈的结论（异常类与消息）在末尾，首行只有 "Traceback (most recent call last):"。
func lastMeaningfulLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" && !probeNoise(line) {
			return line
		}
	}
	return ""
}

// probeNoise 判断输出行是否只是无害的告警（rtslib 的 prefs/deprecation 之类）。
func probeNoise(line string) bool {
	l := strings.ToLower(line)
	return strings.HasPrefix(l, "warning:") || strings.HasPrefix(l, "warn:") ||
		strings.HasPrefix(l, "deprecationwarning:")
}

// probeCmd 在超时约束下**静默**执行一条只读命令，返回合并输出（已 TrimSpace）。
//
// 与 repair.go 的 runCmd 刻意分开：探针是只读动作，不该产生"系统依赖修复命令失败/成功"
// 这类修复语义的日志，也不该因为一次探测把日志刷红。
func probeCmd(o Options, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		o.Logger.Debug("依赖探针命令失败", "cmd", name, "args", args,
			"error", err.Error(), "output", limitText(string(out)))
	}
	return strings.TrimSpace(string(out)), err
}

// textOrNone 让空输出在 Detail 里也读得懂。
func textOrNone(s string) string {
	if s == "" {
		return "(无输出)"
	}
	return limitText(s)
}

// probeTools 检查一组外部命令是否都在 PATH 中。
func probeTools(key, title string, specs []toolSpec, required bool) Item {
	it := Item{Key: key, Title: title, Required: required, Status: StatusOK}
	var missing []toolSpec
	var okCnt int
	for _, s := range specs {
		if _, err := exec.LookPath(s.exe); err != nil {
			missing = append(missing, s)
			continue
		}
		okCnt++
	}
	if len(missing) == 0 {
		it.Detail = fmt.Sprintf("%d 个命令全部可用", okCnt)
		return it
	}
	names := make([]string, 0, len(missing))
	for _, s := range missing {
		names = append(names, s.exe)
	}
	it.Status = StatusMissing
	it.Detail = fmt.Sprintf("可用 %d 个；缺失: %s", okCnt, strings.Join(names, ", "))
	it.Hint = installHint(missing)
	return it
}

// probeFSTools 检查文件系统工具：mkfs.ext4/xfs 至少有一方能建文件系统，
// 并报告各自的扩容工具（resize2fs / xfs_growfs）是否齐备。
func probeFSTools() Item {
	it := Item{Key: "fs_tools", Title: "文件系统工具（mkfs/扩容）", Required: true, Status: StatusOK}
	has := func(exe string) bool { _, err := exec.LookPath(exe); return err == nil }

	ext4 := has("mkfs.ext4") && has("resize2fs")
	xfs := has("mkfs.xfs") && has("xfs_growfs")
	var present, missing []string
	for _, s := range []struct {
		ok  bool
		yes string
		no  string
	}{
		{ext4, "mkfs.ext4 + resize2fs", "e2fsprogs 不完整（缺 mkfs.ext4 或 resize2fs）"},
		{xfs, "mkfs.xfs + xfs_growfs", "xfsprogs 不完整（缺 mkfs.xfs 或 xfs_growfs）"},
	} {
		if s.ok {
			present = append(present, s.yes)
			continue
		}
		missing = append(missing, s.no)
	}
	if ext4 || xfs {
		it.Detail = fmt.Sprintf("可用: %s；未就绪: %s", strings.Join(present, " / "), strings.Join(missing, "；"))
		return it
	}
	var specs []toolSpec
	specs = append(specs,
		toolSpec{exe: "mkfs.ext4", pkg: "e2fsprogs"},
		toolSpec{exe: "resize2fs", pkg: "e2fsprogs"},
		toolSpec{exe: "mkfs.xfs", pkg: "xfsprogs"},
		toolSpec{exe: "xfs_growfs", pkg: "xfsprogs"},
	)
	it.Status = StatusMissing
	it.Detail = "mkfs.ext4 与 mkfs.xfs 都不可用，无法格式化新建的存储卷"
	it.Hint = installHint(specs)
	return it
}

// ---- 系统信息读取（尽量只读 /proc 与文件，不依赖外部命令）----

// isRoot 判断当前是否 root。
func isRoot() bool { return os.Geteuid() == 0 }

// isDir 判断路径存在且是目录。
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// readLoadedModules 解析 /proc/modules，返回已加载模块名集合。
func readLoadedModules() map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(procModulesPath)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			out[fields[0]] = true
		}
	}
	return out
}

// readOSRelease 读取当前内核版本（uname -r 的无进程版本）。
func readOSRelease() string {
	raw, err := os.ReadFile(osReleasePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// moduleAvailable 判断模块文件是否存在于当前内核的模块树中。
//
// 判据是 /lib/modules/<ver>/modules.dep 里有没有它（一次文件读取，比遍历目录树快）。
// 无法判定时（读不到内核版本或 modules.dep）返回 true——宁可让上层去 modprobe 试一次，
// 也不要在这里误报"内核没提供该模块"。
func moduleAvailable(release, module string) bool {
	if release == "" {
		return true
	}
	raw, err := os.ReadFile(fmt.Sprintf("/lib/modules/%s/modules.dep", release))
	if err != nil {
		return true
	}
	suffix := "/" + module + ".ko"
	for _, line := range strings.Split(string(raw), "\n") {
		idx := strings.Index(line, ":")
		if idx <= 0 {
			continue
		}
		field := strings.TrimSpace(line[:idx])
		if strings.HasSuffix(field, suffix) || strings.Contains(field, suffix+".") {
			return true
		}
	}
	return false
}

// findMountFor 在 /proc/mounts 中找**最长匹配**的挂载点，返回 (挂载点, 文件系统类型, 是否找到)。
//
// 为什么要"最长"：/sys 与 /sys/kernel/config 可能同时存在于挂载表里，
// 只有最长的那个才是真正覆盖该路径的文件系统。
func findMountFor(target string) (string, string, bool) {
	raw, err := os.ReadFile(procMountsPath)
	if err != nil {
		return "", "", false
	}
	best, bestType := "", ""
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mp := unescapeMountField(fields[1])
		if mp != target && !strings.HasPrefix(target, strings.TrimSuffix(mp, "/")+"/") {
			continue
		}
		if len(mp) >= len(best) {
			best, bestType = mp, fields[2]
		}
	}
	if best == "" {
		return "", "", false
	}
	return best, bestType, true
}

// unescapeMountField 还原 /proc/mounts 里的八进制转义（如空格写成 \040）。
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+4], "%o", &v); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// mountPointFor 返回 <configfs_root> 的挂载点（即其父目录）。
func mountPointFor(configFSRoot string) string { return filepath.Dir(configFSRoot) }

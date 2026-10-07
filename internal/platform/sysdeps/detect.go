//go:build linux

package sysdeps

import (
	"fmt"
	"os"
	"os/exec"
	"path"
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
// 保证"先挂 configfs、再加载模块、最后看 fabric 目录"。
func probeAll(o Options) []Item {
	return []Item{
		probeRoot(o),
		probeKernelModules(o),
		probeConfigFSMount(o),
		probeIscsiFabricDir(o),
		probeBackstoreDir(o),
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

// probeIscsiFabricDir 检查 <root>/iscsi（iSCSI fabric）是否已注册。
//
// 这一项是"模块在、fabric 目录不在"这类真实故障的判据：模块加载成功不等于
// configfs 里注册成功（目录可能被清理脚本删掉过，或注册被回滚）。
func probeIscsiFabricDir(o Options) Item {
	dir := path.Join(o.ConfigFSRoot, "iscsi")
	it := Item{Key: "lio_iscsi_fabric", Title: "iSCSI fabric 目录", Required: true, Status: StatusOK}
	if isDir(dir) {
		it.Detail = dir + " 存在"
		return it
	}
	it.Status = StatusMissing
	it.Detail = dir + " 不存在"
	if readLoadedModules()["iscsi_target_mod"] {
		it.Hint = "iscsi_target_mod 已加载但 fabric 未注册：modprobe -r iscsi_target_mod && modprobe iscsi_target_mod 可重新注册；" +
			"若无效说明模块与当前内核不匹配"
	} else {
		it.Hint = "先挂载 configfs 并加载 iscsi_target_mod（modprobe iscsi_target_mod）"
	}
	return it
}

// probeBackstoreDir 检查 <root>/core/iblock_0（块设备 backstore 插件）是否已注册。
// 缺它时服务端能起来、也能建 iSCSI 目标，但每次建盘都会失败。
func probeBackstoreDir(o Options) Item {
	dir := path.Join(o.ConfigFSRoot, "core", "iblock_0")
	it := Item{Key: "lio_backstore_plugin", Title: "块设备 backstore 插件", Required: true, Status: StatusOK}
	if isDir(dir) {
		it.Detail = dir + " 存在"
		return it
	}
	it.Status = StatusMissing
	it.Detail = dir + " 不存在"
	it.Hint = "modprobe target_core_iblock；若模块已加载仍不出现，说明模块与当前内核不匹配（重载：modprobe -r target_core_iblock && modprobe target_core_iblock）"
	return it
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

//go:build linux

package linuxlvm

import (
	"context"
	"net/http"
	"os"
	"path"
	"strings"

	"vault/internal/apperr"
)

// 磁盘布局（见 docs/implementation.md 5.16）。
//
// 新建盘一律用 **GPT + 单分区**：整盘上写 GPT，盘内一个占满全盘的分区，文件系统建在分区上。
//
// 为什么必须这样：客户端把磁盘当"磁盘 → 分区 → 卷"的模型看待（Windows 的
// Get-Disk / Get-Partition / Get-Volume 就是这套），整盘直接 mkfs 的盘在客户端上
// 读不出分区（no_partition），文件系统再正确也挂不上。
//
// 于是服务端必须"提前把分区建好"，而不是交给客户端初始化（客户端只是通过 iSCSI
// 看到一块已经分好区、格好式的裸盘）。
const (
	// layoutGPT 已经是"GPT + 单分区"布局。
	layoutGPT = "gpt"
	// layoutLegacy 是升级前的旧布局：裸文件系统直接在整盘上，**没有分区表**。
	layoutLegacy = "legacy"
	// layoutEmpty 是空盘：没有分区表也没有文件系统（本次刚建好布局）。
	layoutEmpty = "empty"
)

// partitionStart 分区起始偏移：1MiB 对齐，留出 GPT 头且对 4K 扇区盘同样安全。
const partitionStart = "1MiB"

// procFileSystemsPath 是内核已注册文件系统列表。
//
// 声明成变量（而非常量）是为了让测试能注入：判断"有没有 ntfs3 驱动"只能读这个文件，
// 而测试机上的内核状态不可控。
var procFileSystemsPath = "/proc/filesystems"

// diskFileSystem 归一化建盘用的文件系统名。
//
// 只认 ntfs / ext4：前者给 Windows 客户端，后者给（尚未实现的）Linux 客户端。
// 取值来自 domain.FileSystem，这里再校正一次，免得把任意字符串拼进 mkfs.<name>。
func diskFileSystem(fileSystem string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(fileSystem)) {
	case "ntfs":
		return "ntfs", nil
	case "ext4":
		return "ext4", nil
	default:
		return "", apperr.InvalidParam("file_system")
	}
}

// ensureDiskLayout 保证盘上是"GPT + 单分区"布局，并返回该盘的**数据设备**与原有布局。
//
// 幂等，且**绝不破坏已有数据**：
//   - 已有分区表：只找/补分区，**绝不重新 mklabel**（重写分区表会连同已有分区一起抹掉）；
//   - 无分区表但有文件系统：升级前的整盘布局（裸 NTFS 直接在整盘上），按原样使用、不转换；
//   - 既无分区表也无文件系统：才写 GPT + 单分区。
//
// 返回的 layoutGPT / layoutLegacy / layoutEmpty 供调用方记录当前实际布局。
//
// 旧版整盘布局**不做转换**：NTFS 无法无损搬进分区里（没有 in-place 转换），只能返回整盘设备
// 按原样读写：盘里的数据仍然可用，但客户端挂不上（缺分区表），需要用户重建存储库。
// **绝不**为了"布局好看"去动有数据的盘。
func (m *Manager) ensureDiskLayout(ctx context.Context, dev, fileSystem string) (string, string, error) {
	fs, err := diskFileSystem(fileSystem)
	if err != nil {
		return "", "", err
	}

	if table := m.partitionTableType(ctx, dev); table != "" {
		part, err := m.ensureFirstPartition(ctx, dev, fs)
		if err != nil {
			return "", "", err
		}
		return part, layoutGPT, nil
	}
	if existing, _ := m.blkidType(ctx, dev); existing != "" {
		m.logger.Warn("盘是旧版整盘布局（无分区表），客户端可能无法挂载，建议重建存储库",
			"dev", dev, "existing_fs", existing)
		return dev, layoutLegacy, nil
	}

	// 空盘：写 GPT 分区表 + 单分区。动手之前先确认"它确实是空盘"。
	if err := m.confirmEmptyDisk(ctx, dev); err != nil {
		return "", "", err
	}
	if err := m.createGPTPartition(ctx, dev, fs); err != nil {
		return "", "", err
	}
	part, err := m.firstPartitionDevice(ctx, dev)
	if err != nil {
		return "", "", err
	}
	if part == "" {
		return "", "", partitionMissing(dev)
	}
	m.logger.Info("已建立 GPT + 单分区布局", "dev", dev, "partition", part, "file_system", fs)
	return part, layoutEmpty, nil
}

// ensureFirstPartition 在**已有分区表**的设备上取第一个分区；没有就先重扫、再补建。
//
// 全程**不碰 mklabel**：分区表一旦被重写，盘上原有分区与数据就再也找不回来了。
func (m *Manager) ensureFirstPartition(ctx context.Context, dev, fs string) (string, error) {
	part, err := m.firstPartitionDevice(ctx, dev)
	if err != nil {
		return "", err
	}
	if part != "" {
		return part, nil
	}
	// 找不到分区常常只是"分区映射还没建立"（刚激活的 LV、udev 尚未 settle）。
	// 先重扫一次再下结论，免得把一个正常盘误判成"没有分区"。
	m.rescanPartitions(ctx, dev)
	if part, err = m.firstPartitionDevice(ctx, dev); err != nil {
		return "", err
	}
	if part != "" {
		return part, nil
	}
	// 分区表在、分区确实没有：上一次建盘在 mklabel 之后、mkpart 之前失败留下的半成品，
	// 补建一个即可（盘上没有分区，所以没有数据可丢）。不重新 mklabel。
	m.logger.Warn("盘上已有分区表但找不到分区，补建一个", "dev", dev)
	if err := m.createPartition(ctx, dev, fs); err != nil {
		return "", err
	}
	if part, err = m.firstPartitionDevice(ctx, dev); err != nil {
		return "", err
	}
	if part == "" {
		return "", partitionMissing(dev)
	}
	return part, nil
}

// confirmEmptyDisk 在写分区表**之前**再确认一次"这确实是一块空盘"。
//
// 探不到分区表也探不到文件系统，还有第三种可能：**设备此刻看不见**（映射未建立）
// 或探测本身读不到表。这种"看起来是空的"盘上贸然 mklabel，会把分区表连同里面的数据
// 一起抹掉，而分区表一没就再也找不回来了——宁可失败交给人去查。
func (m *Manager) confirmEmptyDisk(ctx context.Context, dev string) error {
	nodes, err := m.lsblkTree(ctx)
	if err != nil {
		return err
	}
	n := layoutNode(nodes, dev)
	switch {
	case n == nil:
		// 设备不在 lsblk 树里：无从判断它是不是空盘。
		return apperr.New(CodeVHDFailed, http.StatusInternalServerError).
			WithArg("reason", "device_not_found").WithArg("device", dev)
	case len(n.Children) > 0:
		// 有子设备说明盘上本来就有分区表（方才的探表结果不可信），绝不能重新 mklabel。
		return apperr.New(CodeVHDFailed, http.StatusInternalServerError).
			WithArg("reason", "partition_table_unreadable").WithArg("device", dev)
	}
	return nil
}

// partitionMissing 报告"分区表在，却看不到分区设备"。
//
// 多半是内核不支持 dm 设备分区（极老内核）：此时继续格式化整盘只会做出一块客户端
// 认不出的盘，直接失败更诚实。
func partitionMissing(dev string) error {
	return apperr.New(CodeVHDFailed, http.StatusInternalServerError).
		WithArg("reason", "partition_missing").WithArg("device", dev)
}

// createGPTPartition 在空设备上写 GPT 分区表，并建一个占满全盘的单分区。
func (m *Manager) createGPTPartition(ctx context.Context, dev, fs string) error {
	// 先清残留签名：同名 LV 被删除后又建出来时，旧的 ntfs/ext4 签名会让 parted 报警、
	// 让客户端把整盘误认成裸文件系统。wipefs 缺失（精简系统）不算失败。
	if _, err := LookPath("wipefs"); err == nil {
		if _, err := m.run(ctx, "wipefs", "-a", dev); err != nil {
			m.logger.Warn("wipefs 失败（忽略）", "dev", dev, "err", err.Error())
		}
	}
	if _, err := m.run(ctx, "parted", "-s", dev, "mklabel", "gpt"); err != nil {
		return err
	}
	return m.createPartition(ctx, dev, fs)
}

// createPartition 在**已有分区表**的设备上建一个占满全盘的分区。
//
// fs 是 parted 的**文件系统类型**，parted 据此写 GPT 分区的类型 GUID：
// ntfs → Microsoft 基本数据分区，ext4 → Linux 文件系统；随便填个占位值会让客户端
// 把分区显示成"未知分区"。盘上放不下时 parted 会失败退出——这正是想要的：绝不去动已有分区。
func (m *Manager) createPartition(ctx context.Context, dev, fs string) error {
	if _, err := m.run(ctx, "parted", "-s", "-a", "optimal", dev,
		"mkpart", "primary", fs, partitionStart, "100%"); err != nil {
		return err
	}
	m.rescanPartitions(ctx, dev)
	return nil
}

// rescanPartitions 让内核重读分区表并建立分区设备节点。
//
// 少了这一步，分区设备（/dev/dm-N 或 /dev/mapper/<vg>-<lv>N）就不存在，mkfs/mount 无从下手。
func (m *Manager) rescanPartitions(ctx context.Context, dev string) {
	switch {
	case hasExe("partx"):
		// -a 只为"尚未映射"的分区建映射；已映射时返回非 0，属正常分支。
		if out, err := m.runQuiet(ctx, "partx", "-a", dev); err != nil {
			m.logger.Debug("partx -a 未成功（可能已映射）", "dev", dev, "out", limitText(out))
		}
	case hasExe("partprobe"):
		if _, err := m.run(ctx, "partprobe", dev); err != nil {
			m.logger.Warn("partprobe 失败", "dev", dev, "err", err.Error())
		}
	}
	if hasExe("udevadm") {
		// 等 udev 把设备节点建好、软链补齐，否则紧接着的 mkfs 可能打开不存在的路径。
		if _, err := m.run(ctx, "udevadm", "settle"); err != nil {
			m.logger.Warn("udevadm settle 失败", "dev", dev, "err", err.Error())
		}
	}
}

// dropPartitionMappings 删除设备上的分区映射（幂等）。
//
// 必须在 lvchange -an / lvremove 之前调用：分区是建在 dm 设备**之上**的子设备，
// 子设备还在时内核会拒绝移除父设备（Device or resource busy），
// 盘卸载不掉、也删不掉。partx 缺失时不做任何事（老内核本就不一定有分区映射）。
func (m *Manager) dropPartitionMappings(ctx context.Context, dev string) {
	if !hasExe("partx") {
		return
	}
	if out, err := m.runQuiet(ctx, "partx", "-d", dev); err != nil {
		// 本来就没有分区映射时 partx -d 返回非 0，属正常分支。
		m.logger.Debug("partx -d 未成功（可能本就没有分区映射）", "dev", dev, "out", limitText(out))
	}
}

// partitionTableType 探测设备上的分区表类型（gpt/dos/...）；没有分区表返回空串。
func (m *Manager) partitionTableType(ctx context.Context, dev string) string {
	// blkid -p 走底层探测（不吃缓存），设备上没有分区表时返回非 0 → 空串。
	if out, err := m.runQuiet(ctx, "blkid", "-p", "-o", "value", "-s", "PTTYPE", dev); err == nil {
		if t := strings.ToLower(strings.TrimSpace(out)); t != "" {
			return t
		}
	}
	// 兜底：老版本 util-linux 的 blkid 不认 PTTYPE 时问 parted（属必需依赖，见 sysdeps）。
	//
	// 用 runQuiet："没有分区表"是 parted 的**正常答案**，但它仍打印整块设备信息后以非 0 退出
	// （`Error: <dev>: unrecognised disk label`）。空盘每次探测都会走到这里（建库前探一次、
	// 释放设备后再探一次），按 ERROR 记会让日志满是红字——真机反馈："所有操作都成功了，
	// 但是有这个日志"。判定逻辑不受影响：非 0 一律按"没探到表"处理，输出仍以 Debug 留痕。
	if out, err := m.runQuiet(ctx, "parted", "-s", dev, "print"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			v, ok := strings.CutPrefix(strings.TrimSpace(line), "Partition Table:")
			if !ok {
				continue
			}
			if t := strings.ToLower(strings.TrimSpace(v)); t != "" && t != "unknown" {
				return t
			}
		}
	}
	return ""
}

// layoutNode 在 lsblk 设备树里找 dev 对应的节点；找不到返回 nil。
//
// 不比路径字面量了事：lsblk 对 device-mapper 设备给出的 PATH 在不同版本/发行版上可能是
// /dev/mapper/<vg>-<lv>（dm 名），也可能是 /dev/dm-N（内核名）。拿不到节点就无法判断
// "盘的布局到底是什么"，而这一步的结论会决定要不要写分区表——认保守一点（多认几种写法），
// 好过把一块有数据的盘当成"设备看不见"或者反过来当成"空盘"。
func layoutNode(nodes []lsblkNode, dev string) *lsblkNode {
	if n := findNodeByPath(nodes, dev); n != nil {
		return n
	}
	// dm 设备的 lsblk NAME 就是 dm 名（=<vg>-<lv>），正好等于 dev 的 basename。
	name := path.Base(strings.TrimSpace(dev))
	if name == "" || name == "." || name == "/" {
		return nil
	}
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}

// firstPartitionDevice 返回设备上第一个分区的设备路径；没有分区时返回空串。
//
// 用 lsblk 的设备树而不是拼 "<dev>1" 后缀：dm 设备的分区节点由内核/udev 命名，
// 可能叫 /dev/dm-N，也可能是 /dev/mapper/<vg>-<lv>1 之类的软链，拼名字不可靠。
func (m *Manager) firstPartitionDevice(ctx context.Context, dev string) (string, error) {
	nodes, err := m.lsblkTree(ctx)
	if err != nil {
		return "", err
	}
	n := layoutNode(nodes, dev)
	if n == nil {
		// 设备不在 lsblk 树里（映射尚未建立/已停用）：交给调用方按"没有分区"处理。
		return "", nil
	}
	for i := range n.Children {
		if c := &n.Children[i]; strings.EqualFold(c.Type, "part") {
			return nodePath(c), nil
		}
	}
	// 个别 lsblk 版本对分区的 TYPE 标注不一致：只有一个子设备时按它算。
	if len(n.Children) == 1 {
		return nodePath(&n.Children[0]), nil
	}
	return "", nil
}

// formatVolume 在数据设备（分区）上创建文件系统。
//
// 调用方必须已经确认设备上没有文件系统：这里不做任何"要不要覆盖"的判断。
func (m *Manager) formatVolume(ctx context.Context, dev, fileSystem, label string) error {
	l := strings.TrimSpace(label)
	switch fileSystem {
	case "ntfs":
		// -Q 快速格式化：建盘阶段只要元数据，不必全盘写零。
		args := []string{"-Q"}
		if l != "" {
			args = append(args, "-L", l)
		}
		if _, err := m.run(ctx, "mkfs.ntfs", append(args, dev)...); err != nil {
			return err
		}
	case "ext4":
		// -F 允许在残留签名的设备上创建（分区刚建出来时不该有，但幂等路径上可能有）。
		args := []string{"-F", "-q"}
		if l != "" {
			args = append(args, "-L", l)
		}
		if _, err := m.run(ctx, "mkfs.ext4", append(args, dev)...); err != nil {
			return err
		}
	default:
		return apperr.InvalidParam("file_system")
	}
	m.logger.Info("已格式化文件系统", "dev", dev, "file_system", fileSystem, "label", l)
	return nil
}

// mountVolume 把数据设备（分区）按文件系统挂到 mnt。
//
// NTFS 优先用内核原生的 ntfs3（Linux 5.15+，比 FUSE 的 ntfs-3g 快得多），内核没有这个
// 驱动、或它在个别卷上拒绝挂载时才回退 ntfs-3g。两者的挂载选项**不通用**：
// big_writes 只有 ntfs-3g 认，传给 ntfs3 会直接报 unknown option，所以必须分开传。
func (m *Manager) mountVolume(ctx context.Context, dev, fileSystem, mnt string) error {
	switch fileSystem {
	case "ext4":
		if _, err := m.run(ctx, "mount", "-t", "ext4", dev, mnt); err != nil {
			return err
		}
		return nil
	case "ntfs":
		if m.kernelHasNTFS3(ctx) {
			if _, err := m.run(ctx, "mount", "-t", "ntfs3", dev, mnt); err == nil {
				return nil
			} else {
				m.logger.Warn("ntfs3 挂载失败，回退 ntfs-3g", "dev", dev, "err", err.Error())
			}
		}
		if _, err := m.run(ctx, "mount", "-t", "ntfs-3g", "-o", mountOptions, dev, mnt); err != nil {
			return err
		}
		return nil
	default:
		return apperr.InvalidParam("file_system")
	}
}

// kernelHasNTFS3 报告内核是否提供 ntfs3 驱动。
//
// /proc/filesystems 只列出**已注册**的文件系统：驱动是模块且尚未加载时不会出现，
// 所以先尝试 modprobe（失败无所谓：可能是内建、也可能没装模块，随后的检查说了算）。
func (m *Manager) kernelHasNTFS3(ctx context.Context) bool {
	if hasExe("modprobe") {
		if _, err := m.runQuiet(ctx, "modprobe", "ntfs3"); err != nil {
			m.logger.Debug("modprobe ntfs3 未成功", "err", err.Error())
		}
	}
	data, err := os.ReadFile(procFileSystemsPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		// 行形如 "nodev\tsysfs" 或 "\text4"：取最后一个字段比整行比较稳。
		if f := strings.Fields(line); len(f) > 0 && f[len(f)-1] == "ntfs3" {
			return true
		}
	}
	return false
}

// hasExe 报告外部命令是否可用（只查文件，不执行）。
func hasExe(name string) bool {
	_, err := LookPath(name)
	return err == nil
}

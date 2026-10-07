//go:build linux

package linuxlvm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"vault/internal/apperr"
	"vault/internal/platform"
)

// 本文件实现 platform.StorageVolumeBackend：把"存储"落成 thin pool 里一个真实的 thin LV，
// 格式化（ext4/xfs）后挂载为目录（storages.path），承载 staging / 回收站 / 孤儿扫描，
// 其虚拟大小即该存储的容量配额。
//
// 关键约束：
//   - 挂载必须 **fail-closed**：mount 命令返回成功但 /proc/self/mountinfo 里查不到
//     （挂载点 + 设备都不一致）时一律报错，否则写入会静默落在宿主根文件系统上，
//     日后真正挂载再把数据"藏"起来；
//   - 存储卷用 ext4 / xfs，**不用 NTFS**（NTFS 只用于对外发布的虚拟磁盘）；
//   - 仅系统创建的卷（managed）才允许删除，登记已有卷绝不经本文件删除。

// mountOptionsStorage 是存储卷的挂载选项。
//
// 刻意不加 noatime 之外的激进选项：存储卷承载用户暂存数据，安全优先于性能。
const mountOptionsStorage = ""

// normalizeStorageFS 归一化并校验存储卷文件系统类型（仅 ext4 / xfs）。
func normalizeStorageFS(fs string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(fs)) {
	case "", "ext4":
		return "ext4", nil
	case "xfs":
		return "xfs", nil
	default:
		return "", apperr.InvalidParam("file_system")
	}
}

// CreateStorageVolume 幂等地建 thin LV → 格式化 → 挂载 → 校验挂载确实生效。
func (m *Manager) CreateStorageVolume(ctx context.Context, spec platform.StorageVolumeSpec) (*platform.StorageVolumeStatus, error) {
	ref := strings.TrimSpace(spec.Ref)
	vg, lv, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	// 存储卷落在哪个池由 spec.PoolRef 显式指定（用户在创建存储时选/建的池）；
	// 没指定（历史数据 / 目录模式）就按 ref 的 VG 反查。
	// 这里不再限制"必须是配置的 VG"：多存储池下每个 VG 都可以承载存储卷。
	poolName, err := m.storagePoolFor(ctx, vg, spec.PoolRef)
	if err != nil {
		return nil, err
	}
	mp := filepath.Clean(strings.TrimSpace(spec.MountPoint))
	if !filepath.IsAbs(mp) {
		return nil, apperr.InvalidParam("mount_point")
	}
	if spec.SizeBytes <= 0 {
		return nil, apperr.InvalidParam("size_bytes")
	}
	fs, err := normalizeStorageFS(spec.FileSystem)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return nil, apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithCause(err).WithArg("mount_point", mp)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	dev := lvRef(vg, lv)
	// 1) 建 thin LV（幂等：已存在则跳过，绝不覆盖）。
	if !m.exists(vg, lv) {
		pool, err := poolPath(vg, poolName)
		if err != nil {
			return nil, err
		}
		// 与建盘同一道水位闸门：thin 卷无法限制单卷物理增长，写爆会拖垮整个 pool。
		if err := m.checkWatermark(ctx, vg); err != nil {
			return nil, err
		}
		// -V 同样要求 512 的整数倍（见 alignSectorDown）：界面上两位小数的 GB 换算回字节必然带尾数。
		if _, err := m.run(ctx, "lvcreate", "--type", "thin", "-n", lv,
			"-V", strconv.FormatInt(alignSectorDown(spec.SizeBytes), 10)+"B", "-T", pool); err != nil {
			return nil, err
		}
		m.logger.Info("已创建存储 thin LV", "ref", ref, "size_bytes", spec.SizeBytes)
	}
	// 2) 激活以获得 /dev/mapper 节点（thin 默认带 skip-activation 标记，见 Activate 注释）。
	if _, err := m.run(ctx, "lvchange", "-ay", "-K", vg+"/"+lv); err != nil {
		return nil, err
	}
	// 3) 格式化（幂等：已有同类型 fs 跳过；已有其它 fs 则拒绝覆盖）。
	if err := m.ensureStorageFS(ctx, dev, fs, spec.Label); err != nil {
		return nil, err
	}
	// 4) 挂载 + fail-closed 校验。
	if err := m.mountVerified(ctx, dev, mp); err != nil {
		return nil, err
	}
	st, err := m.StorageVolumeStatus(ctx, ref, mp)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// ensureStorageFS 保证设备上是目标文件系统，且**绝不覆盖**已有其它文件系统。
func (m *Manager) ensureStorageFS(ctx context.Context, dev, fs, label string) error {
	existing, _ := m.blkidType(ctx, dev)
	switch {
	case strings.EqualFold(existing, fs):
		m.logger.Info("存储卷已有目标文件系统，跳过格式化", "dev", dev, "file_system", fs)
		return nil
	case existing != "":
		// 已有其它文件系统：可能承载用户数据，绝不覆盖。
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).WithArg("existing_fs", existing)
	}

	// 兜底清理残留签名（blkid 未识别但内核仍可能认到的旧签名），失败仅告警。
	if _, err := LookPath("wipefs"); err == nil {
		if _, err := m.run(ctx, "wipefs", "-a", dev); err != nil {
			m.logger.Warn("wipefs 清理残留签名失败（忽略）", "dev", dev, "err", err.Error())
		}
	}

	args := []string{"-F"} // ext4: 强制在已存在"看起来像 fs"的块设备上创建
	if fs == "xfs" {
		args = []string{"-f"}
	}
	if l := strings.TrimSpace(label); l != "" {
		args = append(args, "-L", l)
	}
	args = append(args, dev)
	if _, err := m.run(ctx, "mkfs."+fs, args...); err != nil {
		return err
	}
	m.logger.Info("已格式化存储卷", "dev", dev, "file_system", fs, "label", label)
	return nil
}

// mountVerified 把 dev 挂到 mp，并在挂载后核对 /proc/self/mountinfo。
//
// fail-closed：无法确认"挂载点已被该设备占据"时一律报错。
func (m *Manager) mountVerified(ctx context.Context, dev, mp string) error {
	entries, err := readMountInfo()
	if err != nil {
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithCause(err).WithArg("reason", "mountinfo_unreadable")
	}
	if e, ok := findMountIn(entries, mp); ok {
		if sameDevice(e.source, dev) {
			return nil // 已挂载且设备一致，幂等
		}
		// 挂载点被别的设备占着：绝不覆盖挂载。
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithArg("reason", "mount_point_occupied").WithArg("mount_point", mp)
	}

	args := []string{}
	if mountOptionsStorage != "" {
		args = append(args, "-o", mountOptionsStorage)
	}
	args = append(args, dev, mp)
	if _, err := m.run(ctx, "mount", args...); err != nil {
		return err
	}

	// 挂载后必须复核：命令返回成功不等于挂载真的生效。
	entries, err = readMountInfo()
	if err != nil {
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithCause(err).WithArg("reason", "mountinfo_unreadable")
	}
	e, ok := findMountIn(entries, mp)
	if !ok || !sameDevice(e.source, dev) {
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithArg("reason", "mount_not_effective").WithArg("mount_point", mp)
	}
	m.logger.Info("已挂载存储卷并校验生效", "dev", dev, "mount_point", mp)
	return nil
}

// MountStorageVolume 幂等地把存储卷挂载到 mountPoint。
func (m *Manager) MountStorageVolume(ctx context.Context, ref, mountPoint string) error {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}
	mp := filepath.Clean(strings.TrimSpace(mountPoint))
	if !filepath.IsAbs(mp) {
		return apperr.InvalidParam("mount_point")
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithCause(err).WithArg("mount_point", mp)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.exists(vg, lv) {
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).WithArg("reason", "volume_missing")
	}
	if _, err := m.run(ctx, "lvchange", "-ay", "-K", vg+"/"+lv); err != nil {
		return err
	}
	return m.mountVerified(ctx, lvRef(vg, lv), mp)
}

// UnmountStorageVolume 幂等地卸载存储卷（未挂载视为成功）。
func (m *Manager) UnmountStorageVolume(ctx context.Context, _ string, mountPoint string) error {
	mp := filepath.Clean(strings.TrimSpace(mountPoint))
	if !filepath.IsAbs(mp) {
		return apperr.InvalidParam("mount_point")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := readMountInfo()
	if err != nil {
		// 读不到挂载信息就无法确认"是否已卸载"，fail-closed 直接报错。
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithCause(err).WithArg("reason", "mountinfo_unreadable")
	}
	e, ok := findMountIn(entries, mp)
	if !ok {
		return nil
	}
	if _, err := m.run(ctx, "umount", mp); err != nil {
		return err
	}
	if entries, err = readMountInfo(); err == nil {
		if _, still := findMountIn(entries, mp); still {
			return apperr.New(CodeMountFailed, http.StatusInternalServerError).
				WithArg("reason", "umount_not_effective").WithArg("mount_point", mp)
		}
	}
	m.logger.Info("已卸载存储卷", "mount_point", mp, "device", e.source)
	return nil
}

// ResizeStorageVolume 扩容存储卷（**只扩不缩**），并在线扩展文件系统。
func (m *Manager) ResizeStorageVolume(ctx context.Context, ref string, sizeBytes int64) error {
	if sizeBytes <= 0 {
		return apperr.InvalidParam("size_bytes")
	}
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.exists(vg, lv) {
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).WithArg("reason", "volume_missing")
	}
	cur, err := m.lvSizeBytes(vg, lv)
	if err != nil {
		return err
	}
	// 目标容量先对齐扇区再比较：下面用的是增量（-L +delta），而 LVM 对增量同样要求
	// 512 的整数倍（见 alignSectorDown）。先对齐还有一层好处——"目标比当前只大不到一个扇区"
	// 会被判成没变化，直接跳过 lvextend，而不是发一条注定被拒的命令。
	sizeBytes = alignSectorDown(sizeBytes)
	if sizeBytes < cur {
		// 缩容会丢数据，明确拒绝（不是 no-op，避免调用方误以为已生效）。
		return apperr.InvalidParam("size_bytes")
	}
	if sizeBytes > cur {
		if err := m.checkWatermark(ctx, vg); err != nil {
			return err
		}
		// 用相对增量（-L +delta）：thin 卷对绝对大小有额外语义，增量最无歧义。
		delta := strconv.FormatInt(sizeBytes-cur, 10) + "B"
		if _, err := m.run(ctx, "lvextend", "-L", "+"+delta, vg+"/"+lv); err != nil {
			return err
		}
		m.logger.Info("已扩展存储卷", "ref", ref, "size_bytes", sizeBytes)
	}
	return m.growStorageFS(ctx, ref, vg, lv)
}

// growStorageFS 在线扩展存储卷的文件系统（幂等：容量已正确时是 no-op）。
func (m *Manager) growStorageFS(ctx context.Context, ref, vg, lv string) error {
	dev := lvRef(vg, lv)
	fs, _ := m.blkidType(ctx, dev)
	switch strings.ToLower(strings.TrimSpace(fs)) {
	case "ext4", "ext3", "ext2":
		// ext 系列可在线扩展。
		if _, err := m.run(ctx, "resize2fs", dev); err != nil {
			return err
		}
	case "xfs":
		// xfs **只能在线扩展**，必须指向已挂载的挂载点。
		mp, err := mountPointOf(dev)
		if err != nil || mp == "" {
			return apperr.New(CodeMountFailed, http.StatusInternalServerError).
				WithArg("reason", "xfs_requires_online_grow")
		}
		if _, err := m.run(ctx, "xfs_growfs", mp); err != nil {
			return err
		}
	case "":
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).WithArg("reason", "no_file_system")
	default:
		return apperr.New(CodeMountFailed, http.StatusInternalServerError).WithArg("existing_fs", fs)
	}
	m.logger.Info("已扩展存储卷文件系统", "ref", ref, "file_system", fs)
	return nil
}

// DeleteStorageVolume 卸载后删除存储卷。幂等（不存在视为成功）。
//
// 调用方必须先确认该卷是系统创建的；登记已有的卷绝不可走本方法。
func (m *Manager) DeleteStorageVolume(ctx context.Context, ref string) error {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.exists(vg, lv) {
		m.logger.Info("存储卷不存在，跳过删除（幂等）", "ref", ref)
		return nil
	}
	dev := lvRef(vg, lv)
	// 先卸载；未挂载则跳过（mountPointOf 读不到时告警但不阻断删除）。
	if mp, err := mountPointOf(dev); err != nil {
		m.logger.Warn("读取挂载信息失败，跳过卸载", "ref", ref, "err", err.Error())
	} else if mp != "" {
		if _, err := m.run(ctx, "umount", mp); err != nil {
			return err
		}
	}
	if _, err := m.run(ctx, "lvchange", "-an", vg+"/"+lv); err != nil {
		m.logger.Debug("删除前停用存储卷未成功（忽略）", "ref", ref, "err", err.Error())
	}
	if _, err := m.run(ctx, "lvremove", "-y", vg+"/"+lv); err != nil {
		return err
	}
	m.logger.Info("已删除存储卷", "ref", ref)
	return nil
}

// StorageVolumeStatus 查询存储卷现状（存在性 / 挂载状态 / 容量 / 健康）。
//
// 挂载状态以 /proc/self/mountinfo 的实挂为准（挂载点 + 设备双重比对），
// 未挂载时 Health = "not_mounted"：调用方据此把它排除出落盘位置（fail-closed）。
func (m *Manager) StorageVolumeStatus(_ context.Context, ref, mountPoint string) (*platform.StorageVolumeStatus, error) {
	vg, lv, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	mp := filepath.Clean(strings.TrimSpace(mountPoint))
	st := &platform.StorageVolumeStatus{Ref: ref, MountPoint: mp}

	if !m.exists(vg, lv) {
		st.Health = "missing"
		return st, nil
	}
	st.Exists = true
	if size, err := m.lvSizeBytes(vg, lv); err == nil {
		st.SizeBytes = size
	}

	entries, err := readMountInfo()
	if err != nil {
		st.Health = "mountinfo_unreadable"
		return st, nil
	}
	dev := lvRef(vg, lv)
	e, ok := findMountIn(entries, mp)
	if !ok || !sameDevice(e.source, dev) {
		st.Health = "not_mounted"
		return st, nil
	}
	st.Mounted = true
	st.FileSystem = e.fsType

	if size, err := statfsSize(mp); err == nil {
		st.UsedBytes = size.totalBytes - size.freeBytes
		// 容量口径以 LV 虚拟大小为准（thin 卷不自增长，写满虚拟大小即 ENOSPC）。
		if st.SizeBytes <= 0 {
			st.SizeBytes = size.totalBytes
		}
	}
	return st, nil
}

// ---- statfs 与 mountinfo 辅助 ----

// fsSize 是 statfs(2) 的容量结果（只取容量）。
//
// 文件系统类型刻意不在此判定：mountinfo 里已是可读字符串，
// 不必维护一份易错的 magic number 表。
type fsSize struct {
	totalBytes int64
	freeBytes  int64
}

// statfsSize 读取路径所在文件系统的总容量与**非特权可用**容量。
//
// 用 Bavail 而非 Bfree：Bfree 含仅 root 可用的保留块，用它会把保留空间算成可用。
func statfsSize(path string) (fsSize, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fsSize{}, err
	}
	bs := int64(st.Bsize)
	return fsSize{
		totalBytes: int64(st.Blocks) * bs,
		freeBytes:  int64(st.Bavail) * bs,
	}, nil
}

// mountInfoEntry 是 /proc/self/mountinfo 中我们关心的字段。
type mountInfoEntry struct {
	mountPoint string
	fsType     string
	source     string
}

// readMountInfo 解析 /proc/self/mountinfo。
//
// 行格式（proc(5)）：
//
//	36 35 98:0 /mnt /mnt rw,relatime shared:1 - ext4 /dev/sda1 rw
//	<id> <parent> <maj:min> <root> <mountpoint> <opts> [optional...] - <fstype> <source> <superopts>
//
// 因此：挂载点 = 分隔符前的第 5 段；fstype / source = 分隔符后的前两段。
func readMountInfo() ([]mountInfoEntry, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	out := make([]mountInfoEntry, 0, len(lines))
	for _, line := range lines {
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 2 {
			continue
		}
		out = append(out, mountInfoEntry{
			mountPoint: unescapeMountField(left[4]),
			fsType:     right[0],
			source:     unescapeMountField(right[1]),
		})
	}
	return out, nil
}

// findMountIn 在已解析的挂载表里按挂载点精确查找。
func findMountIn(entries []mountInfoEntry, mountPoint string) (mountInfoEntry, bool) {
	for _, e := range entries {
		if e.mountPoint == mountPoint {
			return e, true
		}
	}
	return mountInfoEntry{}, false
}

// mountOf 返回包含 path 的那个挂载项（最长前缀匹配）；找不到返回 false。
//
// 用于 SpaceUsageOf：path 通常是 storages.path，可能是存储卷的挂载点本身，
// 也可能是宿主文件系统上的普通目录，两种情况都要拿到"承载它的卷"。
func mountOf(path string) (mountInfoEntry, bool) {
	entries, err := readMountInfo()
	if err != nil {
		return mountInfoEntry{}, false
	}
	p := filepath.Clean(path)
	best := -1
	for i := range entries {
		mp := entries[i].mountPoint
		if !pathWithin(p, mp) {
			continue
		}
		if best < 0 || len(mp) > len(entries[best].mountPoint) {
			best = i
		}
	}
	if best < 0 {
		return mountInfoEntry{}, false
	}
	return entries[best], true
}

// pathWithin 判断 p 是否等于 mp 或位于 mp 之下（按路径边界比较，避免 /a-b 被误判在 /a 内）。
func pathWithin(p, mp string) bool {
	if p == mp {
		return true
	}
	if mp == "/" {
		return strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, strings.TrimSuffix(mp, "/")+"/")
}

// sameDevice 判断两个设备标识是否指向同一设备（解析符号链接后比较，如 /dev/mapper/x 与 /dev/dm-0）。
func sameDevice(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && ra == rb
}

// vgFreeBytesOf 返回指定 VG 的剩余容量（读不到时返回错误，调用方自行决定是否忽略）。
func (m *Manager) vgFreeBytesOf(ctx context.Context, vg string) (int64, error) {
	vg = strings.TrimSpace(vg)
	if vg == "" {
		return 0, fmt.Errorf("未配置卷组")
	}
	rows, err := m.vgsRows(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if strings.TrimSpace(rowStr(r, "vg_name")) == vg {
			free, ok := rowIntOK(r, "vg_free")
			if !ok {
				// 有这一行却读不出容量：当成"读不到"上报，别让 0 冒充真实空余。
				return 0, fmt.Errorf("读不出卷组 %s 的剩余容量", vg)
			}
			return free, nil
		}
	}
	return 0, fmt.Errorf("未找到卷组 %s", vg)
}

//go:build linux

package linuxlvm

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform"
)

// mountOptions 是 ntfs-3g 的挂载选项。big_writes 显著提升大块写入吞吐（建盘拷入场景）。
const mountOptions = "big_writes"

// EnsureFormatted 幂等地把盘准备好：分区表 + 单分区 + 分区上的文件系统。
//
// 服务端负责"把盘做成客户端能直接挂载的样子"（客户端只看结果，不再自己分区/格式化）：
//  1. 激活 LV（保证 /dev/mapper 设备节点存在）；
//  2. 布局：空盘写 GPT + 单分区；已有分区的盘复用第一个分区；升级前的整盘布局
//     （裸文件系统直接在整盘上、盘里已有数据的旧盘）按原样使用，**不做转换**，
//     见 ensureDiskLayout；
//  3. 格式化：数据设备上**还没有**文件系统时才格式化；已有任何文件系统一律不覆盖。
//
// 支持 ntfs（Windows 客户端）与 ext4（Linux 客户端，预留）：用哪种由建库时的
// "客户端操作系统"决定，并持久化在盘记录上（domain.Disk.FileSystem）。
func (m *Manager) EnsureFormatted(ctx context.Context, ref, fileSystem, label string) (*platform.Volume, error) {
	fs, err := diskFileSystem(fileSystem)
	if err != nil {
		return nil, err
	}
	vg, lv, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	// 先激活，保证 /dev/mapper 节点存在——未激活的 LV 没有设备节点，blkid/mkfs 都会失败。
	if err := m.Activate(ctx, ref, false); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 数据设备是"分区设备"（旧版整盘布局则是整盘本身）：GPT 只是给客户端看的壳，
	// 文件系统建在分区上，客户端按"磁盘 → 分区 → 卷"的模型就能直接挂载。
	dev, layout, err := m.ensureDiskLayout(ctx, lvRef(vg, lv), fs)
	if err != nil {
		return nil, err
	}

	existing, _ := m.blkidType(ctx, dev)
	switch {
	case strings.EqualFold(existing, fs):
		m.logger.Info("数据设备已有目标文件系统，跳过格式化",
			"ref", ref, "device", dev, "file_system", fs, "layout", layout)
	case existing != "":
		// 已有其它文件系统：绝不覆盖用户数据。
		return nil, apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithArg("existing_fs", existing)
	default:
		if err := m.formatVolume(ctx, dev, fs, label); err != nil {
			return nil, err
		}
	}

	size, err := m.lvSizeBytes(vg, lv)
	if err != nil {
		size = 0
	}
	// 刻意不在此 Deactivate：LV 需要保持激活供 iSCSI(LIO iblock) 发布使用。
	return &platform.Volume{Device: dev, FileSystem: strings.ToUpper(fs), SizeBytes: size}, nil
}

// MountAndCopy 一次性完成"激活 → 分区 → 格式化 → 递归拷入 sourceDir 内容 → 校验 → 卸载"。
//
// 校验口径与项目既有 CopyTree 一致：目标侧文件数/字节数不得少于源，否则 platform.copy_failed。
func (m *Manager) MountAndCopy(ctx context.Context, ref, sourceDir, fileSystem, label string) (int, int64, error) {
	src := filepath.Clean(sourceDir)
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		return 0, 0, apperr.InvalidParam("source_dir")
	}
	fs, err := diskFileSystem(fileSystem)
	if err != nil {
		return 0, 0, err
	}

	// EnsureFormatted 内部会激活 LV、建好分区，并返回**数据设备**（分区）路径。
	vol, err := m.EnsureFormatted(ctx, ref, fileSystem, label)
	if err != nil {
		return 0, 0, err
	}
	dev := vol.Device

	mnt, err := os.MkdirTemp("", "vault-disk-")
	if err != nil {
		return 0, 0, apperr.New(CodeMountFailed, http.StatusInternalServerError).WithCause(err)
	}
	// defer 保证失败路径也清理临时目录。
	defer func() { _ = os.Remove(mnt) }()

	// NTFS 优先内核 ntfs3、回退 ntfs-3g；ext4 走内核驱动（见 mountVolume）。
	if err := m.mountVolume(ctx, dev, fs, mnt); err != nil {
		return 0, 0, err
	}
	mounted := true
	// defer 保证失败路径也卸载（即使 ctx 已取消也要尽力卸载，避免留下脏挂载）。
	defer func() {
		if !mounted {
			return
		}
		if _, err := m.run(context.WithoutCancel(ctx), "umount", mnt); err != nil {
			m.logger.Warn("卸载临时挂载点失败", "ref", ref, "mountpoint", mnt, "err", err.Error())
		}
	}()

	srcFiles, srcBytes, err := countTree(src)
	if err != nil {
		return 0, 0, apperr.New(CodeCopyFailed, http.StatusInternalServerError).WithCause(err)
	}
	dstFiles, dstBytes, err := copyTree(ctx, src, mnt)
	if err != nil {
		return dstFiles, dstBytes, apperr.New(CodeCopyFailed, http.StatusInternalServerError).WithCause(err)
	}
	if _, err := m.run(ctx, "sync"); err != nil {
		m.logger.Warn("sync 失败", "ref", ref, "err", err.Error())
	}
	if _, err := m.run(ctx, "umount", mnt); err != nil {
		return dstFiles, dstBytes, apperr.New(CodeMountFailed, http.StatusInternalServerError).WithCause(err)
	}
	mounted = false

	// 尽力而为地修复 NTFS 日志（ntfsfix -d 清 dirty flag）；缺失或失败只告警，不影响建盘结果。
	// 只对 NTFS 有意义，ext4 上没有这个工具也不需要。
	if fs == "ntfs" {
		if _, err := LookPath("ntfsfix"); err == nil {
			if _, err := m.run(ctx, "ntfsfix", "-d", dev); err != nil {
				m.logger.Warn("ntfsfix 失败（忽略）", "ref", ref, "err", err.Error())
			}
		}
	}

	if dstFiles < srcFiles || dstBytes < srcBytes {
		m.logger.Error("卷内容复制校验失败",
			"src_dir", src, "src_files", srcFiles, "src_bytes", srcBytes,
			"dst_files", dstFiles, "dst_bytes", dstBytes)
		return dstFiles, dstBytes, apperr.New(CodeCopyFailed, http.StatusInternalServerError).
			WithArg("src_files", srcFiles).
			WithArg("dst_files", dstFiles).
			WithArg("src_bytes", srcBytes).
			WithArg("dst_bytes", dstBytes)
	}
	m.logger.Info("已完成卷格式化与内容拷贝", "ref", ref, "files", dstFiles, "bytes", dstBytes)
	return dstFiles, dstBytes, nil
}

// SpaceUsageOf 返回路径所在卷的标识、文件系统与可用/总空间。
//
// 入参 path 通常是 storages.path：Linux 下它可能是某个存储 thin LV 的**挂载点**，
// 也可能是宿主文件系统上的普通目录（登记已有目录的逃生入口）。
// 因此这里**以 statfs 为准**取实际容量与文件系统，而不是按配置的 VG 硬编码：
//   - Name 取承载该路径的设备（mountinfo 的 source），供 PathGuardSet 按卷去重——
//     若回退成 VG 名，多个存储会被去重塌成一条；
//   - TotalBytes 取 statfs 的总量（存储卷即其 LV 虚拟大小）；
//   - FreeBytes 取 **min(statfs 可用, 所属 thin pool 剩余)**：thin 卷不自增长，写满虚拟大小
//     即 ENOSPC，"虚拟大小里还剩多少"与"池里还剩多少"是两个独立的限制，取小者才不会让
//     PathGuardSet.Pick 选到一个池已见底的根。
//
// ⚠️ 曾经这里取的是**卷组剩余（vg_free）**，那是错的：池建好时就把卷组空间整块划走了，
// vg_free 只剩零头，而往 thin 卷里写数据消耗的是池的空间。真机反馈：16G 的存储卷配 16G 的池，
// vg_free 只剩 48M，界面因此显示"16G 的卷、可用 48M"，新建存储库一律被判
// storage.low_free_space（见 thinPoolFreeBytesOf）。
func (m *Manager) SpaceUsageOf(ctx context.Context, path string) (*domain.VolumeSpace, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return nil, apperr.InvalidParam("path")
	}
	size, err := statfsSize(p)
	if err != nil {
		return nil, apperr.New(CodeVHDFailed, http.StatusInternalServerError).
			WithCause(err).WithArg("path", p)
	}

	name, fsType := p, ""
	if e, ok := mountOf(p); ok {
		name, fsType = e.source, e.fsType
	}

	free := size.freeBytes
	// 该路径落在某个 thin 卷上时，还要看它所属**池**还剩多少（多存储池下各算各的）：
	// 池见底后这个卷再空也写不进去。猜不出池（厚卷、宿主目录）就只看文件系统本身。
	if poolFree, ok := m.thinPoolFreeBytesOf(ctx, name); ok && poolFree < free {
		free = poolFree
	}
	return &domain.VolumeSpace{
		Name:       name,
		FileSystem: fsType,
		FreeBytes:  free,
		TotalBytes: size.totalBytes,
	}, nil
}

// FileSystemOf 返回路径所在卷的文件系统名。
//
// Linux 侧卷就是承载该路径的文件系统：优先给 statfs/mountinfo 的**真实**类型；
// 路径暂时不可 stat（如存储根尚未挂载）时退化为按配置 VG 校验，保持既有"根可用性"语义。
func (m *Manager) FileSystemOf(ctx context.Context, path string) (string, error) {
	if e, ok := mountOf(path); ok && e.fsType != "" {
		return e.fsType, nil
	}
	vg := m.volumeNameOf(path)
	if vg == "" {
		return "", apperr.InvalidParam("path")
	}
	if !m.vgExists(ctx, vg) {
		return "", apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).WithArg("path", path)
	}
	return "LVM", nil
}

// volumeNameOf 把入参归一化为 VG 名：仅在入参确实是 "<vg>/..." 或
// "/dev/mapper/<vg>-..." 时取其中的 VG，否则回退到配置的 VG。
//
// 之所以要回退：调用方传进来的 storages.path 是本地普通目录（如 /var/lib/vault），
// 用 vgFromPath 解析会失败，而它的空间口径本就应该按承载磁盘的 VG 统计。
func (m *Manager) volumeNameOf(path string) string {
	if vg, err := vgFromPath(path); err == nil && m.vgExists(context.Background(), vg) {
		return vg
	}
	return strings.TrimSpace(m.vg)
}

// vgFromPath 从 "<vg>/<thin_pool>" 或 "/dev/mapper/<vg>-<lv>" 中提取 VG 名。
func vgFromPath(path string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return "", apperr.InvalidParam("path")
	}
	if rest, ok := strings.CutPrefix(p, mapperPrefix); ok {
		i := strings.IndexByte(rest, '-')
		if i <= 0 {
			return "", apperr.InvalidParam("path")
		}
		p = rest[:i]
	} else if i := strings.IndexAny(p, "/\\"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimSpace(p)
	if !lvNameRe.MatchString(p) {
		return "", apperr.InvalidParam("path")
	}
	return p, nil
}

// countTree 统计目录树下的常规文件数与总字节数。
func countTree(root string) (files int, bytes int64, err error) {
	err = filepath.Walk(root, func(_ string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fi.Mode().IsRegular() {
			files++
			bytes += fi.Size()
		}
		return nil
	})
	return files, bytes, err
}

// copyTree 递归把 src 目录内容复制进 dst，返回复制的常规文件数与字节数。
//
// 自己实现而不依赖 cp：需要精确统计文件数/字节数用于校验（与既有 CopyTree 口径一致），
// 且避免 cp 在 ntfs-3g 上 sparse/权限语义不可控带来的差异。
func copyTree(ctx context.Context, src, dst string) (int, int64, error) {
	var files int
	var bytes int64
	err := filepath.Walk(src, func(path string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil // 跳过符号链接/设备等特殊文件
		}
		n, err := copyFile(path, target, fi.Mode())
		if err != nil {
			return err
		}
		files++
		bytes += n
		return nil
	})
	return files, bytes, err
}

// copyFile 复制单个常规文件并返回写入的字节数。
func copyFile(src, dst string, mode os.FileMode) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close() //nolint:errcheck
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if err != nil {
		_ = out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return n, nil
}

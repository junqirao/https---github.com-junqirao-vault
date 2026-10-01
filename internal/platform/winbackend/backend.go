//go:build windows

// Package winbackend 把 Windows 侧的三块既有实现（winvhd / volume / iscsitarget）
// 适配成 platform 包定义的 DiskBackend / VolumeBackend / IscsiBackend / StorageAdmin 接口。
//
// 设计要点：
//   - 本包**只做适配与语义翻译**，不重复实现任何底层能力，所有调用直接转发到既有 Manager；
//   - 引用（ref）在 Windows 上就是 VHDX 文件的绝对路径，app 层原样传递不解析；
//   - 平台不具备的能力（LVM 池、会话枚举）统一返回 platform.ErrUnsupported，
//     由上层据此隐藏入口或走降级路径，而不是把错误暴露给用户。
package winbackend

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform"
	"vault/internal/platform/iscsitarget"
	"vault/internal/platform/volume"
	"vault/internal/platform/winps"
	"vault/internal/platform/winvhd"
)

// Backend 是 Windows 平台后端的聚合入口，一次装配三个既有 Manager。
type Backend struct {
	// VHD 管理本机 VHDX 文件（创建/挂载/复制/压缩/重置磁盘标识）。
	VHD *winvhd.Manager
	// Vol 管理磁盘-卷层面操作（初始化/分区/格式化/空间查询）。
	Vol *volume.Manager
	// Iscsi 管理 Windows IscsiTarget 角色（虚拟盘登记/目标/授权/CHAP/映射）。
	Iscsi *iscsitarget.Manager
	log   *slog.Logger
}

// 编译期断言：Backend 必须同时满足四个后端接口（外加可选的能力报告接口）。
var (
	_ platform.DiskBackend          = (*Backend)(nil)
	_ platform.VolumeBackend        = (*Backend)(nil)
	_ platform.IscsiBackend         = (*Backend)(nil)
	_ platform.StorageAdmin         = (*Backend)(nil)
	_ platform.StorageVolumeBackend = (*Backend)(nil)
	_ platform.BackendCapabilities  = (*Backend)(nil)
)

// New 构造 Backend。logger 为 nil 时回退到 slog.Default()。
func New(vhd *winvhd.Manager, vol *volume.Manager, iscsi *iscsitarget.Manager, logger *slog.Logger) *Backend {
	if logger == nil {
		logger = slog.Default()
	}
	return &Backend{VHD: vhd, Vol: vol, Iscsi: iscsi, log: logger}
}

// ---- DiskBackend ----

// Kind 返回平台种类。
func (b *Backend) Kind() platform.Kind { return platform.KindWindows }

// Available 探测 iSCSI 目标服务可用性（IscsiBackend 契约）。
//
// 磁盘后端（platform.DiskBackend）刻意不含 Available：Windows 上 VirtDisk 为系统自带、
// 恒可用，磁盘可用性由磁盘操作自身的成败体现；唯一"会变化"的可用性是 iSCSI 角色
// （依赖 WinTarget 服务 + IscsiTarget 模块），由本方法承载（app.Health 的 win_target 依赖它）。
func (b *Backend) Available(ctx context.Context) error { return b.Iscsi.Available(ctx) }

// DiskRef 把"存储根 + 平台中性的相对布局标识"翻译为 VHDX 绝对路径。
//
// 与 domain.PathGuard.Resolve 同等做越界校验：拒绝绝对路径/UNC，Clean 后必须落在 root 之内
// （Windows 大小写不敏感，且按父目录边界比较，避免 `D:\Vault-evil` 被误判为在 `D:\Vault` 内）。
// 之所以在这里校验：磁盘的物理位置是平台概念，上层只负责给出稳定的布局标识。
func (b *Backend) DiskRef(storageRoot, rel string) (string, error) {
	if strings.TrimSpace(storageRoot) == "" {
		return "", apperr.InvalidParam("path")
	}
	root := filepath.Clean(storageRoot)
	if strings.TrimSpace(rel) == "" {
		return root, nil
	}
	if isAbsoluteLike(rel) {
		return "", apperr.InvalidParam("path")
	}
	target := filepath.Clean(filepath.Join(root, rel))
	if !domain.UnderRoot(root, target) {
		return "", apperr.InvalidParam("path")
	}
	return target, nil
}

// Exists 判断 VHDX 文件是否存在。
func (b *Backend) Exists(ref string) bool { return b.VHD.Exists(ref) }

// PhysicalSize 返回 VHDX 文件的物理占用（宿主卷上的实际大小）。
func (b *Backend) PhysicalSize(ref string) (int64, error) { return b.VHD.FileSize(ref) }

// Fingerprint 计算母盘内容指纹（尺寸 + mtime + 头部 SHA-256）。
func (b *Backend) Fingerprint(ref string) (string, error) { return b.VHD.Fingerprint(ref) }

// Create 创建动态扩展 VHDX。sizeBytes 为负时视为参数非法。
func (b *Backend) Create(ctx context.Context, ref string, sizeBytes int64) error {
	if sizeBytes < 0 {
		return apperr.InvalidParam("size_bytes")
	}
	return b.VHD.CreateDynamic(ctx, ref, uint64(sizeBytes))
}

// CreateDiff 创建差异盘（childRef 引用 parentRef，容量继承父盘）。
func (b *Backend) CreateDiff(ctx context.Context, childRef, parentRef string) error {
	return b.VHD.CreateDifferencing(ctx, childRef, parentRef)
}

// Clone 全量复制 VHDX。
//
// 不再单独调用 ResetDiskIdentifier：winvhd.Copy 内部已在复制末尾重置磁盘标识，
// 这里显式再调一次只会重复执行脚本（既有 app 层因历史原因做过，属冗余）。
func (b *Backend) Clone(ctx context.Context, srcRef, dstRef string) error {
	return b.VHD.Copy(ctx, srcRef, dstRef)
}

// Delete 删除 VHDX 文件（不存在视为成功，保证幂等）。
func (b *Backend) Delete(ctx context.Context, ref string) error { return b.VHD.Delete(ctx, ref) }

// Optimize 回收 VHDX 未使用的物理空间（Retrim / Compact）。
func (b *Backend) Optimize(ctx context.Context, ref string) error { return b.VHD.Optimize(ctx, ref) }

// ResetDiskIdentifier 重置 VHDX 磁盘标识。
//
// 无 Hyper-V 模块时 winvhd 会返回 platform.reset_disk_id_unavailable，
// 由上层决定降级策略，本层不吞该错误。
func (b *Backend) ResetDiskIdentifier(ctx context.Context, ref string) error {
	return b.VHD.ResetDiskIdentifier(ctx, ref)
}

// Activate 在服务端本地挂载 VHDX，供后续格式化与拷入使用。
//
// 这里丢弃 winvhd.Attach 返回的句柄是安全的：句柄已由 winvhd 内部按（规范化）路径登记，
// Deactivate / Detach 能凭同一路径找到并关闭它；接口刻意不把句柄向上暴露，
// 以免调用方持有句柄造成生命周期纠缠。
func (b *Backend) Activate(ctx context.Context, ref string, readOnly bool) error {
	_, err := b.VHD.Attach(ctx, ref, readOnly)
	return err
}

// Deactivate 取消本地挂载（幂等）。
func (b *Backend) Deactivate(ctx context.Context, ref string) error { return b.VHD.Detach(ctx, ref) }

// ---- VolumeBackend ----

// EnsureFormatted 幂等地初始化 + 分区 + 格式化，并把 VolumeInfo 映射为平台中性结构。
//
// Device 取盘符（无盘符时为空串），与 platform.Volume.Device 的约定一致。
func (b *Backend) EnsureFormatted(ctx context.Context, ref, fileSystem, label string) (*platform.Volume, error) {
	info, err := b.Vol.EnsureFormatted(ctx, ref, fileSystem, label)
	if err != nil {
		return nil, err
	}
	return &platform.Volume{
		Device:     info.DriveLetter,
		FileSystem: info.FileSystem,
		SizeBytes:  info.SizeBytes,
	}, nil
}

// MountAndCopy 严格复刻 app 层建盘流水线的挂载段：
// 挂载 → 刷新主机存储缓存 → 格式化 → 递归拷入 → 卸载。
//
// 卸载放在 defer 中并用 context.WithoutCancel 执行：即使中途失败或 ctx 取消，
// 也必须把盘分离，避免留下"已挂载但无句柄管理"的脏状态（与 buildVHDXFromSource 一致）。
func (b *Backend) MountAndCopy(ctx context.Context, ref, sourceDir, fileSystem, label string) (int, int64, error) {
	if _, err := b.VHD.Attach(ctx, ref, false); err != nil {
		return 0, 0, err
	}
	detached := false
	defer func() {
		if detached {
			return
		}
		if dErr := b.VHD.Detach(context.WithoutCancel(ctx), ref); dErr != nil {
			b.log.Warn("卸载 VHDX 失败", "ref", ref, "error", dErr)
		}
	}()

	// 挂载后立即 Get-Disk 可能查不到磁盘，需刷新缓存；失败仅告警，不阻断建盘。
	if err := b.Vol.UpdateHostStorageCache(ctx); err != nil {
		b.log.Warn("刷新主机存储缓存失败", "error", err)
	}

	info, err := b.Vol.EnsureFormatted(ctx, ref, fileSystem, label)
	if err != nil {
		return 0, 0, err
	}
	if info.DriveLetter == "" {
		// 没有盘符就无法把内容拷进去，无法继续（与既有实现同一处理）。
		return 0, 0, apperr.New("disk.format_failed", http.StatusInternalServerError).
			WithArg("reason", "no_drive_letter")
	}

	files, bytes, err := b.VHD.CopyTree(ctx, sourceDir, info.DriveLetter+`:\\`)
	if err != nil {
		return 0, 0, err
	}
	if err := b.VHD.Detach(ctx, ref); err != nil {
		return 0, 0, err
	}
	detached = true
	return files, bytes, nil
}

// SpaceUsageOf 返回路径所在卷的标识、文件系统与可用/总空间。
func (b *Backend) SpaceUsageOf(ctx context.Context, path string) (*domain.VolumeSpace, error) {
	return b.Vol.SpaceUsageOf(ctx, path)
}

// FileSystemOf 返回路径所在卷的文件系统名。
func (b *Backend) FileSystemOf(ctx context.Context, path string) (string, error) {
	return b.Vol.FileSystemOf(ctx, path)
}

// ---- IscsiBackend ----
//
// Kind 与 Available 见上方（Kind 与 DiskBackend 共用；Available 是本接口独有）。

// EnsureTarget 幂等地把目标收敛到 spec 描述的期望状态。
//
// ⚠️ 顺序（重要，Windows 侧实测语义）：
//
//	① Import-IscsiVirtualDisk 登记虚拟盘（VHDX 必须未被本地挂载）；
//	② New/Set-IscsiServerTarget 下发目标本体（授权列表 + CHAP），**暂不改启用状态**；
//	③ Add-IscsiVirtualDiskTargetMapping 建立「虚拟盘 ↔ 目标」映射；
//	④ 最后才按 spec.Enabled 启用/停用。
//
// 之所以把"启用"挪到映射之后：一个尚未映射任何虚拟盘的目标被启用时，
// Windows 会直接以"无法创建 iSCSI 目标。"失败（对应日志 reason=set_target_failed
// step=set_enabled）。这也是 Microsoft 官方示例里创建目标后不单独调用 -Enable 的原因
// （见 docs/windows-iscsi-vhdx-api.md 5.1/5.2）。
//
// ⚠️ spec.ReadOnly 在 Windows 侧被忽略：IscsiTarget 模块没有 per-mapping 的只读开关，
// 只读一直是由客户端挂载方式承担的，见 platform.TargetSpec 注释。
func (b *Backend) EnsureTarget(ctx context.Context, spec platform.TargetSpec) error {
	if spec.BackingRef != "" {
		if err := b.Iscsi.ImportVirtualDisk(ctx, spec.BackingRef, "vault:"+spec.Name); err != nil {
			return err
		}
	}

	// ids 取 spec.Initiators 的副本，nil 归一为空切片（表示全量替换为"无授权"）。
	ids := make([]string, len(spec.Initiators))
	copy(ids, spec.Initiators)

	// 注意：SetTarget 内部仅在 ChapUser 非空时才下发 CHAP 参数，
	// 因此空的 CHAP 凭据原样放入结构体即等价于"不传"，与既有行为一致；
	// CHAP 密钥只在此结构体内传递，绝不写入日志。
	opt := iscsitarget.SetTargetOptions{
		InitiatorIDs:      &ids,
		ChapUser:          spec.ChapUser,
		ChapSecret:        spec.ChapSecret,
		EnableReverseChap: spec.EnableReverseChap,
		ReverseChapUser:   spec.ReverseChapUser,
		ReverseChapSecret: spec.ReverseChapSecret,
	}

	if spec.BackingRef == "" {
		// 无盘可映射（例如"仅停用目标"的最小期望状态）：没有映射约束，直接下发启用状态。
		enabled := spec.Enabled
		opt.Enabled = &enabled
		return b.Iscsi.SetTarget(ctx, spec.Name, opt)
	}

	// ①②③ 先建/更新目标（不改启用状态）→ 建立映射。
	//
	// 任一步失败都要让"已登记"记忆失效（ForgetImported）：否则下次会跳过真正的登记，
	// 把"其实没登记/没映射"这类真因掩盖成更难查的隐性故障。
	if err := b.Iscsi.SetTarget(ctx, spec.Name, opt); err != nil {
		b.Iscsi.ForgetImported(spec.BackingRef)
		return err
	}
	if err := b.Iscsi.AddMapping(ctx, spec.Name, spec.BackingRef); err != nil {
		b.Iscsi.ForgetImported(spec.BackingRef)
		return err
	}
	// ④ 映射就绪后再收敛启用状态（单独一次调用，不带任何其它字段）。
	enabled := spec.Enabled
	if err := b.Iscsi.SetTarget(ctx, spec.Name, iscsitarget.SetTargetOptions{Enabled: &enabled}); err != nil {
		b.Iscsi.ForgetImported(spec.BackingRef)
		return err
	}
	return nil
}

// RemoveTarget 删除目标（含其名下全部映射与授权）。幂等。
func (b *Backend) RemoveTarget(ctx context.Context, name string) error {
	return b.Iscsi.RemoveTarget(ctx, name)
}

// GetTarget 查询单个目标；不存在时返回 iscsi.target_not_found。
func (b *Backend) GetTarget(ctx context.Context, name string) (*platform.TargetInfo, error) {
	info, err := b.Iscsi.GetTarget(ctx, name)
	if err != nil {
		return nil, err
	}
	return toTargetInfo(info), nil
}

// ListTargets 列出全部目标。
func (b *Backend) ListTargets(ctx context.Context) ([]platform.TargetInfo, error) {
	infos, err := b.Iscsi.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]platform.TargetInfo, 0, len(infos))
	for i := range infos {
		out = append(out, *toTargetInfo(&infos[i]))
	}
	return out, nil
}

// ListSessions Windows IscsiTarget 模块没有会话枚举 API，返回 ErrUnsupported。
func (b *Backend) ListSessions(_ context.Context, _ string) ([]platform.Session, error) {
	return nil, platform.ErrUnsupported
}

// ForceLogout Windows 侧没有精确到会话的服务端登出能力，返回 ErrUnsupported；
// 需要的"踢下线"降级动作（停用目标）由上层编排，不在本适配层伪造。
func (b *Backend) ForceLogout(_ context.Context, _, _ string) error {
	return platform.ErrUnsupported
}

// EnsureVirtualDisk 幂等地把已有虚拟磁盘纳入 iSCSI 虚拟盘登记。
func (b *Backend) EnsureVirtualDisk(ctx context.Context, ref, description string) error {
	return b.Iscsi.ImportVirtualDisk(ctx, ref, description)
}

// RemoveVirtualDisk 移除虚拟盘登记（不删除底层 VHDX 文件）。幂等。
func (b *Backend) RemoveVirtualDisk(ctx context.Context, ref string) error {
	return b.Iscsi.RemoveVirtualDisk(ctx, ref)
}

// AttachLun 建立「虚拟磁盘 ↔ 目标」映射。幂等。
//
// ⚠️ readOnly 被忽略：Windows 的映射没有只读属性，只读由客户端挂载方式承担。
func (b *Backend) AttachLun(ctx context.Context, targetName, ref string, _ bool) error {
	return b.Iscsi.AddMapping(ctx, targetName, ref)
}

// DetachLun 解除映射。幂等（未映射视为成功）。
func (b *Backend) DetachLun(ctx context.Context, targetName, ref string) error {
	return b.Iscsi.RemoveMapping(ctx, targetName, ref)
}

// ---- StorageAdmin ----

// ListBlockDevices Windows 上 VHDX 直接落在 NTFS 卷上，没有"选块设备做 LVM 物理卷"的概念。
func (b *Backend) ListBlockDevices(_ context.Context) ([]platform.BlockDevice, error) {
	return nil, platform.ErrUnsupported
}

// Status Windows 侧没有卷组/thin pool/dm-cache，故无池状态可报告。
func (b *Backend) Status(_ context.Context) (*platform.PoolStatus, error) {
	return nil, platform.ErrUnsupported
}

// InitializePool Windows 侧无需初始化存储池：目标是 NTFS 卷上的目录。
func (b *Backend) InitializePool(_ context.Context, _ platform.PoolSpec) error {
	return platform.ErrUnsupported
}

// ---- StorageVolumeBackend ----

// Windows 上的"存储"就是 NTFS 卷上的一个目录，没有独立的底层卷可创建/挂载，
// 因此本组方法整体返回 ErrUnsupported，由 app 层走"目录模式"（与改动前行为一致）。

// CreateStorageVolume 不支持：Windows 无存储卷概念。
func (b *Backend) CreateStorageVolume(_ context.Context, _ platform.StorageVolumeSpec) (*platform.StorageVolumeStatus, error) {
	return nil, platform.ErrUnsupported
}

// MountStorageVolume 不支持：目录无需挂载。
func (b *Backend) MountStorageVolume(_ context.Context, _, _ string) error {
	return platform.ErrUnsupported
}

// UnmountStorageVolume 不支持：目录无需挂载。
func (b *Backend) UnmountStorageVolume(_ context.Context, _, _ string) error {
	return platform.ErrUnsupported
}

// ResizeStorageVolume 不支持：目录容量随宿主卷，无可独立调整的卷。
func (b *Backend) ResizeStorageVolume(_ context.Context, _ string, _ int64) error {
	return platform.ErrUnsupported
}

// DeleteStorageVolume 不支持：目录由管理员自行管理，系统绝不代为删除。
func (b *Backend) DeleteStorageVolume(_ context.Context, _ string) error {
	return platform.ErrUnsupported
}

// StorageVolumeStatus 不支持：请用 SpaceUsageOf 查询目录所在卷空间。
func (b *Backend) StorageVolumeStatus(_ context.Context, _, _ string) (*platform.StorageVolumeStatus, error) {
	return nil, platform.ErrUnsupported
}

// ---- BackendCapabilities（可选接口）----

// Capabilities 报告 Windows 侧的平台特有探测结果。
//
// 这里只报告 Hyper-V 模块可用性：它决定 ResetDiskIdentifier / Optimize-VHD 走哪条路径，
// 管理端据此提示"安装 Hyper-V 管理工具"，因此值得单独暴露。
func (b *Backend) Capabilities(_ context.Context) map[string]bool {
	return map[string]bool{"hyperv": b.VHD.HyperVAvailable()}
}

// ProbeHyperV 探测目标环境是否具备 Hyper-V 模块（Set-VHD / Optimize-VHD）。
//
// 之所以放在本包：这是 Windows 平台专有的探测，且其结果只用于构造 winvhd.Manager
// （见 winvhd.Options.HyperVModuleAvailable），不应出现在平台中性的 app 层。
//
// 探测失败（PowerShell 不可用等）一律按"不可用"处理并记录告警：
// 该探测只影响空间回收与磁盘标识重置的可用性，绝不能因探测本身失败而阻断启动。
func ProbeHyperV(ctx context.Context, ps *winps.Runner) bool {
	if ps == nil {
		return false
	}
	var out struct {
		Available bool `json:"available"`
	}
	if err := ps.RunJSON(ctx, hyperVProbeScript, &out); err != nil {
		slog.Default().Warn("探测 Hyper-V 模块失败，按不可用处理", "error", err)
		return false
	}
	return out.Available
}

// hyperVProbeScript 以命令是否存在判定 Hyper-V 模块可用性。
//
// 用 Get-Command 而非 Get-Module：前者不加载模块即可判定命令可发现性，更快且无副作用；
// 同时也覆盖了"Hyper-V 模块被显式导入"的情况。
const hyperVProbeScript = `$cmd = Get-Command -Name Set-VHD -ErrorAction SilentlyContinue
[pscustomobject]@{ ok = $true; available = [bool]$cmd } | ConvertTo-Json -Compress`

// ---- 内部辅助 ----

// toTargetInfo 把 iscsitarget 的 TargetInfo 映射为平台中性结构。
//
// 两个同名类型（iscsitarget.TargetInfo / platform.TargetInfo）的存在，
// 正是本适配层需要承担"翻译"职责的原因。
func toTargetInfo(info *iscsitarget.TargetInfo) *platform.TargetInfo {
	return &platform.TargetInfo{
		Name:       info.Name,
		Enabled:    info.Enabled,
		Initiators: info.InitiatorIDs,
		Devices:    info.MappedDevices,
	}
}

// isAbsoluteLike 识别绝对路径与 UNC，避免相对布局标识绕过 Join 语义逃出白名单根。
func isAbsoluteLike(p string) bool {
	if filepath.IsAbs(p) {
		return true
	}
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

// Package platform 定义"虚拟磁盘 / 卷 / iSCSI 目标"三块能力的**跨平台契约**。
//
// 分层约定（见 docs/implementation.md 6.2）：
//   - 本包只允许出现平台中性的类型与接口，**不得** import 任何 OS 专属包；
//   - 具体实现放在子包中：Windows = internal/platform/winbackend，
//     Linux = internal/platform/linuxlvm + internal/platform/liotarget；
//   - app 层只依赖本包接口，从而"换平台只需替换实现"。
//
// 引用（Ref）语义：所有后端都用**不透明字符串**标识一个虚拟磁盘——
// Windows 上是 VHDX 文件的绝对路径，Linux 上是 /dev/mapper/<vg>-<lv>。
// app 层把 domain.Disk.VHDXPath 原样当作该引用使用，不解析其内容。
package platform

import (
	"context"
	"errors"

	"vault/internal/domain"
)

// Kind 是平台种类。
type Kind string

const (
	// KindWindows Windows（VHDX + IscsiTarget 模块）。
	KindWindows Kind = "windows"
	// KindLinux Linux（LVM thin + LIO）。
	KindLinux Kind = "linux"
)

// ErrUnsupported 表示当前平台后端不具备该能力。
//
// 调用方（app / api）据此隐藏入口或走降级路径，而不是把错误暴露给用户。
var ErrUnsupported = errors.New("platform: unsupported")

// IsUnsupported 判断错误链上是否存在 ErrUnsupported。
func IsUnsupported(err error) bool { return errors.Is(err, ErrUnsupported) }

// Volume 是一次格式化/挂载操作返回的卷信息。
type Volume struct {
	// Device 卷设备标识：Windows 为盘符（如 "E"，无盘符时为空串）；Linux 为 /dev/mapper/<vg>-<lv>。
	Device string
	// FileSystem 卷文件系统名。
	FileSystem string
	// SizeBytes 卷容量（字节）。
	SizeBytes int64
}

// DiskBackend 是虚拟磁盘后端。
//
// 抽象映射：
//
//	Windows：VHDX 文件（动态/固定/差异盘）
//	Linux  ：LVM thin LV（Create=thin LV，CreateDiff=thin snapshot）
type DiskBackend interface {
	// Kind 返回平台种类。
	Kind() Kind

	// 注意：本接口**刻意不提供** Available 探测。
	//
	// 原因有二：
	//  1. 磁盘后端与 iSCSI 后端常由同一类型实现（如 winbackend.Backend），
	//     而 Go 类型无法给同名方法两套语义；iSCSI 的可用性才是会变化的（依赖角色/服务），
	//     已由 IscsiBackend.Available 承载；
	//  2. 虚拟磁盘可用性由磁盘操作自身的成败体现（工具缺失时 DiskRef/Create 会直接报错），
	//     静默返回 nil 反而会掩盖问题。存储池是否就绪由 StorageAdmin.Status 报告。

	// DiskRef 依据"存储根 + 平台中性的相对布局标识"生成虚拟磁盘引用。
	//
	// storageRoot 取自 storages.path：
	//   - Windows 是文件系统根目录（如 `D:\Vault`），rel 形如 `disks/parents/<id>/base.vhdx`，
	//     结果为 `D:\Vault\disks\parents\<id>\base.vhdx`（必须校验仍在 root 之内）；
	//   - Linux 是**真实本地目录**（暂存/回收站所在，如 `/var/lib/vault`），
	//     磁盘本身不落在该目录里，而是 LVM thin LV：后端按**自身配置的 VG** + rel
	//     推导出 LV 名（如 `disks/parents/<id>/base.vhdx` → `/dev/mapper/<vg>-disks_parents_<id>_base`）。
	//
	// 之所以把该翻译放进后端：磁盘的"位置"本身就是平台概念，
	// 上层的 repo/upload/disk 逻辑只负责给出稳定、可复用的布局标识。
	DiskRef(storageRoot, rel string) (string, error)

	// Exists 判断引用对应的虚拟磁盘是否存在（对 Windows 即文件是否存在）。
	Exists(ref string) bool

	// PhysicalSize 返回实际物理占用（字节）。
	//
	// Windows：文件大小；Linux：thin LV 的**独占**物理占用（必须用 thin_ls 语义，
	// 不能把共享块的占用重复计入）。
	PhysicalSize(ref string) (int64, error)

	// Fingerprint 计算内容指纹，用于检测"绕过本系统直接改写"的情况。
	Fingerprint(ref string) (string, error)

	// Create 创建一块逻辑容量为 sizeBytes 的虚拟磁盘。
	Create(ctx context.Context, ref string, sizeBytes int64) error

	// CreateDiff 创建差异盘（childRef 引用 parentRef，容量继承父盘）。
	CreateDiff(ctx context.Context, childRef, parentRef string) error

	// Clone 完整复制一块虚拟磁盘到 dstRef（目标已存在时返回错误），
	// 并重置磁盘标识，保证克隆结果可作为独立盘被识别。
	Clone(ctx context.Context, srcRef, dstRef string) error

	// Delete 删除虚拟磁盘。幂等（不存在视为成功）。
	Delete(ctx context.Context, ref string) error

	// Optimize 回收未使用的物理空间（Windows=compact；Linux=trim/discard）。
	Optimize(ctx context.Context, ref string) error

	// ResetDiskIdentifier 重置磁盘标识（克隆盘去重必需）。
	// 平台不具备该能力时返回 ErrUnsupported，由上层决定降级策略。
	ResetDiskIdentifier(ctx context.Context, ref string) error

	// Activate 在**服务端本地**激活/暴露该虚拟磁盘，供后续格式化与拷入使用。
	Activate(ctx context.Context, ref string, readOnly bool) error

	// Deactivate 取消本地激活（与 Activate 配对）。幂等。
	Deactivate(ctx context.Context, ref string) error
}

// VolumeBackend 是卷（文件系统）后端。
type VolumeBackend interface {
	// EnsureFormatted 幂等地初始化 + 分区 + 格式化，返回卷信息。
	//
	// 幂等保证：已有文件系统且与 fileSystem 一致时跳过格式化；
	// 已有其它文件系统时报错（避免覆盖已有数据）。
	EnsureFormatted(ctx context.Context, ref, fileSystem, label string) (*Volume, error)

	// MountAndCopy 一次性完成"激活 → 格式化 → 递归拷入 sourceDir 内容 → 卸载"，
	// 返回拷入的文件数与总字节数。
	//
	// 该方法是"从源目录建盘"的完整流水线，调用方不需要关心中间的挂载细节。
	MountAndCopy(ctx context.Context, ref, sourceDir, fileSystem, label string) (files int, bytes int64, err error)

	// SpaceUsageOf 返回路径所在卷的标识、文件系统与可用/总空间。
	SpaceUsageOf(ctx context.Context, path string) (*domain.VolumeSpace, error)

	// FileSystemOf 返回路径所在卷的文件系统名。
	FileSystemOf(ctx context.Context, path string) (string, error)
}

// StorageVolumeSpec 描述一次"存储卷创建"的期望（幂等）。
type StorageVolumeSpec struct {
	// Ref 卷设备引用（Linux 为 /dev/mapper/<vg>-<lv>），**必填**，
	// 由上层用 DiskRef("", "storages/"+storageID) 生成（复用既有 LV 命名折叠规则）。
	Ref string
	// SizeBytes 逻辑容量（字节）。thin 卷不自增长，用满即 ENOSPC。
	SizeBytes int64
	// FileSystem 文件系统类型（存储卷用 ext4 / xfs；**不用 NTFS**，NTFS 仅用于对外发布的虚拟磁盘）。
	FileSystem string
	// MountPoint 挂载目录（即 storages.path）。
	MountPoint string
	// Label 文件系统标签（可空）。
	Label string
}

// StorageVolumeStatus 是存储卷的现状快照。
type StorageVolumeStatus struct {
	// Exists 底层卷是否存在。
	Exists bool
	// Mounted 是否已挂载在 MountPoint 上（以 /proc/self/mountinfo 的实挂为准）。
	Mounted bool
	// Ref 卷设备引用。
	Ref string
	// MountPoint 挂载点。
	MountPoint string
	// FileSystem 实际文件系统类型（取自 statfs，而非配置或硬编码）。
	FileSystem string
	// SizeBytes 卷逻辑容量（字节）。
	SizeBytes int64
	// UsedBytes 已用空间（字节）。
	UsedBytes int64
	// Health 健康状态（空 = 正常；否则给出降级原因，供前端与放置决策使用）。
	Health string
}

// StorageVolumeBackend 是"存储底层卷"的平台能力（仅 Linux 有意义）。
//
// 与 StorageAdmin 的区别：StorageAdmin 只管**共享的存储池**（VG / thin pool / 缓存），
// 本接口管**每个存储各自的卷**（一个 thin LV + 其文件系统 + 挂载点）。
//
// Windows 后端整体返回 ErrUnsupported（VHDX 直接落在 NTFS 卷上，存储就是普通目录），
// app 层据此走"目录模式"，Windows 代码路径零改动。
//
// 所有方法都**必须幂等**：启动时会对全部启用中的存储逐个重放 MountStorageVolume。
type StorageVolumeBackend interface {
	// CreateStorageVolume 幂等地建卷 → 格式化 → 挂载 → **校验挂载确实生效**。
	//
	// 挂载校验是 fail-closed 的：挂载命令返回成功但 mountinfo 里查不到时，
	// 必须报错（否则写入会静默落在宿主根文件系统上，日后挂载再把数据"藏"起来）。
	CreateStorageVolume(ctx context.Context, spec StorageVolumeSpec) (*StorageVolumeStatus, error)

	// MountStorageVolume 幂等地把卷挂载到 mountPoint（已挂载则直接返回）。
	MountStorageVolume(ctx context.Context, ref, mountPoint string) error

	// UnmountStorageVolume 幂等地卸载（未挂载视为成功）。
	UnmountStorageVolume(ctx context.Context, ref, mountPoint string) error

	// ResizeStorageVolume 扩容到 sizeBytes（**只扩不缩**），并在线扩展文件系统。
	ResizeStorageVolume(ctx context.Context, ref string, sizeBytes int64) error

	// DeleteStorageVolume 卸载后删除底层卷。幂等（不存在视为成功）。
	//
	// 调用方必须先确认该卷是系统创建的（managed），登记已有的卷绝不可走此方法。
	DeleteStorageVolume(ctx context.Context, ref string) error

	// StorageVolumeStatus 查询卷现状（存在性 / 挂载状态 / 容量 / 健康）。
	StorageVolumeStatus(ctx context.Context, ref, mountPoint string) (*StorageVolumeStatus, error)
}

// TargetSpec 是一个 iSCSI 目标的**完整期望状态**（全量语义，非增量）。
//
// 之所以用全量：Windows IscsiTarget 模块的 -InitiatorIds 本身就是整体覆盖，
// Linux LIO 的 configfs 写入同样是"以目录内容为准"，两边都天然适合全量下发。
type TargetSpec struct {
	// Name 目标名（IQN）。
	Name string
	// Enabled 目标是否启用。
	Enabled bool
	// ReadOnly 是否为只读目标。
	//
	// Windows 后端忽略该字段（沿用既有行为：只读由客户端挂载方式承担）；
	// Linux(LIO) 后端落实为 per-ACL 的 mapped_lun write_protect=1。
	ReadOnly bool
	// Initiators 授权白名单（全量替换；空切片表示清空）。
	Initiators []string
	// ChapUser / ChapSecret 同时非空时启用单向 CHAP。密钥绝不进日志。
	ChapUser   string
	ChapSecret string
	// EnableReverseChap 为 true 时启用反向 CHAP。
	EnableReverseChap bool
	ReverseChapUser   string
	ReverseChapSecret string
	// BackingRef 该目标发布的虚拟磁盘引用；为空表示不发布任何盘。
	BackingRef string
}

// TargetInfo 是目标的实际状态快照。
type TargetInfo struct {
	Name string
	// IQN 平台侧**实际对外**的 IQN（Windows 会改写目标名，客户端登录必须用它）；
	// 与 Name（`-TargetName` 寻址用的名字）不同。可能为空（Linux LIO 二者一致）。
	IQN     string
	Enabled bool
	// Initiators 已授权的 initiator（Windows 形如 "IQN:iqn.xxx"，Linux 为纯 IQN）。
	Initiators []string
	// Devices 已映射的虚拟磁盘引用。
	Devices []string
}

// Session 是一条 iSCSI 会话（Linux LIO 可真实枚举；Windows 不支持）。
type Session struct {
	// ID 会话标识（LIO 为 <sid>）。
	ID string
	// InitiatorName 对端 initiator IQN。
	InitiatorName string
	// TargetName 本端目标名。
	TargetName string
	// State 会话状态描述（平台自由文本）。
	State string
}

// IscsiBackend 是 iSCSI 目标后端。
type IscsiBackend interface {
	// Kind 返回平台种类。
	Kind() Kind

	// Available 探测 iSCSI 目标服务是否可用。
	Available(ctx context.Context) error

	// EnsureTarget 幂等地把目标收敛到 spec 描述的期望状态（不存在则创建）。
	EnsureTarget(ctx context.Context, spec TargetSpec) error

	// RemoveTarget 删除目标（含其名下全部映射与授权）。幂等。
	RemoveTarget(ctx context.Context, name string) error

	// GetTarget 查询单个目标；不存在时返回 iscsi.target_not_found。
	GetTarget(ctx context.Context, name string) (*TargetInfo, error)

	// ListTargets 列出全部目标。
	ListTargets(ctx context.Context) ([]TargetInfo, error)

	// ListSessions 枚举指定目标的在线会话（name 为空表示全部）。
	// 平台不支持时返回 ErrUnsupported。
	ListSessions(ctx context.Context, name string) ([]Session, error)

	// ForceLogout 强制登出指定会话。
	//
	// ⚠️ 能力差异：Windows 与 Linux(LIO) 都**没有**精确到会话的服务端登出，
	// 实现只能做"停用目标 + 拆除授权"的降级动作，并在日志中明确标注为降级。
	// 平台完全不支持时返回 ErrUnsupported。
	ForceLogout(ctx context.Context, name, sessionID string) error

	// EnsureVirtualDisk 幂等地把已有虚拟磁盘纳入 iSCSI 虚拟盘登记（已登记则跳过）。
	EnsureVirtualDisk(ctx context.Context, ref, description string) error

	// RemoveVirtualDisk 移除虚拟盘登记（**不删除底层数据**）。幂等。
	RemoveVirtualDisk(ctx context.Context, ref string) error

	// AttachLun 建立「虚拟磁盘 ↔ 目标」映射。幂等。
	AttachLun(ctx context.Context, targetName, ref string, readOnly bool) error

	// DetachLun 解除映射。幂等（未映射视为成功）。
	DetachLun(ctx context.Context, targetName, ref string) error
}

// BackendCapabilities 是后端**可选**实现的能力报告接口。
//
// 用于暴露平台特有、且无法用平台中性字段表达的探测结果（如 Windows 的 Hyper-V 模块）。
// app 层通过类型断言按需读取；未实现的后端返回"无额外能力"，不是错误。
type BackendCapabilities interface {
	// Capabilities 返回平台特有能力的键值快照。
	//
	// 约定键：
	//   - "hyperv"：Hyper-V 模块可用（影响 Windows 的 ResetDiskIdentifier / Optimize-VHD 路径）。
	Capabilities(ctx context.Context) map[string]bool
}

// BlockDevice 是一块可被选作 LVM 物理卷的块设备（仅 Linux 有意义）。
type BlockDevice struct {
	// Name 内核短名，如 "sdb"。
	Name string
	// Path 设备全路径，如 "/dev/sdb"。
	Path string
	// SizeBytes 容量（字节）。
	SizeBytes int64
	// Model 型号（可能为空）。
	Model string
	// Rotational 为 true 表示机械盘（HDD）；false 为固态（SSD/NVMe）。
	Rotational bool
	// System 为 true 表示承载根文件系统/引导分区/swap —— **禁止被选作缓存盘**。
	System bool
	// HasFS 设备或其子分区上已有文件系统。
	HasFS bool
	// HasPV 设备或其子分区已被 LVM 使用。
	HasPV bool
	// VGName 若已是某 VG 的成员，给出 VG 名。
	VGName string
	// Partitions 子分区数量（>0 时通常不宜直接作 PV）。
	Partitions int
	// Reason 不可选用时给出原因（供前端直接展示；可选用时为空）。
	Reason string
}

// CacheSpec 描述"用哪些块设备做 dm-cache"。
//
// 本系统**不实现任何自定义缓存逻辑**：只是把选定的块设备交给 LVM 原生 dm-cache。
type CacheSpec struct {
	// Devices 用作缓存的块设备全路径（必须与目标 VG 同属一个 VG 才可行）。
	Devices []string
	// ChunkSize 缓存块大小（如 "256K"；须为 32K 的整数倍，范围 32K–1G）。
	ChunkSize string
	// Policy 缓存策略，默认 smq。
	Policy string
	// Mode 缓存模式：writethrough（默认、安全）| writeback（SSD 故障可能丢数据）。
	Mode string
}

// PoolSpec 描述一次"存储池初始化"（一次性动作，幂等）。
type PoolSpec struct {
	// VG 目标卷组名。
	VG string
	// ThinPool 目标 thin pool 名。
	ThinPool string
	// HDDDevices 组成 VG 的块设备全路径（VG 已存在时忽略）。
	HDDDevices []string
	// ChunkSize thin pool 的 chunk 大小，如 "256K"。
	ChunkSize string
	// MetadataSize thin pool 元数据大小，如 "4G"。
	MetadataSize string
	// Cache 可选的缓存配置（nil 表示不加缓存）。
	Cache *CacheSpec
}

// PoolStatus 是存储池与缓存的现状快照（只读展示用）。
type PoolStatus struct {
	Kind     Kind
	VG       string
	ThinPool string
	// Exists 目标 VG 与 thin pool 是否都已存在。
	Exists bool
	// SizeBytes / FreeBytes VG 的总容量与剩余空间（字节）。
	SizeBytes int64
	FreeBytes int64
	// DataPercent / MetadataPercent thin pool 的数据/元数据使用率（0–100）。
	DataPercent     float64
	MetadataPercent float64
	// CacheAttached 是否已挂载 dm-cache。
	CacheAttached bool
	// CacheMode / CacheChunkSize / CachePolicy 缓存配置。
	CacheMode      string
	CacheChunkSize string
	CachePolicy    string
	// CacheDirtyBlocks 未回写的脏块数（writeback 模式下尤其关键）。
	CacheDirtyBlocks int64
	// CacheReadHits / CacheReadMisses / CacheWriteHits / CacheWriteMisses 命中统计。
	CacheReadHits    int64
	CacheReadMisses  int64
	CacheWriteHits   int64
	CacheWriteMisses int64
	// CacheDevices 正在充当缓存的设备。
	CacheDevices []string
	// CacheHealth 缓存健康状态（空表示正常；否则给出 Fail/needs_check 等）。
	CacheHealth string
}

// StorageAdmin 是"存储池与缓存设备"的平台管理能力。
//
// Windows 后端整体返回 ErrUnsupported（VHDX 直接落在 NTFS 卷上，没有"池"的概念）。
type StorageAdmin interface {
	// ListBlockDevices 枚举可选的块设备（供"缓存设备选择器"使用）。
	ListBlockDevices(ctx context.Context) ([]BlockDevice, error)

	// Status 返回存储池与缓存的现状。
	Status(ctx context.Context) (*PoolStatus, error)

	// InitializePool 幂等地初始化存储池（含可选的缓存设备）。
	// 目标已存在时跳过创建并返回现状，绝不覆盖既有数据。
	InitializePool(ctx context.Context, spec PoolSpec) error
}

package domain

// User 本地用户。作为分配资源的载体。
type User struct {
	ID           string `db:"id" json:"id"`
	Username     string `db:"username" json:"username"`
	Role         Role   `db:"role" json:"role"`
	PasswordHash string `db:"password_hash" json:"-"` // 永不外泄
	Enabled      bool   `db:"enabled" json:"enabled"`
	QuotaBytes   int64  `db:"quota_bytes" json:"quota_bytes"` // 0 表示不限
	UsedBytes    int64  `db:"used_bytes" json:"used_bytes"`
	Remark       string `db:"remark" json:"remark"`
	CreatedAt    int64  `db:"created_at" json:"created_at"`
	UpdatedAt    int64  `db:"updated_at" json:"updated_at"`
}

// IsSuperAdmin 判断是否为超级管理员。
func (u *User) IsSuperAdmin() bool { return u.Role == RoleSuperAdmin }

// Certificate 客户端证书。
//
// 身份绑定基于 SPKI（公钥）指纹而非证书指纹：
// 续期时保留密钥对 → SPKI 不变 → 无需重新审批（见 docs/implementation.md 9.1 约束 2）。
type Certificate struct {
	ID string `db:"id" json:"id"`
	// UserID 所属用户。
	UserID string `db:"user_id" json:"user_id"`
	// Serial 证书序列号。
	Serial string `db:"serial" json:"serial"`
	// Fingerprint 证书 DER 的 SHA-256，用于排障与吊销展示。
	Fingerprint string `db:"fingerprint" json:"fingerprint"`
	// SPKISHA256 公钥 SubjectPublicKeyInfo 的 SHA-256，**身份绑定与续期比对使用该值**。
	SPKISHA256 string `db:"spki_sha256" json:"spki_sha256"`
	// Status active | revoked。
	Status string `db:"status" json:"status"`
	// BoundIP 允许来源 IP/CIDR，作为附加收紧策略，非身份来源。
	BoundIP string `db:"bound_ip" json:"bound_ip"`
	// BoundMAC 仅作展示与排障线索，**不参与鉴权**（见 docs/implementation.md 13.3-⑯）。
	BoundMAC  string `db:"bound_mac" json:"bound_mac"`
	NotBefore int64  `db:"not_before" json:"not_before"`
	NotAfter  int64  `db:"not_after" json:"not_after"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
}

// CertificateStatusActive 有效证书状态。
const CertificateStatusActive = "active"

// CertificateStatusRevoked 已吊销。
const CertificateStatusRevoked = "revoked"

// Storage 是一个可在线管理的"存储"：允许在其下创建 VHDX 的根目录。
//
// 数据来源：数据库是**唯一真源**；config.yaml 的 storage.whitelist_roots 仅作为
// 首次启动的一次性种子（见 cmd/vault-server 的初始化流程），之后请在管理端维护。
// 新建磁盘时只在 Enabled 为 true 的存储中选择（见 app.StorageService）。
type Storage struct {
	ID string `db:"id" json:"id"`
	// Name 展示用名称，全局唯一（应用层按大小写不敏感校验）。
	Name string `db:"name" json:"name"`
	// Path 绝对路径根目录，全局唯一（应用层按大小写不敏感校验）；
	// 所有放置到该存储的 VHDX 只能是其下的绝对路径。
	Path string `db:"path" json:"path"`
	// Enabled 为 false 时不再参与新磁盘放置，但已有磁盘记录仍可读取/维护。
	Enabled   bool  `db:"enabled" json:"enabled"`
	CreatedAt int64 `db:"created_at" json:"created_at"`
	UpdatedAt int64 `db:"updated_at" json:"updated_at"`
}

// StorageVolume 是存储底层卷的登记信息（Linux 的 thin LV / 已有 LV 才有值）。
//
// **无对应记录 = 目录模式**（Windows 全量，或 Linux 未登记卷信息的历史存储）：
// 此时存储只作为 storages.path 指向的普通目录使用。
//
// 挂载点一律取 storages.path，此处不重复存储（避免双写漂移）。
type StorageVolume struct {
	// StorageID 与 storages.id 一一对应（主键）。
	StorageID string `db:"storage_id" json:"storage_id"`
	// Kind 卷类型：thin（系统创建的 thin LV）| lv（登记已有 LV）| dir（登记已有目录）。
	Kind string `db:"kind" json:"kind"`
	// Managed 为 true 时系统才有权删除该卷；登记已有卷/目录必须为 false，避免误删用户数据。
	Managed bool `db:"managed" json:"managed"`
	// State pending（创建中途，可能留下无主 LV）| ready（可用）。
	State string `db:"state" json:"state"`
	// Ref 设备引用 /dev/mapper/<vg>-<lv>；kind=dir 时为空。
	Ref string `db:"ref" json:"ref"`
	// FileSystem 文件系统类型（ext4 / xfs 等）；dir 模式为空。
	FileSystem string `db:"file_system" json:"file_system"`
	// SizeBytes 分配容量（LV 虚拟大小）；dir 模式为 0。
	SizeBytes int64 `db:"size_bytes" json:"size_bytes"`
	CreatedAt int64 `db:"created_at" json:"created_at"`
	UpdatedAt int64 `db:"updated_at" json:"updated_at"`
}

// 存储卷类型取值（见 StorageVolume.Kind）。
const (
	// StorageVolumeKindThin 系统创建的 thin LV。
	StorageVolumeKindThin = "thin"
	// StorageVolumeKindLV 登记已有的 LV。
	StorageVolumeKindLV = "lv"
	// StorageVolumeKindDir 登记已有的目录（无底层卷）。
	StorageVolumeKindDir = "dir"
)

// 存储卷状态取值（见 StorageVolume.State）。
const (
	// StorageVolumeStatePending 创建中途（lvcreate 成功但尚未挂载就绪）。
	StorageVolumeStatePending = "pending"
	// StorageVolumeStateReady 就绪可用。
	StorageVolumeStateReady = "ready"
)

// RepoMeta 是存储库的"逻辑记录字段"，用于存放**纯客户端展示/配置**数据。
//
// 注意：挂载后执行脚本属于可执行内容，需版本化与审计，
// 按 docs/implementation.md 13.2-⑨ 应独立成表（repo_scripts），不放入此处。
type RepoMeta struct {
	// Group 分组标签，用于客户端归类展示（多服务端下可同名聚合）。
	Group string `json:"group,omitempty"`
	// ClientConfig 面向客户端的附加配置。
	ClientConfig map[string]any `json:"client_config,omitempty"`
}

// Repository 是面向用户的"存储库"，封装磁盘 + iSCSI + 授权 + 密钥。
type Repository struct {
	ID      string   `db:"id" json:"id"`
	Name    string   `db:"name" json:"name"`
	Mode    RepoMode `db:"mode" json:"mode"`
	OwnerID string   `db:"owner_id" json:"owner_id"`
	// ParentDiskID 共享模式下的母盘 ID。
	ParentDiskID *string `db:"parent_disk_id" json:"parent_disk_id,omitempty"`
	// ParentVersion 母盘版本号。每次进入维护并更新内容后 +1；
	// 差异盘记录创建时基于的版本，挂载前需比对一致。
	ParentVersion int `db:"parent_version" json:"parent_version"`
	// ParentCondition 母盘用途条件，仅共享模式有效。
	ParentCondition *ParentCondition `db:"parent_condition" json:"parent_condition,omitempty"`
	// MaxDiffDisks 单母盘差异盘上限，创建时由 owner 配置。
	MaxDiffDisks int `db:"max_diff_disks" json:"max_diff_disks"`
	// QuotaBytes 应用层配额（母盘占用由 owner 独担，见 docs/implementation.md 13.2-⑧）。
	QuotaBytes int64 `db:"quota_bytes" json:"quota_bytes"`
	// UsedBytes 已用量（母盘 + 本库名下差异盘物理占用）。
	UsedBytes int64     `db:"used_bytes" json:"used_bytes"`
	State     RepoState `db:"state" json:"state"`
	// Meta 逻辑记录字段，JSON 序列化存储。
	Meta      RepoMeta `db:"meta" json:"meta"`
	CreatedAt int64    `db:"created_at" json:"created_at"`
	UpdatedAt int64    `db:"updated_at" json:"updated_at"`
}

// IsShared 判断是否为共享模式。
func (r *Repository) IsShared() bool { return r.Mode == RepoModeShared }

// Condition 返回母盘条件；非共享模式返回 idle 作为中性值。
func (r *Repository) Condition() ParentCondition {
	if r.ParentCondition == nil {
		return ParentIdle
	}
	return *r.ParentCondition
}

// RepoMember 可管理列表项。仅 owner 可设置。
type RepoMember struct {
	RepoID string     `db:"repo_id" json:"repo_id"`
	UserID string     `db:"user_id" json:"user_id"`
	Perm   Permission `db:"perm" json:"perm"`
}

// Disk 是 VHDX 的元数据记录。
type Disk struct {
	ID     string   `db:"id" json:"id"`
	RepoID string   `db:"repo_id" json:"repo_id"`
	Kind   DiskKind `db:"kind" json:"kind"`
	// VHDXPath 白名单根下的绝对路径，全局唯一。
	VHDXPath string `db:"vhdx_path" json:"vhdx_path"`
	// ParentID 差异盘指向其母盘。
	ParentID *string `db:"parent_id" json:"parent_id,omitempty"`
	// ParentVersion 差异盘创建时母盘的版本号，用于挂载前一致性校验。
	ParentVersion int `db:"parent_version" json:"parent_version"`
	// ContentFingerprint 母盘内容指纹（size + mtime + 头部哈希），用于检测"母盘被绕过改写"。
	ContentFingerprint string `db:"content_fingerprint" json:"content_fingerprint"`
	// SizeBytes 逻辑大小。
	SizeBytes int64 `db:"size_bytes" json:"size_bytes"`
	// PhysicalBytes 实际占用，由监控任务定期采样。
	PhysicalBytes int64     `db:"physical_bytes" json:"physical_bytes"`
	VHDType       VHDType   `db:"vhd_type" json:"vhd_type"`
	State         DiskState `db:"state" json:"state"`
	// DesiredState 期望状态，供对账器使用。
	DesiredState string `db:"desired_state" json:"desired_state"`
	// ObservedState 上一次对账观测到的实际状态。
	ObservedState string `db:"observed_state" json:"observed_state"`
	// Mounted 是否在本机挂载（服务端本地挂载，用于维护/建盘）。
	Mounted   bool  `db:"mounted" json:"mounted"`
	CreatedAt int64 `db:"created_at" json:"created_at"`
	UpdatedAt int64 `db:"updated_at" json:"updated_at"`
}

// Allocation 是把某个磁盘分配给某用户的记录。
//
// 共享模式下每条分配对应一个**独立差异盘 + 独立 iSCSI 目标**
// （见 docs/implementation.md 5.3：CHAP 无法在同一 target 内区分多用户）。
type Allocation struct {
	ID     string          `db:"id" json:"id"`
	RepoID string          `db:"repo_id" json:"repo_id"`
	DiskID string          `db:"disk_id" json:"disk_id"`
	UserID string          `db:"user_id" json:"user_id"`
	State  AllocationState `db:"state" json:"state"`
	// UserID 为 "@temp" 时表示母盘临时共享的占位分配。
	CreatedAt int64 `db:"created_at" json:"created_at"`
	UpdatedAt int64 `db:"updated_at" json:"updated_at"`
}

// TempAllocationUserID 是母盘临时共享使用的占位用户 ID。
//
// 用显式记录而非内存标志位，保证服务端重启后仍能识别并清理临时共享（见 docs/implementation.md 5.6）。
const TempAllocationUserID = "@temp"

// AuditLog 审计记录。
type AuditLog struct {
	ID        string `db:"id" json:"id"`
	UserID    string `db:"user_id" json:"user_id"`
	Action    string `db:"action" json:"action"`
	Resource  string `db:"resource" json:"resource"`
	Detail    string `db:"detail" json:"detail"`
	IP        string `db:"ip" json:"ip"`
	Result    string `db:"result" json:"result"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
}

// 审计结果取值。
const (
	AuditResultOK     = "ok"
	AuditResultDenied = "denied"
	AuditResultError  = "error"
)

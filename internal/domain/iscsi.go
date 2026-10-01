package domain

// IscsiTarget 是一个 iSCSI 目标。
//
// 授权模型：**一个分配对应一个独立目标**，因为 Windows 的 CHAP 是"每目标一组账号"，
// 无法在同一目标内区分多用户（见 docs/implementation.md 5.3）。
type IscsiTarget struct {
	ID string `db:"id" json:"id"`
	// TargetName 目标名，同时是 IQN 后缀；全局唯一。
	TargetName string `db:"target_name" json:"target_name"`
	// DiskID 映射的虚拟盘。临时共享目标指向母盘。
	DiskID *string `db:"disk_id" json:"disk_id,omitempty"`
	// Purpose user | temp_parent。
	Purpose Purpose `db:"purpose" json:"purpose"`
	// AllocationID 该目标服务的分配（purpose=user 时必有）。
	AllocationID *string  `db:"allocation_id" json:"allocation_id,omitempty"`
	AuthMode     AuthMode `db:"auth_mode" json:"auth_mode"`
	// ChapUser CHAP 用户名（密钥加密存储，绝不出现在 API 响应中）。
	ChapUser string `db:"chap_user" json:"chap_user,omitempty"`
	// ChapSecretEnc AES-GCM 密文，禁止明文落库。
	ChapSecretEnc []byte `db:"chap_secret_enc" json:"-"`
	// ReverseChapSecretEnc 反向 CHAP 密钥密文。
	ReverseChapSecretEnc []byte `db:"reverse_chap_secret_enc" json:"-"`
	// Enabled 当前实际启用状态（对应 Set-IscsiServerTarget -Enable）。
	Enabled bool `db:"enabled" json:"enabled"`
	// DesiredEnabled 期望启用状态，供对账器收敛。
	DesiredEnabled bool `db:"desired_enabled" json:"desired_enabled"`
	// ReadOnly 是否以只读方式发布（母盘临时共享必须为只读）。
	ReadOnly  bool  `db:"read_only" json:"read_only"`
	CreatedAt int64 `db:"created_at" json:"created_at"`
	UpdatedAt int64 `db:"updated_at" json:"updated_at"`
}

// IQN 返回完整的 iSCSI 限定名。
//
// base 为服务端配置的 IQN 前缀（如 iqn.2026-01.com.vault）。
func (t *IscsiTarget) IQN(base string) string {
	return base + ":" + t.TargetName
}

// IscsiInitiatorID 是目标的一条 initiator 白名单。
type IscsiInitiatorID struct {
	TargetID string          `db:"target_id" json:"target_id"`
	IDType   InitiatorIDType `db:"id_type" json:"id_type"`
	Value    string          `db:"value" json:"value"`
}

// ToInitiatorID 转为值对象。
func (i IscsiInitiatorID) ToInitiatorID() InitiatorID {
	return InitiatorID{Type: i.IDType, Value: i.Value}
}

// Lease 是客户端与服务端之间的连接契约。
//
// 由于 Windows iSCSI Target Server 既无会话枚举也无强制登出 API，
// 租约/心跳是"在线状态"的**唯一权威来源**（见 docs/implementation.md 5.7）。
type Lease struct {
	ID string `db:"id" json:"id"`
	// AllocationID 关联的分配。
	AllocationID string `db:"allocation_id" json:"allocation_id"`
	TargetName   string `db:"target_name" json:"target_name"`
	UserID       string `db:"user_id" json:"user_id"`
	// ClientID 客户端实例标识，全局唯一且稳定（基于设备与安装实例，不使用 MAC/IP）。
	ClientID string `db:"client_id" json:"client_id"`
	// OwnerToken 防止他人抢占同一租约。
	OwnerToken string `db:"owner_token" json:"-"`
	// MountPoint 客户端上报的挂载点（盘符或目录路径）。
	MountPoint string     `db:"mount_point" json:"mount_point"`
	State      LeaseState `db:"state" json:"state"`
	// ExpiresAt 到期时间，被 Reaper 扫描判定离线。
	ExpiresAt  int64 `db:"expires_at" json:"expires_at"`
	LastSeenAt int64 `db:"last_seen_at" json:"last_seen_at"`
	CreatedAt  int64 `db:"created_at" json:"created_at"`
}

// IsExpired 判断租约在给定时刻是否已过期。
func (l *Lease) IsExpired(nowMillis int64) bool {
	return l.State == LeaseStateActive && nowMillis >= l.ExpiresAt
}

// Job 是异步任务。所有长耗时操作都要落到这里（见 docs/implementation.md 5.12）。
type Job struct {
	ID   string  `db:"id" json:"id"`
	Type JobType `db:"type" json:"type"`
	// RefID 关联的业务实体 ID。
	RefID string `db:"ref_id" json:"ref_id"`
	// IdemKey 幂等键，唯一索引；重复提交直接返回已有任务。
	IdemKey string `db:"idem_key" json:"idem_key"`
	// Payload JSON 序列化的任务参数。
	Payload string   `db:"payload" json:"payload"`
	State   JobState `db:"state" json:"state"`
	// Progress 0-100。
	Progress int `db:"progress" json:"progress"`
	// Attempt 已尝试次数。
	Attempt int `db:"attempt" json:"attempt"`
	// LastError 最近一次失败原因。
	LastError string `db:"last_error" json:"last_error"`
	// LockKey 用于资源互斥：同一 LockKey 的任务串行执行。
	LockKey    string `db:"lock_key" json:"lock_key"`
	CreatedAt  int64  `db:"created_at" json:"created_at"`
	StartedAt  int64  `db:"started_at" json:"started_at"`
	FinishedAt int64  `db:"finished_at" json:"finished_at"`
}

// Upload 是大目录分块上传会话（见 docs/implementation.md 5.10）。
type Upload struct {
	ID     string `db:"id" json:"id"`
	UserID string `db:"user_id" json:"user_id"`
	// RepoName 目标存储库名。
	RepoName string `db:"repo_name" json:"repo_name"`
	// Mode copy | move。
	Mode string `db:"mode" json:"mode"`
	// TotalFiles / TotalBytes 由客户端 manifest 提供。
	TotalFiles    int   `db:"total_files" json:"total_files"`
	TotalBytes    int64 `db:"total_bytes" json:"total_bytes"`
	ReceivedBytes int64 `db:"received_bytes" json:"received_bytes"`
	// StagingDir 暂存目录，必须与 disks 目录同卷。
	StagingDir string `db:"staging_dir" json:"staging_dir"`
	// Manifest JSON 序列化的文件清单。
	Manifest  string `db:"manifest" json:"manifest"`
	State     string `db:"state" json:"state"`
	CreatedAt int64  `db:"created_at" json:"created_at"`
	UpdatedAt int64  `db:"updated_at" json:"updated_at"`
}

// 上传状态。
const (
	UploadStateOpen      = "open"
	UploadStateVerifying = "verifying"
	UploadStateComplete  = "complete"
	UploadStateFailed    = "failed"
	UploadStateAborted   = "aborted"
)

// 上传模式。
const (
	UploadModeCopy = "copy"
	UploadModeMove = "move"
)

// UploadChunk 是单个上传分块。
type UploadChunk struct {
	UploadID   string `db:"upload_id" json:"upload_id"`
	ChunkIndex int    `db:"chunk_index" json:"chunk_index"`
	// RelPath 相对于源根目录的路径，已做规范化与安全检查。
	RelPath    string `db:"rel_path" json:"rel_path"`
	Offset     int64  `db:"offset" json:"offset"`
	Size       int64  `db:"size" json:"size"`
	Checksum   string `db:"checksum" json:"checksum"`
	ReceivedAt int64  `db:"received_at" json:"received_at"`
}

// ManifestEntry 是 manifest 中的单条文件记录。
type ManifestEntry struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime,omitempty"`
	// SHA256 可选，用于完整性校验。
	SHA256 string `json:"sha256,omitempty"`
}

// UploadManifest 是上传清单。
type UploadManifest struct {
	Root  string          `json:"root"`
	Files []ManifestEntry `json:"files"`
}

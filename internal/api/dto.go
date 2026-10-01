package api

import (
	"vault/internal/app"
	"vault/internal/domain"
)

// 说明：本文件集中定义对外 DTO。
//
// 铁律（见 docs/implementation.md 6.5 与 9.4）：
//   - 绝不直接序列化 domain 实体；
//   - 任何形态都不得包含 password_hash、chap_secret_enc、owner_token 等敏感字段。

type userDTO struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	Enabled    bool   `json:"enabled"`
	QuotaBytes int64  `json:"quota_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
	Remark     string `json:"remark"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

func toUserDTO(u *domain.User) userDTO {
	return userDTO{
		ID: u.ID, Username: u.Username, Role: string(u.Role), Enabled: u.Enabled,
		QuotaBytes: u.QuotaBytes, UsedBytes: u.UsedBytes, Remark: u.Remark,
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt,
	}
}

func toUserDTOs(users []domain.User) []userDTO {
	out := make([]userDTO, 0, len(users))
	for i := range users {
		out = append(out, toUserDTO(&users[i]))
	}
	return out
}

type certificateDTO struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	Serial      string `json:"serial"`
	Fingerprint string `json:"fingerprint"`
	SPKISHA256  string `json:"spki_sha256"`
	Status      string `json:"status"`
	BoundIP     string `json:"bound_ip"`
	BoundMAC    string `json:"bound_mac"`
	NotBefore   int64  `json:"not_before"`
	NotAfter    int64  `json:"not_after"`
	CreatedAt   int64  `json:"created_at"`
}

// IssuedCertificateDTO 是"服务端生成密钥对并签发客户端证书"的一次性结果。
//
// ⚠️ key_pem 仅在本次响应中返回一次：服务端绝不落库、绝不写日志、绝不写审计详情。
type IssuedCertificateDTO struct {
	CertPEM           string `json:"cert_pem"`
	KeyPEM            string `json:"key_pem"`
	CAPEM             string `json:"ca_pem"`
	Serial            string `json:"serial"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
	SPKISHA256        string `json:"spki_sha256"`
	NotBefore         int64  `json:"not_before"`
	NotAfter          int64  `json:"not_after"`
}

// toIssuedCertificateDTO 转换一次性签发结果；入参为 nil 时返回 nil。
func toIssuedCertificateDTO(issued *app.IssuedClientCertificate) *IssuedCertificateDTO {
	if issued == nil {
		return nil
	}
	return &IssuedCertificateDTO{
		CertPEM:           string(issued.CertPEM),
		KeyPEM:            string(issued.KeyPEM),
		CAPEM:             string(issued.CAPEM),
		Serial:            issued.Serial,
		FingerprintSHA256: issued.FingerprintSHA256,
		SPKISHA256:        issued.SPKISHA256,
		NotBefore:         issued.NotBefore.UnixMilli(),
		NotAfter:          issued.NotAfter.UnixMilli(),
	}
}

func toCertificateDTOs(certs []domain.Certificate) []certificateDTO {
	out := make([]certificateDTO, 0, len(certs))
	for i := range certs {
		c := &certs[i]
		out = append(out, certificateDTO{
			ID: c.ID, UserID: c.UserID, Serial: c.Serial, Fingerprint: c.Fingerprint,
			SPKISHA256: c.SPKISHA256, Status: c.Status, BoundIP: c.BoundIP, BoundMAC: c.BoundMAC,
			NotBefore: c.NotBefore, NotAfter: c.NotAfter, CreatedAt: c.CreatedAt,
		})
	}
	return out
}

type repoDTO struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Mode            string         `json:"mode"`
	OwnerID         string         `json:"owner_id"`
	ParentDiskID    string         `json:"parent_disk_id,omitempty"`
	ParentVersion   int            `json:"parent_version"`
	ParentCondition string         `json:"parent_condition,omitempty"`
	MaxDiffDisks    int            `json:"max_diff_disks"`
	QuotaBytes      int64          `json:"quota_bytes"`
	UsedBytes       int64          `json:"used_bytes"`
	State           string         `json:"state"`
	Group           string         `json:"group,omitempty"`
	ClientConfig    map[string]any `json:"client_config,omitempty"`
	CreatedAt       int64          `json:"created_at"`
	UpdatedAt       int64          `json:"updated_at"`
}

func toRepoDTO(r *domain.Repository) repoDTO {
	dto := repoDTO{
		ID: r.ID, Name: r.Name, Mode: string(r.Mode), OwnerID: r.OwnerID,
		ParentVersion: r.ParentVersion, MaxDiffDisks: r.MaxDiffDisks,
		QuotaBytes: r.QuotaBytes, UsedBytes: r.UsedBytes, State: string(r.State),
		Group: r.Meta.Group, ClientConfig: r.Meta.ClientConfig,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.ParentDiskID != nil {
		dto.ParentDiskID = *r.ParentDiskID
	}
	if r.ParentCondition != nil {
		dto.ParentCondition = string(*r.ParentCondition)
	}
	return dto
}

func toRepoDTOs(repos []domain.Repository) []repoDTO {
	out := make([]repoDTO, 0, len(repos))
	for i := range repos {
		out = append(out, toRepoDTO(&repos[i]))
	}
	return out
}

type memberDTO struct {
	UserID string `json:"user_id"`
	Perm   string `json:"perm"`
}

type diskDTO struct {
	ID            string `json:"id"`
	RepoID        string `json:"repo_id"`
	Kind          string `json:"kind"`
	VHDXPath      string `json:"vhdx_path"`
	ParentID      string `json:"parent_id,omitempty"`
	ParentVersion int    `json:"parent_version"`
	SizeBytes     int64  `json:"size_bytes"`
	PhysicalBytes int64  `json:"physical_bytes"`
	VHDType       string `json:"vhd_type"`
	State         string `json:"state"`
	Mounted       bool   `json:"mounted"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

func toDiskDTO(d *domain.Disk) diskDTO {
	dto := diskDTO{
		ID: d.ID, RepoID: d.RepoID, Kind: string(d.Kind), VHDXPath: d.VHDXPath,
		ParentVersion: d.ParentVersion, SizeBytes: d.SizeBytes, PhysicalBytes: d.PhysicalBytes,
		VHDType: string(d.VHDType), State: string(d.State), Mounted: d.Mounted,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
	if d.ParentID != nil {
		dto.ParentID = *d.ParentID
	}
	return dto
}

func toDiskDTOs(disks []domain.Disk) []diskDTO {
	out := make([]diskDTO, 0, len(disks))
	for i := range disks {
		out = append(out, toDiskDTO(&disks[i]))
	}
	return out
}

type allocationDTO struct {
	ID        string `json:"id"`
	RepoID    string `json:"repo_id"`
	DiskID    string `json:"disk_id"`
	UserID    string `json:"user_id"`
	State     string `json:"state"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func toAllocationDTO(a *domain.Allocation) allocationDTO {
	return allocationDTO{
		ID: a.ID, RepoID: a.RepoID, DiskID: a.DiskID, UserID: a.UserID,
		State: string(a.State), CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

func toAllocationDTOs(allocs []domain.Allocation) []allocationDTO {
	out := make([]allocationDTO, 0, len(allocs))
	for i := range allocs {
		out = append(out, toAllocationDTO(&allocs[i]))
	}
	return out
}

type targetDTO struct {
	ID             string `json:"id"`
	TargetName     string `json:"target_name"`
	DiskID         string `json:"disk_id,omitempty"`
	Purpose        string `json:"purpose"`
	AllocationID   string `json:"allocation_id,omitempty"`
	AuthMode       string `json:"auth_mode"`
	ChapUser       string `json:"chap_user,omitempty"`
	HasChapSecret  bool   `json:"has_chap_secret"`
	Enabled        bool   `json:"enabled"`
	DesiredEnabled bool   `json:"desired_enabled"`
	ReadOnly       bool   `json:"read_only"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// toTargetDTO 转换 iSCSI 目标。**绝不包含 chap_secret_enc 密文或明文密钥**。
func toTargetDTO(t *domain.IscsiTarget) targetDTO {
	dto := targetDTO{
		ID: t.ID, TargetName: t.TargetName, Purpose: string(t.Purpose),
		AuthMode: string(t.AuthMode), ChapUser: t.ChapUser,
		HasChapSecret: len(t.ChapSecretEnc) > 0,
		Enabled:       t.Enabled, DesiredEnabled: t.DesiredEnabled, ReadOnly: t.ReadOnly,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	if t.DiskID != nil {
		dto.DiskID = *t.DiskID
	}
	if t.AllocationID != nil {
		dto.AllocationID = *t.AllocationID
	}
	return dto
}

type leaseDTO struct {
	ID           string `json:"id"`
	AllocationID string `json:"allocation_id"`
	TargetName   string `json:"target_name"`
	UserID       string `json:"user_id"`
	ClientID     string `json:"client_id"`
	MountPoint   string `json:"mount_point"`
	State        string `json:"state"`
	ExpiresAt    int64  `json:"expires_at"`
	LastSeenAt   int64  `json:"last_seen_at"`
	CreatedAt    int64  `json:"created_at"`
}

// toLeaseDTO 转换租约。owner_token 用于防抢占，**不得外泄**。
func toLeaseDTO(l *domain.Lease) leaseDTO {
	return leaseDTO{
		ID: l.ID, AllocationID: l.AllocationID, TargetName: l.TargetName,
		UserID: l.UserID, ClientID: l.ClientID, MountPoint: l.MountPoint,
		State: string(l.State), ExpiresAt: l.ExpiresAt, LastSeenAt: l.LastSeenAt, CreatedAt: l.CreatedAt,
	}
}

func toLeaseDTOs(leases []domain.Lease) []leaseDTO {
	out := make([]leaseDTO, 0, len(leases))
	for i := range leases {
		out = append(out, toLeaseDTO(&leases[i]))
	}
	return out
}

type jobDTO struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	RefID      string `json:"ref_id"`
	State      string `json:"state"`
	Progress   int    `json:"progress"`
	Attempt    int    `json:"attempt"`
	Failed     bool   `json:"failed"`
	CreatedAt  int64  `json:"created_at"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
}

// toJobDTO 转换任务。
//
// 刻意不输出 idem_key 与 last_error：前者是内部去重键，
// 后者可能包含底层平台错误文本（错误文本只应进日志，见 6.5）。
func toJobDTO(j *domain.Job) jobDTO {
	return jobDTO{
		ID: j.ID, Type: string(j.Type), RefID: j.RefID, State: string(j.State),
		Progress: j.Progress, Attempt: j.Attempt, Failed: j.LastError != "",
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
}

func toJobDTOs(jobs []domain.Job) []jobDTO {
	out := make([]jobDTO, 0, len(jobs))
	for i := range jobs {
		out = append(out, toJobDTO(&jobs[i]))
	}
	return out
}

type auditDTO struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Action    string `json:"action"`
	Resource  string `json:"resource"`
	Detail    string `json:"detail"`
	IP        string `json:"ip"`
	Result    string `json:"result"`
	CreatedAt int64  `json:"created_at"`
}

func toAuditDTOs(logs []domain.AuditLog) []auditDTO {
	out := make([]auditDTO, 0, len(logs))
	for i := range logs {
		l := &logs[i]
		out = append(out, auditDTO{
			ID: l.ID, UserID: l.UserID, Action: l.Action, Resource: l.Resource,
			Detail: l.Detail, IP: l.IP, Result: l.Result, CreatedAt: l.CreatedAt,
		})
	}
	return out
}

// storageDTO 是"存储"（可在线管理的 VHDX 根目录）视图。
//
// 统计字段说明：file_system / free_bytes / total_bytes / volume_name 为所在卷信息
// （同卷的多个存储取值相同）；disk_count / used_bytes 为该存储目录下的磁盘汇总。
//
// 底层卷字段（Linux）：kind = thin | lv | dir（空串表示目录模式/历史存储）；
// mounted 为底层卷是否已挂载就绪（未挂载的存储会被剔除出落盘集合）；ref 为设备引用；
// size_bytes 为分配容量（LV 虚拟大小）。Windows 上没有底层卷概念，mounted 恒为 true。
type storageDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	FileSystem string `json:"file_system"`
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
	VolumeName string `json:"volume_name"`
	DiskCount  int    `json:"disk_count"`
	UsedBytes  int64  `json:"used_bytes"`
	// Kind 底层卷类型（thin | lv | dir）；空 = 目录模式。
	Kind string `json:"kind"`
	// Mounted 底层卷是否已挂载就绪（目录模式恒为 true）。
	Mounted bool `json:"mounted"`
	// Ref 底层卷设备引用（如 /dev/mapper/<vg>-<lv>）；目录模式为空。
	Ref string `json:"ref"`
	// SizeBytes 分配容量（字节）；目录模式为 0。
	SizeBytes int64 `json:"size_bytes"`
}

func toStorageDTO(v app.StorageView) storageDTO {
	dto := storageDTO{
		ID: v.Storage.ID, Name: v.Storage.Name, Path: v.Storage.Path, Enabled: v.Storage.Enabled,
		CreatedAt: v.Storage.CreatedAt, UpdatedAt: v.Storage.UpdatedAt,
		FileSystem: v.FileSystem, FreeBytes: v.FreeBytes, TotalBytes: v.TotalBytes,
		VolumeName: v.VolumeName, DiskCount: v.DiskCount, UsedBytes: v.UsedBytes,
		Mounted: v.Mounted,
	}
	if v.Volume != nil {
		dto.Kind = v.Volume.Kind
		dto.Ref = v.Volume.Ref
		dto.SizeBytes = v.Volume.SizeBytes
	}
	return dto
}

func toStorageDTOs(views []app.StorageView) []storageDTO {
	out := make([]storageDTO, 0, len(views))
	for i := range views {
		out = append(out, toStorageDTO(views[i]))
	}
	return out
}

// fsRootDTO 是「源目录选择」中的一个可浏览根。
type fsRootDTO struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Exists 该根当前是否真实存在（首次建盘前尚未创建为 false）。
	Exists bool `json:"exists"`
}

// fsEntryDTO 是浏览结果中的一条子目录。
type fsEntryDTO struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	HasChildren bool   `json:"has_children"`
}

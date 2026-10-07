// Package domain 定义业务实体、值对象与状态机。
//
// 分层约定（见 docs/implementation.md 6.2）：
//   - 本包不依赖任何 Windows API、DB 驱动或 HTTP 框架；
//   - 状态迁移规则集中在 statemachine.go，业务层不得绕过。
package domain

import (
	"fmt"
	"time"
)

// Role 用户角色。
type Role string

const (
	RoleSuperAdmin Role = "super_admin"
	RoleUser       Role = "user"
)

// Valid 校验角色合法性。
func (r Role) Valid() bool {
	return r == RoleSuperAdmin || r == RoleUser
}

// RepoMode 存储库模式。共享与独享互斥（见 docs/implementation.md 3.2）。
type RepoMode string

const (
	// RepoModeShared 共享模式：母盘 + 差异盘，可分配给多个用户。
	RepoModeShared RepoMode = "shared"
	// RepoModeExclusive 独享模式：单 VHDX + 单 iSCSI 目标。
	RepoModeExclusive RepoMode = "exclusive"
)

// Valid 校验模式合法性。
func (m RepoMode) Valid() bool {
	return m == RepoModeShared || m == RepoModeExclusive
}

// RepoState 存储库生命周期状态。
type RepoState string

const (
	// RepoStateCreating 建库中：母盘与"共享数量"个差异盘（含 iSCSI 目标）正在异步预创建。
	//
	// 该状态下拒绝分配与挂载：池还没建好，让用户分配只会得到"没盘可用"，
	// 而挂载会退回到"边挂边等建盘"的慢路径（那正是本状态想消除的体验）。
	RepoStateCreating RepoState = "creating"
	RepoStateActive   RepoState = "active"
	RepoStateSealing  RepoState = "sealing"
	RepoStateDeleting RepoState = "deleting"
	RepoStateError    RepoState = "error"
)

// ParentCondition 母盘用途条件。
//
// 四种用途**互斥**（见 docs/implementation.md 4.3）：
// 母盘同一时刻只能处于"可派生 / 已派生 / 临时共享 / 维护更新"之一。
type ParentCondition string

const (
	// ParentIdle 无差异盘、未发布、未挂载。
	ParentIdle ParentCondition = "idle"
	// ParentDerived 存在 ≥1 个差异盘。
	ParentDerived ParentCondition = "derived"
	// ParentTempShared 以只读 iSCSI 方式临时共享给客户端。
	ParentTempShared ParentCondition = "temp_shared"
	// ParentMaintenance 管理员读写挂载，用于更新母盘内容。
	ParentMaintenance ParentCondition = "maintenance"
)

// Valid 校验取值。
func (c ParentCondition) Valid() bool {
	switch c {
	case ParentIdle, ParentDerived, ParentTempShared, ParentMaintenance:
		return true
	default:
		return false
	}
}

// DiskKind 磁盘类型。
type DiskKind string

const (
	// DiskKindParent 母盘。
	DiskKindParent DiskKind = "parent"
	// DiskKindDiff 差异盘。
	DiskKindDiff DiskKind = "diff"
	// DiskKindStandalone 独享库的单盘。
	DiskKindStandalone DiskKind = "standalone"
)

// Valid 校验取值。
func (k DiskKind) Valid() bool {
	return k == DiskKindParent || k == DiskKindDiff || k == DiskKindStandalone
}

// DiskState 磁盘状态。
type DiskState string

const (
	DiskStateCreating  DiskState = "creating"
	DiskStateReady     DiskState = "ready"
	DiskStatePublished DiskState = "published"
	DiskStateDeleting  DiskState = "deleting"
	DiskStateError     DiskState = "error"
)

// Valid 校验取值。
func (s DiskState) Valid() bool {
	switch s {
	case DiskStateCreating, DiskStateReady, DiskStatePublished, DiskStateDeleting, DiskStateError:
		return true
	default:
		return false
	}
}

// VHDType 虚拟磁盘类型（对应 VirtDisk 的三种创建方式）。
type VHDType string

const (
	VHDTypeDynamic      VHDType = "dynamic"
	VHDTypeFixed        VHDType = "fixed"
	VHDTypeDifferencing VHDType = "differencing"
)

// Valid 校验取值。
func (t VHDType) Valid() bool {
	return t == VHDTypeDynamic || t == VHDTypeFixed || t == VHDTypeDifferencing
}

// AllocationState 分配状态。
type AllocationState string

const (
	AllocationStateAllocated AllocationState = "allocated"
	AllocationStateMounting  AllocationState = "mounting"
	AllocationStateMounted   AllocationState = "mounted"
	AllocationStateReleasing AllocationState = "releasing"
	AllocationStateReleased  AllocationState = "released"
)

// LeaseState 租约状态。在线状态的**权威来源**（见 docs/implementation.md 5.7）。
type LeaseState string

const (
	LeaseStateActive   LeaseState = "active"
	LeaseStateExpired  LeaseState = "expired"
	LeaseStateRevoked  LeaseState = "revoked"
	LeaseStateReleased LeaseState = "released"
)

// IsTerminal 判断是否为终态。
func (s LeaseState) IsTerminal() bool {
	return s == LeaseStateRevoked || s == LeaseStateReleased
}

// AuthMode iSCSI 鉴权模式。
type AuthMode string

const (
	AuthModeNone AuthMode = "none"
	AuthModeIP   AuthMode = "ip"
	AuthModeCHAP AuthMode = "chap"
)

// Valid 校验取值。
func (m AuthMode) Valid() bool {
	return m == AuthModeNone || m == AuthModeIP || m == AuthModeCHAP
}

// InitiatorIDType initiator 标识类型，对应 IscsiTarget 的 `IdType:Value` 格式。
type InitiatorIDType string

const (
	InitiatorIQN         InitiatorIDType = "IQN"
	InitiatorIPAddress   InitiatorIDType = "IPAddress"
	InitiatorIPv6Address InitiatorIDType = "IPv6Address"
	InitiatorDNSName     InitiatorIDType = "DNSName"
	InitiatorMACAddress  InitiatorIDType = "MACAddress"
)

// Valid 校验取值。
func (t InitiatorIDType) Valid() bool {
	switch t {
	case InitiatorIQN, InitiatorIPAddress, InitiatorIPv6Address, InitiatorDNSName, InitiatorMACAddress:
		return true
	default:
		return false
	}
}

// InitiatorID 是一条 initiator 白名单项。
type InitiatorID struct {
	Type  InitiatorIDType `json:"type"`
	Value string          `json:"value"`
}

// String 输出 PowerShell 需要的 `IdType:Value` 形式。
func (i InitiatorID) String() string {
	return string(i.Type) + ":" + i.Value
}

// Valid 校验合法性。
func (i InitiatorID) Valid() bool {
	return i.Type.Valid() && i.Value != ""
}

// Purpose iSCSI 目标的用途。
type Purpose string

const (
	// PurposeUser 面向用户的分配目标（每分配一个目标，见 docs/implementation.md 5.3）。
	PurposeUser Purpose = "user"
	// PurposeTempParent 母盘临时只读共享目标。
	PurposeTempParent Purpose = "temp_parent"
)

// Permission 存储库成员权限等级。
type Permission string

const (
	PermRead   Permission = "read"
	PermMount  Permission = "mount"
	PermManage Permission = "manage"
)

// Valid 校验取值。
func (p Permission) Valid() bool {
	return p == PermRead || p == PermMount || p == PermManage
}

// JobType 异步任务类型。
type JobType string

const (
	JobCreateVHDX        JobType = "create_vhdx"
	JobCreateVHDXFromDir JobType = "create_vhdx_from_dir"
	JobCopyVHDX          JobType = "copy_vhdx"
	JobCreateDiff        JobType = "create_diff"
	JobPublish           JobType = "publish"
	JobUnpublish         JobType = "unpublish"
	JobReclaim           JobType = "reclaim"
	JobCompact           JobType = "compact"
	JobReconcile         JobType = "reconcile"
	JobDeleteDisk        JobType = "delete_disk"
	JobUploadComplete    JobType = "upload_complete"
	// JobPrepareRepo 建库预创建：母盘 → "共享数量"个差异盘 → 同数量的 iSCSI 目标（见 5.14）。
	//
	// 与 JobCreateVHDX 的区别：它不只建母盘，还要把整个"差异盘池"建好并发布，
	// 建完才把存储库置为 active。
	JobPrepareRepo JobType = "prepare_repo"
	// JobResetDiff 释放分配后的"回池"：原地重建该差异盘（清空上一个用户的数据）并重新发布。
	//
	// 池化存储库（见 RepoMeta.Pool）不用 JobDeleteDisk：把盘删掉池子就少一格，
	// 下一次分配又要现建（用户重新回到"挂载要等很久"）。
	JobResetDiff JobType = "reset_diff"
	// JobInstallDeps 按需安装系统依赖包（管理端"安装"按钮触发，见 app.InstallSysDeps）。
	//
	// 与其他任务的区别：它**不碰业务数据**，只调发行版包管理器装包（Linux 专有；
	// Windows 侧没有这类"进程外依赖"，提交时直接返回 platform.unsupported）。
	// RefID 与 Payload 都带依赖项 Key（如 lio_tools）——RefID 便于按引用检索，Payload 供 handler 读取。
	JobInstallDeps JobType = "install_deps"
)

// JobState 任务状态。
type JobState string

const (
	JobStatePending   JobState = "pending"
	JobStateRunning   JobState = "running"
	JobStateSucceeded JobState = "succeeded"
	JobStateFailed    JobState = "failed"
	JobStateCancelled JobState = "cancelled"
)

// IsTerminal 判断是否终态。
func (s JobState) IsTerminal() bool {
	return s == JobStateSucceeded || s == JobStateFailed || s == JobStateCancelled
}

// UnixMilli 是项目中统一的时间表示（DB 中以 INTEGER 存储，避免方言差异）。
type UnixMilli int64

// FromTime 将 time.Time 转为 UnixMilli。
func FromTime(t time.Time) UnixMilli { return UnixMilli(t.UnixMilli()) }

// Time 还原为 time.Time。
func (u UnixMilli) Time() time.Time { return time.UnixMilli(int64(u)) }

// Valid 判断是否已设置。
func (u UnixMilli) Valid() bool { return u > 0 }

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

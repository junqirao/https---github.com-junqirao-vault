// Package apperr 定义业务错误码与统一的错误包装。
//
// 设计约定（见 docs/implementation.md 6.5）：
//   - 对外只暴露稳定的错误码 Code 与插值参数 Args，绝不把底层原始报错（如 PowerShell 输出）返回客户端；
//   - HTTP 状态码由业务错误携带，避免 handler 里散落状态码判断；
//   - Cause 仅用于日志，不参与序列化。
package apperr

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
)

// Error 是业务错误。实现 error 接口，并支持 errors.Is / errors.As。
type Error struct {
	// Code 稳定标识，形如 "repo.not_found"。前端据此翻译文案（i18n）。
	Code string
	// HTTP 建议的 HTTP 状态码。
	HTTP int
	// Args 供前端插值的参数。
	Args map[string]any
	// Cause 内部根因，仅进日志。
	Cause error
}

// Error 实现 error 接口。
//
// nil 接收者安全：接口里可能装着 (*Error)(nil) 这种"typed nil"（例如某个
// `func(...) *apperr.Error` 在成功分支直接 return nil，再被当作 error 返回），
// 此时方法会被真实调用到，不兜住就是 nil 解引用 panic。
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Cause)
	}
	return e.Code
}

// Unwrap 支持 errors.Is / errors.As 沿错误链下钻。nil 接收者安全（见 Error）。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// LogValue 实现 slog.LogValuer：让 `"error", err` 这种**最常见的**日志写法自动带上
// Args 与 Cause。
//
// 为什么必须补这一步：Error() 按约定只返回稳定的 Code（Args 是留给前端 i18n 插值的），
// 于是 component / hint / root 这些**排障时唯一有用**的信息在日志里全部消失，现场只看到
// 一句光秃秃的 system.unavailable。真实后果：明明是"iscsi_target_mod 没加载、fabric 目录
// 不在"，却因为看不出组件而在代码逻辑里反复找原因（曾据此误判为"iSCSI 还在自己实现"）。
//
// 为什么实现 LogValuer 而不改 Error()：Error() 的字符串可能被写进任务结果/接口响应下发到
// 客户端，而 Args 里含服务端路径；**日志要全、对外要收敛**，两者不能共用一条渲染路径。
//
// 无需改动任何调用点：slog 的 Value.Kind() 会优先识别 LogValuer（见 log/slog value.go
// 的 `case LogValuer: return KindLogValuer`），故既有 `slog.X(..., "error", err)` 即刻生效。
// nil 接收者安全（与 Error/Unwrap 一致）。JSON 输出形如
// "error":{"code":"system.unavailable","component":"iscsi_fabric","hint":"..."}。
func (e *Error) LogValue() slog.Value {
	if e == nil {
		return slog.StringValue("")
	}
	attrs := make([]slog.Attr, 0, len(e.Args)+2)
	attrs = append(attrs, slog.String("code", e.Code))
	// 参数名排序：同一错误的日志字段顺序稳定，便于 diff 与按字段检索。
	for _, k := range e.argKeys() {
		attrs = append(attrs, slog.Any(k, e.Args[k]))
	}
	if e.Cause != nil {
		attrs = append(attrs, slog.String("cause", e.Cause.Error()))
	}
	return slog.GroupValue(attrs...)
}

// argKeys 返回排序后的插值参数名。
func (e *Error) argKeys() []string {
	keys := make([]string, 0, len(e.Args))
	for k := range e.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// WithArg 追加插值参数，返回自身便于链式调用。
func (e *Error) WithArg(k string, v any) *Error {
	if e.Args == nil {
		e.Args = make(map[string]any, 2)
	}
	e.Args[k] = v
	return e
}

// WithCause 设置根因，返回自身便于链式调用。
func (e *Error) WithCause(err error) *Error {
	e.Cause = err
	return e
}

// New 构造一个业务错误。
func New(code string, httpStatus int) *Error {
	return &Error{Code: code, HTTP: httpStatus}
}

// Wrap 在已有错误上叠加业务语义。若 err 已是 *Error 则原样返回，避免层层包裹丢失原始码。
func Wrap(err error, code string, httpStatus int) *Error {
	var existed *Error
	if errors.As(err, &existed) {
		return existed
	}
	return &Error{Code: code, HTTP: httpStatus, Cause: err}
}

// As 提取业务错误；若不是业务错误则返回 nil, false。
//
// 对"typed nil"（接口非 nil 但 *Error 为 nil）也返回 nil, false：
// 否则调用方拿到 nil 之后去读 e.Code / e.Args 会 panic，且"假错误"会被当成业务错误处理。
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		if e == nil {
			return nil, false
		}
		return e, true
	}
	return nil, false
}

// HTTPStatus 返回应使用的 HTTP 状态码；非业务错误一律 500。
func HTTPStatus(err error) int {
	if e, ok := As(err); ok {
		return e.HTTP
	}
	return http.StatusInternalServerError
}

// CodeOf 返回错误码；非业务错误返回统一的内部错误码。
func CodeOf(err error) string {
	if e, ok := As(err); ok {
		return e.Code
	}
	return CodeInternal
}

// 通用错误码。
const (
	CodeInternal     = "system.internal"
	CodeInvalidParam = "system.invalid_param"
	CodeNotFound     = "system.not_found"
	CodeConflict     = "system.conflict"
	CodeUnavailable  = "system.unavailable"
)

// 常用构造器（按域分组，新增错误码集中在此便于审查与前端对齐）。

// ---- auth ----

func AuthRequired() *Error  { return New("auth.required", http.StatusUnauthorized) }
func AuthForbidden() *Error { return New("auth.forbidden", http.StatusForbidden) }
func AuthLocked() *Error    { return New("auth.locked", http.StatusTooManyRequests) }

// CertNotForThisServer 客户端证书不属于本服务端（见 9.1 约束 3）。
func CertNotForThisServer() *Error {
	return New("auth.cert_not_for_this_server", http.StatusForbidden)
}

// CertCAChanged 服务端 CA 指纹与首次信任不一致，必须人工重新确认（见 9.1、R23）。
func CertCAChanged() *Error {
	return New("auth.ca_changed", http.StatusForbidden)
}

// ---- system ----

// IncompatibleProtocol 协议版本不匹配。属于硬性不可用，与"可关闭的版本区间"不同。
func IncompatibleProtocol(serverAPI, clientMax int) *Error {
	return New("system.incompatible_protocol", http.StatusUpgradeRequired).
		WithArg("server_api_version", serverAPI).
		WithArg("client_max_api_version", clientMax)
}

func InvalidParam(field string) *Error {
	return New(CodeInvalidParam, http.StatusBadRequest).WithArg("field", field)
}

// ---- system：孤儿磁盘文件（对账发现、管理端手动清理，见 app/orphan.go）----

// OrphanPathInvalid 传入的路径不在任何生效存储根的 disks 目录下（或不是 .vhdx 文件）。
//
// 删除接口是本系统里唯一"按用户给的路径直接删文件"的接口，参数校验即最后一道防线，
// 因此比普通参数校验更严（绝对路径 + .vhdx + 必须落在根白名单的 disks_dir 之下）。
func OrphanPathInvalid() *Error {
	return New("system.orphan_path_invalid", http.StatusBadRequest)
}

// OrphanRegistered 该文件已被数据库登记为磁盘，不能当作孤儿删除。
//
// 页面上的清单是扫描那一刻的快照，点删除前必须重新确认：期间文件可能已被建库/派生
// 重新用上。宁可报错，也不能删掉真实在用的盘。
func OrphanRegistered() *Error {
	return New("system.orphan_registered", http.StatusConflict)
}

// OrphanNotFound 孤儿文件已不存在（可能已被删除，或已被对账移入孤儿目录）。
func OrphanNotFound() *Error {
	return New("system.orphan_not_found", http.StatusNotFound)
}

// OrphanDeleteFailed 删除失败（最常见的原因是文件正被程序占用）。
func OrphanDeleteFailed() *Error {
	return New("system.orphan_delete_failed", http.StatusInternalServerError)
}

// ---- platform ----

// PlatformUnsupported 当前平台后端不具备该能力（如 Windows 上的 LVM 池管理）。
func PlatformUnsupported() *Error {
	return New("platform.unsupported", http.StatusNotImplemented)
}

// PoolSizeExceeded 申请的池容量超过卷组当前可用空间。
//
// 必须在调用 lvcreate **之前**拦住：容量超限时 LVM 不但会失败，还会抛出
// "Do you really want to remove and DISCARD logical volume <vg>/lvol0_pmspare?" 这种
// 会破坏数据的交互式提问（真实反馈：真机上就是这么失败的）。服务进程没有 tty，
// 提问默认取 n、命令以 exit 5 收场，用户只看到一句无从下手的英文；
// 而一旦有人在命令里补上 -y，pmspare 卷就真被抹了。
func PoolSizeExceeded(freeBytes, requestedBytes int64) *Error {
	return New("platform.pool_size_exceeded", http.StatusBadRequest).
		WithArg("free_bytes", freeBytes).
		WithArg("requested_bytes", requestedBytes)
}

// CacheDeviceOverlap 同一块设备被同时指定为"容量盘"与"缓存盘"。
//
// 两者最终都要落进同一个卷组：一块 PV 不可能既承载 thin pool 又承载 cache pool。
// 真走到 LVM 只会得到一句看不懂的报错（甚至可能把已经建好的池搅乱），
// 因此在校验阶段就拒绝。前端已把两者做成互斥勾选，这里兜住直连 API 的情况。
func CacheDeviceOverlap(device string) *Error {
	return New("platform.cache_device_overlap", http.StatusBadRequest).WithArg("device", device)
}

// ---- platform：存储池运维（删除池/卷组，见 platform/linuxlvm/pooldelete.go）----

// PoolInUse 池或卷组里还有内容，禁止删除。
//
// reason 取值（前端据此选用哪条文案）：
//   - "thin_volumes"：池里还有 thin 卷——存储的底层卷、母盘、差异盘都住在这个池里；
//   - "volume_group"：要连卷组一起删，但卷组里还有别的逻辑卷；
//   - "storages"    ：应用层查库发现该池上还挂着存储记录（先删存储，再删池）。
//
// 与 StorageInUse 同一思路：把"还剩几个、是谁"作为参数带给前端。只回一句"池被占用"，
// 用户只能自己去敲 LVM 命令行找原因，这正是本次要消灭的体验。
func PoolInUse(reason string) *Error {
	return New("platform.pool_in_use", http.StatusConflict).WithArg("reason", reason)
}

// PoolProtected 不允许删除的池：服务端配置的默认池，或物理卷上带着系统挂载点的卷组。
//
// 默认池（platform.lvm.vg/thin_pool）是服务端所有建库/建存储的落脚点，删掉它服务随即不可用，
// 因此不论前端怎么点都拒绝——真要删得先改配置、重启后再来。
// reason 取值："default_pool" | "system_vg"。
func PoolProtected(reason string) *Error {
	return New("platform.pool_protected", http.StatusConflict).WithArg("reason", reason)
}

// PoolNotFound 池或卷组不存在（可能已被删除，或已被别人先一步处理掉）。
func PoolNotFound(vg, thinPool string) *Error {
	return New("platform.pool_not_found", http.StatusNotFound).
		WithArg("vg", vg).
		WithArg("thin_pool", thinPool)
}

// ---- platform：系统依赖按需安装（管理端"安装"按钮，见 platform/sysdeps/install.go）----

// SysDepsNotInstallable 该依赖项不支持按需安装。
//
// 内核模块、configfs 挂载这类缺项不是"装个包"能解决的，必须按 Hint 里的命令人工处理；
// 用 501 而不是 400：请求本身合法，是服务端没有这项能力。
func SysDepsNotInstallable(key string) *Error {
	return New("platform.sysdeps_not_installable", http.StatusNotImplemented).WithArg("key", key)
}

// SysDepsInstallDisabled 服务端配置明确关闭了装包（platform.auto_install=false）。
//
// 刻意**不**因为"这是用户点按钮"就绕过开关：该开关的语义正是"别让服务端动这台机器的包管理"，
// 通常意味着依赖由 Ansible/镜像构建等外部工具接管，或机器根本不通外网——
// 此时调包管理器只会等满超时再失败，徒增困惑。
func SysDepsInstallDisabled() *Error {
	return New("platform.sysdeps_install_disabled", http.StatusConflict)
}

// SysDepsInstallNotRoot 服务端不是以 root 运行，无法调用发行版包管理器。
func SysDepsInstallNotRoot() *Error {
	return New("platform.sysdeps_install_not_root", http.StatusConflict)
}

// SysDepsNoPackageManager 宿主机没有受支持的包管理器，无法自动安装。
func SysDepsNoPackageManager() *Error {
	return New("platform.sysdeps_no_pkg_manager", http.StatusConflict)
}

// SysDepsInstallFailed 安装命令执行失败（软件源不可达、包名与发行版不匹配等）。
//
// 与上面几个的区别：那些是**提交前**就能确定的拒绝理由，这个是命令真的跑过并失败了。
// 底层输出只进日志，客户端只拿到这个码（见 6.5）。
func SysDepsInstallFailed() *Error {
	return New("platform.sysdeps_install_failed", http.StatusInternalServerError)
}

// ---- repo ----

func RepoNotFound() *Error  { return New("repo.not_found", http.StatusNotFound) }
func RepoNameTaken() *Error { return New("repo.name_taken", http.StatusConflict) }

// RepoParentConditionViolation 母盘状态不满足操作前提（见 4.3 状态机）。
func RepoParentConditionViolation(current, expected string) *Error {
	return New("repo.parent_condition_violation", http.StatusConflict).
		WithArg("current", current).
		WithArg("expected", expected)
}

// RepoDiffLimitExceeded 差异盘数量达到 max_diff_disks 上限。
func RepoDiffLimitExceeded(limit int) *Error {
	return New("repo.diff_limit_exceeded", http.StatusConflict).WithArg("limit", limit)
}

// RepoCreating 存储库仍在建库中（母盘与"共享数量"个差异盘正在异步预创建）。
//
// 与 repo.state_invalid 分开：那是一个笼统的"状态不对"，而这里必须让用户看懂
// "库还没建好，等它建完就能分配/挂载了"，前端也好据此把按钮置灰并显示当前步骤。
func RepoCreating() *Error { return New("repo.creating", http.StatusConflict) }

// RepoHasActiveLease 仍有客户端在线，禁止进入维护/清理。
func RepoHasActiveLease(count int) *Error {
	return New("repo.has_active_lease", http.StatusConflict).WithArg("count", count)
}

// RepoQuotaExceeded 配额不足。
func RepoQuotaExceeded(quotaBytes, usedBytes, needBytes int64) *Error {
	return New("repo.quota_exceeded", http.StatusConflict).
		WithArg("quota_bytes", quotaBytes).
		WithArg("used_bytes", usedBytes).
		WithArg("need_bytes", needBytes)
}

// ---- disk ----

func DiskNotFound() *Error { return New("disk.not_found", http.StatusNotFound) }
func DiskBusy() *Error     { return New("disk.busy", http.StatusConflict) }

// DiskNotReady 磁盘仍在创建中（差异盘由分配异步派生），此刻还不能发布/挂载。
//
// 这是**可重试**的临时状态（通常几百毫秒后即可），因此用 409 而不是 500：
// 早期把它当成脚本错误返回 platform.ps_failed 500，用户看到的是"操作失败"，
// 而实际上重试一次就好了。
func DiskNotReady() *Error { return New("disk.not_ready", http.StatusConflict) }

// DiskCreateFailed 磁盘创建已彻底失败（异步建盘任务重试用尽，磁盘状态为 error）。
//
// 与 DiskNotReady 的区别：后者是"再等等就好"的临时态，本错误是确定结论。
// 必须分开，否则建盘失败后用户会被"稍后重试"一直误导，反复点挂载也永远不会成功。
func DiskCreateFailed() *Error { return New("disk.create_failed", http.StatusInternalServerError) }

// DiskNotParent 该磁盘不是母盘，不支持内容下载（409）。
func DiskNotParent() *Error { return New("disk.not_parent", http.StatusConflict) }

// DiskParentFingerprintMismatch 母盘内容被绕过修改，差异盘已不可信（见 5.4、R2）。
func DiskParentFingerprintMismatch() *Error {
	return New("disk.parent_fingerprint_mismatch", http.StatusConflict)
}

// DiskParentVersionMismatch 差异盘基于的母盘版本与当前不一致（见 5.4）。
func DiskParentVersionMismatch(childVersion, currentVersion int) *Error {
	return New("disk.parent_version_mismatch", http.StatusConflict).
		WithArg("child_parent_version", childVersion).
		WithArg("current_parent_version", currentVersion)
}

// ---- iscsi ----

func IscsiTargetNotFound() *Error { return New("iscsi.target_not_found", http.StatusNotFound) }

// ---- lease ----

func LeaseNotFound() *Error { return New("lease.not_found", http.StatusNotFound) }
func LeaseRevoked() *Error  { return New("lease.revoked", http.StatusForbidden) }

// ---- upload ----

func UploadNotFound() *Error { return New("upload.not_found", http.StatusNotFound) }

// ---- storage ----

// StorageNotFound 存储不存在。
func StorageNotFound() *Error { return New("storage.not_found", http.StatusNotFound) }

// StorageDisabled 存储已停用，不可用于新建磁盘。
func StorageDisabled() *Error { return New("storage.disabled", http.StatusConflict) }

// StorageInUse 存储下仍有磁盘，禁止删除（只删记录、绝不删文件）。
func StorageInUse(diskCount int) *Error {
	return New("storage.in_use", http.StatusConflict).WithArg("disk_count", diskCount)
}

// StorageNameTaken 存储名称已存在（大小写不敏感）。
func StorageNameTaken() *Error { return New("storage.name_taken", http.StatusConflict) }

// StoragePathTaken 存储路径已存在（大小写不敏感）。
func StoragePathTaken() *Error { return New("storage.path_taken", http.StatusConflict) }

// StorageVolumeNotFound 存储未登记底层卷（目录模式）。对需要卷的操作（卸载/扩容）返回。
func StorageVolumeNotFound() *Error { return New("storage.volume_not_found", http.StatusNotFound) }

// StorageNotMounted 存储的底层卷当前未挂载，不可作为落盘位置。
func StorageNotMounted() *Error { return New("storage.not_mounted", http.StatusConflict) }

// StorageVolumeBusy 存储仍有进行中的上传/任务，禁止卸载或删除底层卷。
func StorageVolumeBusy() *Error { return New("storage.volume_busy", http.StatusConflict) }

// StorageVolumeFailed 底层卷操作失败（建卷/格式化/挂载/扩容/删除）。
func StorageVolumeFailed(reason string) *Error {
	return New("storage.volume_failed", http.StatusInternalServerError).WithArg("reason", reason)
}

// ---- job ----

func JobNotFound() *Error { return New("job.not_found", http.StatusNotFound) }

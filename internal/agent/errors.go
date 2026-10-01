// Package agent 实现 Vault-Agent：客户端本地代理。
//
// 职责（见 docs/agent-api.md）：
//   - 只监听 127.0.0.1，所有请求必须携带 X-Vault-Agent-Token；
//   - 消费前端推送的服务端会话，代其调用服务端 REST API；
//   - 执行 iSCSI 连接 / 磁盘上线 / 盘符或目录挂载 / 卸载的完整管道；
//   - 维持租约心跳，接收服务端「踢下线」指令并主动卸载；
//   - 通过本地 SSE 把挂载状态、服务端连通性、更新状态推给前端。
//
// 安全约束：
//   - CHAP 密钥只在内存中保留到卸载，绝不落盘、绝不写日志；
//   - 服务端会话令牌同样只在内存中保留；
//   - 对外响应只暴露稳定的错误码与插值参数，底层原始报错只进日志。
package agent

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// maxMountErrorDetailRunes 是挂载失败详情（原始报错）保留的最大字符数。
//
// 状态会经 SSE 推送、并写进本地状态文件，必须截断，避免异常文本把状态撑爆。
const maxMountErrorDetailRunes = 400

// mountErrorDetailOf 提取"挂载失败的原始报错细节"，供**本机诊断展示**。
//
// 去处有两处（都是**本机、令牌校验过**的信任边界，与"原始报错不进服务端公开 API"不冲突）：
//   - 挂载状态的 last_error_detail（保留记录时）；
//   - 挂载失败错误响应的 args.detail（失败记录被抹掉后，这是用户界面上唯一能看到的真因）。
//
// ⚠️ 仍然严格受限：
//   - winps.ScriptError.Message 本身不参与 Error() 文本，只能像这里一样显式取用，
//     所以常规错误码 / args / 响应文本都不受影响；
//   - 只取 PowerShell 的 message 与 reason@step，**不含命令行里的 -ChapSecret**
//     （脚本的 catch 分支也不会回显密钥）；
//   - 截断到 maxMountErrorDetailRunes，防止异常文本把状态撑爆。
//
// 为什么值得破例：挂载失败在界面上只显示稳定码（agent.mount_failed + stage=connect），
// 用户完全无从知道原因（真实工单："根本没法定位错误 / 什么都看不到"）。
func mountErrorDetailOf(err error) string {
	var scriptErr *winps.ScriptError
	if !errors.As(err, &scriptErr) {
		return ""
	}
	detail := strings.TrimSpace(scriptErr.Message)
	label := strings.TrimSpace(scriptErr.Reason)
	if label != "" && strings.TrimSpace(scriptErr.Step) != "" {
		label = label + "@" + strings.TrimSpace(scriptErr.Step)
	}
	switch {
	case label != "" && detail != "":
		detail = label + ": " + detail
	case detail == "":
		detail = label
	}
	if runes := []rune(detail); len(runes) > maxMountErrorDetailRunes {
		detail = string(runes[:maxMountErrorDetailRunes]) + "…"
	}
	return detail
}

// 本地代理错误码（见 docs/agent-api.md「错误约定」）。
const (
	// CodeTokenInvalid 本地令牌缺失或不匹配（401）。
	CodeTokenInvalid = "agent.token_invalid"
	// CodeNoSession 尚未推送服务端会话（409）。
	CodeNoSession = "agent.no_session"
	// CodeAdminRequired 代理未以管理员权限运行（403）。
	CodeAdminRequired = "agent.admin_required"
	// CodeMountFailed 挂载失败（500，args.stage 指明阶段）。
	CodeMountFailed = "agent.mount_failed"
	// CodeUnmountFailed 卸载失败（500）。
	CodeUnmountFailed = "agent.unmount_failed"
	// CodeBusyMounts 有活跃挂载，操作被拒（409）。
	CodeBusyMounts = "agent.busy_mounts"
	// CodeNotMounted 指定分配当前未挂载（404）。
	CodeNotMounted = "agent.not_mounted"
	// CodeServerUnreachable 无法连接服务端（502）。
	CodeServerUnreachable = "agent.server_unreachable"
	// CodeServerTimeout 服务端在超时时间内没有响应（504，args.timeout_seconds 指明超时）。
	//
	// 与 CodeServerUnreachable 区分开的原因：挂载申请等服务端同步重活在网络完全正常时
	// 也会超时，笼统报"无法连接服务端"会把排查引向错误的地址/网络方向（真实工单）。
	CodeServerTimeout = "agent.server_timeout"
	// CodeTLSRequired 服务端启用了 TLS：明文 http 被拒（HTTP 400）且 https 也失败（502）。
	CodeTLSRequired = "agent.tls_required"
	// CodeBadResponse 已连上服务端，但响应不是预期的 JSON（例如连到了别的 Web 服务）（502）。
	CodeBadResponse = "agent.bad_response"
	// CodeUpdateInProgress 已有更新流程在执行（409）。
	CodeUpdateInProgress = "update.in_progress"
	// CodeUpdateNotAvailable 当前没有可用的更新（409）。
	CodeUpdateNotAvailable = "update.not_available"
	// CodeUpdateFailed 更新流程本身失败（500，args.stage 指明阶段）。
	CodeUpdateFailed = "update.failed"
	// CodeDigestMismatch 下载产物与清单声明的 sha256/大小不一致（422）。
	CodeDigestMismatch = "update.digest_mismatch"
	// CodeUpdaterMissing 未找到或无法启动 updater 子进程（503）。
	CodeUpdaterMissing = "update.updater_missing"
	// CodeWebUpdateFailed 渲染层资源（client_web）热更失败（500，args.stage 指明阶段）。
	CodeWebUpdateFailed = "agent.web_update_failed"
	// CodeIdentityNotInstalled 本地尚未安装客户端身份（identity.json）（409）。
	CodeIdentityNotInstalled = "agent.identity_not_installed"
	// CodeIdentityInvalid 本地身份文件不可用或密钥对不匹配（409）。
	CodeIdentityInvalid = "agent.identity_invalid"
	// CodeServerCertUnknown 缺少可固定校验的服务端证书指纹，无法安全登录（502）。
	CodeServerCertUnknown = "agent.server_cert_unknown"
	// CodeDownloadFailed 母盘下载失败（500，args.stage 指明阶段）。
	CodeDownloadFailed = "agent.download_failed"
	// CodeInsufficientLocalSpace 目标卷可用空间不足以放下母盘（507，args.need_bytes / args.free_bytes）。
	CodeInsufficientLocalSpace = "agent.insufficient_local_space"
	// CodeScanFailed 本地目录扫描失败（500，args.stage 指明阶段）。
	CodeScanFailed = "agent.scan_failed"
	// CodeScanEmpty 本地目录为空（没有可上传的常规文件；服务端不接受空 manifest）（409）。
	CodeScanEmpty = "agent.scan_empty"
	// CodeScanTooLarge 本地目录超出扫描上限（文件数 / 总字节）（413，args 携带上限与实测值）。
	CodeScanTooLarge = "agent.scan_too_large"
	// CodeUploadFailed 本地目录分块上传/建库失败（500，args.stage 指明阶段）。
	CodeUploadFailed = "agent.upload_failed"
)

// errTokenInvalid 本地令牌校验失败。
func errTokenInvalid() *apperr.Error { return apperr.New(CodeTokenInvalid, http.StatusUnauthorized) }

// errNoSession 尚未推送服务端会话。
func errNoSession() *apperr.Error { return apperr.New(CodeNoSession, http.StatusConflict) }

// errAdminRequired 代理未以管理员权限运行。
func errAdminRequired() *apperr.Error { return apperr.New(CodeAdminRequired, http.StatusForbidden) }

// errNotMounted 指定分配当前未挂载。
func errNotMounted(allocationID string) *apperr.Error {
	return apperr.New(CodeNotMounted, http.StatusNotFound).WithArg("allocation_id", allocationID)
}

// errBusyMounts 有活跃挂载，操作被拒。
func errBusyMounts(count int) *apperr.Error {
	return apperr.New(CodeBusyMounts, http.StatusConflict).WithArg("count", count)
}

// errMountFailed 挂载失败，stage 指明失败阶段。
func errMountFailed(stage string, cause error) *apperr.Error {
	return apperr.New(CodeMountFailed, http.StatusInternalServerError).
		WithArg("stage", stage).
		WithCause(cause)
}

// errMountFailedWith 在 errMountFailed 基础上附加**诊断参数**（门户地址、目标 IQN、
// TCP 可达性等）。
//
// 为什么需要：只回一个 args.stage=connect，用户与支持人员都无从判断到底是网络/防火墙、
// 目标名写错、还是 CHAP 不对（真实反馈："根本没法定位错误"）。
//
// ⚠️ 只放非敏感信息：CHAP 密钥**绝不**进入参数或错误文本（见 mountRuntime 的说明）。
func errMountFailedWith(stage string, cause error, extra map[string]any) *apperr.Error {
	err := errMountFailed(stage, cause)
	for key, value := range extra {
		if strings.TrimSpace(key) == "" || value == nil {
			continue
		}
		// 空白字符串一律不写：args 里出现 portal="" 只会让界面显示成空壳。
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		err = err.WithArg(key, value)
	}
	return err
}

// errUnmountFailed 卸载失败；stage 指明失败的阶段。
//
// 为什么要带 stage：卸载失败只有一句"卸载失败"时，用户看到的是"盘符没了、状态还写着已挂载、
// iSCSI 里会话还在"，完全无从判断卡在哪一步（真实反馈）。阶段名会随 args 一起回到界面。
//
// 统一卸载语义（见 mountEngine.unmount）后，唯一还会失败的是 mount_point 阶段 ——
// 盘符/目录仍属于该卷，Windows 认为卷正被占用，界面据此提示"关掉占用它的程序后重试"。
func errUnmountFailed(stage string, cause error) *apperr.Error {
	err := apperr.New(CodeUnmountFailed, http.StatusInternalServerError).WithCause(cause)
	if strings.TrimSpace(stage) != "" {
		err = err.WithArg("stage", stage)
	}
	return err
}

// errServerUnreachable 无法连接服务端。
func errServerUnreachable(cause error) *apperr.Error {
	return apperr.New(CodeServerUnreachable, http.StatusBadGateway).WithCause(cause)
}

// errServerTimeout 服务端未在超时时间内响应。
//
// timeout 填入 args.timeout_seconds，便于界面与排障直接看到"等了多久"。
func errServerTimeout(cause error, timeout time.Duration) *apperr.Error {
	e := apperr.New(CodeServerTimeout, http.StatusGatewayTimeout).WithCause(cause)
	if timeout > 0 {
		e = e.WithArg("timeout_seconds", int(timeout.Seconds()))
	}
	return e
}

// errTLSRequired 服务端启用了 TLS：应改用 https:// 访问。
func errTLSRequired(cause error) *apperr.Error {
	return apperr.New(CodeTLSRequired, http.StatusBadGateway).WithCause(cause)
}

// errBadResponse 响应不是预期的服务端 JSON。
func errBadResponse(cause error) *apperr.Error {
	return apperr.New(CodeBadResponse, http.StatusBadGateway).WithCause(cause)
}

// errIdentityNotInstalled 本地尚未安装客户端身份。
func errIdentityNotInstalled() *apperr.Error {
	return apperr.New(CodeIdentityNotInstalled, http.StatusConflict)
}

// errServerCertUnknown 缺少服务端证书指纹，无法固定校验。
func errServerCertUnknown() *apperr.Error {
	return apperr.New(CodeServerCertUnknown, http.StatusBadGateway)
}

// errDownloadFailed 母盘下载失败，stage 指明失败阶段。
func errDownloadFailed(stage string, cause error) *apperr.Error {
	return apperr.New(CodeDownloadFailed, http.StatusInternalServerError).
		WithArg("stage", stage).
		WithCause(cause)
}

// errWebUpdateFailed 渲染层资源热更失败，stage 指明失败阶段。
//
// 仅暴露稳定错误码与 stage；底层原因只进日志（绝不回显内容/路径细节）。
func errWebUpdateFailed(stage string, cause error) *apperr.Error {
	e := apperr.New(CodeWebUpdateFailed, http.StatusInternalServerError).WithArg("stage", stage)
	if cause != nil {
		e = e.WithCause(cause)
	}
	return e
}

// errInsufficientLocalSpace 目标卷可用空间不足，无法开始/继续写盘。
//
// free 为探测到的当前可用字节（探测失败时为 0）；need 为下载所需字节（含预留）。
func errInsufficientLocalSpace(need, free int64) *apperr.Error {
	return apperr.New(CodeInsufficientLocalSpace, http.StatusInsufficientStorage).
		WithArg("need_bytes", need).
		WithArg("free_bytes", free)
}

// errScanFailed 本地目录扫描失败，stage 指明失败阶段。
func errScanFailed(stage string, cause error) *apperr.Error {
	e := apperr.New(CodeScanFailed, http.StatusInternalServerError).WithArg("stage", stage)
	if cause != nil {
		e = e.WithCause(cause)
	}
	return e
}

// errScanEmpty 本地目录为空（服务端拒绝空 manifest，无法建库）。
func errScanEmpty() *apperr.Error { return apperr.New(CodeScanEmpty, http.StatusConflict) }

// errScanTooLarge 本地目录超出扫描上限（给出上限与实测值，便于前端提示）。
func errScanTooLarge(fileCount int, totalBytes int64) *apperr.Error {
	return apperr.New(CodeScanTooLarge, http.StatusRequestEntityTooLarge).
		WithArg("file_count", fileCount).
		WithArg("total_bytes", totalBytes).
		WithArg("max_files", scanMaxFiles).
		WithArg("max_bytes", scanMaxBytes)
}

// errUploadFailed 本地目录上传/建库失败，stage 指明失败阶段。
func errUploadFailed(stage string, cause error) *apperr.Error {
	e := apperr.New(CodeUploadFailed, http.StatusInternalServerError).WithArg("stage", stage)
	if cause != nil {
		e = e.WithCause(cause)
	}
	return e
}

// describeError 生成适合写入本地状态文件的简短错误描述。
//
// 只保留「阶段 + 稳定错误码」，不回显底层原始报错（如 PowerShell 文本）。
func describeError(stage string, err error) string {
	code := apperr.CodeOf(err)
	if stage == "" {
		return code
	}
	return stage + ":" + code
}

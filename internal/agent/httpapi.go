package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"vault/internal/apperr"
)

// maxLocalBodyBytes 限制本地请求体大小。
const maxLocalBodyBytes = 1 << 20

// localMux 构造本地 HTTP 路由（全部经过令牌校验中间件）。
func (a *Agent) localMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /agent/health", a.handleHealth)
	mux.HandleFunc("GET /agent/state", a.handleState)
	mux.HandleFunc("GET /agent/log", a.handleLog)

	mux.HandleFunc("POST /agent/session", a.handleSetSession)
	mux.HandleFunc("DELETE /agent/session", a.handleClearSession)

	mux.HandleFunc("POST /agent/mount", a.handleMount)
	mux.HandleFunc("POST /agent/unmount", a.handleUnmount)
	mux.HandleFunc("POST /agent/remount", a.handleRemount)
	mux.HandleFunc("GET /agent/mounts", a.handleListMounts)

	mux.HandleFunc("GET /agent/config", a.handleGetConfig)
	mux.HandleFunc("PATCH /agent/config", a.handlePatchConfig)

	// 母盘内容下载（后台异步，见 docs/agent-api.md「磁盘内容下载」）。
	mux.HandleFunc("POST /agent/disks/download", a.handleDiskDownload)
	mux.HandleFunc("GET /agent/downloads", a.handleListDownloads)
	mux.HandleFunc("DELETE /agent/downloads/{disk_id}", a.handleCancelDownload)

	// 本地目录扫描与上传建库（后台异步，见 docs/agent-api.md「本地目录扫描与上传建库」）。
	mux.HandleFunc("POST /agent/fs/scan", a.handleFSScan)
	mux.HandleFunc("POST /agent/uploads/start", a.handleUploadStart)
	mux.HandleFunc("GET /agent/uploads", a.handleListUploads)
	mux.HandleFunc("DELETE /agent/uploads/{upload_id}", a.handleCancelUpload)

	// 客户端证书身份（免密登录）。
	mux.HandleFunc("GET /agent/identity", a.handleGetIdentity)
	mux.HandleFunc("POST /agent/identity/install", a.handleInstallIdentity)
	mux.HandleFunc("DELETE /agent/identity", a.handleDeleteIdentity)
	mux.HandleFunc("POST /agent/identity/login", a.handleIdentityLogin)

	mux.HandleFunc("GET /agent/events", a.handleEvents)

	mux.HandleFunc("POST /agent/server/test", a.handleServerTest)

	mux.HandleFunc("POST /agent/update/check", a.handleUpdateCheck)
	mux.HandleFunc("POST /agent/update/apply", a.handleUpdateApply)

	// 渲染层资源热更（client_web，见 docs/agent-api.md「客户端资源热更」）。
	mux.HandleFunc("GET /agent/update/web", a.handleWebUpdateState)
	mux.HandleFunc("POST /agent/update/web/apply", a.handleWebUpdateApply)

	mux.HandleFunc("POST /agent/shutdown", a.handleShutdown)

	return a.corsMiddleware(a.tokenMiddleware(mux))
}

// corsMiddleware 为本地代理接口补充 CORS 响应头，并直接应答预检请求。
//
// 为什么必须要有它：客户端界面（Electron 渲染进程）用 fetch 调用本代理，请求带自定义头
// `X-Vault-Agent-Token` —— 这属于"非简单请求"，浏览器会先发一个 OPTIONS 预检。
// 而预检请求**不携带**令牌头，因此必须在令牌校验**之前**处理；
// 否则预检会拿到 401/405 且缺少 CORS 头，浏览器直接拦截，
// fetch 抛出 TypeError，界面只能显示笼统的"网络错误"（排查时极具误导性）。
//
// 安全性：代理只监听 127.0.0.1，真正的访问控制由一次性令牌承担；
// 令牌由客户端主进程生成并只在本机进程间传递，网页无法读取，
// 因此这里放开 Origin 不构成额外风险（同时也不依赖 cookie，故不使用 credentials）。
func (a *Agent) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "X-Vault-Agent-Token, Content-Type")
		h.Set("Access-Control-Max-Age", "600")

		// 预检请求到此为止，不进业务路由（业务路由不注册 OPTIONS，会返回 405）。
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenMiddleware 校验 X-Vault-Agent-Token；不匹配返回 401 agent.token_invalid。
func (a *Agent) tokenMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Vault-Agent-Token")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
			a.writeError(w, errTokenInvalid())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 状态查询 ----

// handleLog 返回代理日志的**尾部**（日志模块用）。
//
// 为什么要有它：挂载失败时原始报错（如 Connect-IscsiTarget 的 .NET 异常）只进代理日志，
// 界面看不到（真实反馈："在客户端上加一个日志模块……不然什么都看不到"）。这是本机、只读、
// 令牌校验过的接口，只回最近的日志尾部（默认 256KiB）。
//
// 可选查询参数 tail=<字节数>。
func (a *Agent) handleLog(w http.ResponseWriter, r *http.Request) {
	const defaultTail = 256 << 10
	tail := int64(defaultTail)
	if v := strings.TrimSpace(r.URL.Query().Get("tail")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			tail = n
		}
	}
	resp := map[string]any{"path": "", "text": ""}
	if a.logSource != nil {
		resp["path"] = a.logSource.CurrentPath()
		if data, err := a.logSource.ReadTail(tail); err == nil {
			resp["text"] = string(data)
		} else {
			resp["error"] = apperr.CodeOf(err)
		}
	}
	a.writeJSON(w, http.StatusOK, resp)
}

// handleHealth 返回健康信息（含管理员权限与真实连接状态）。
//
// 可选查询参数 expected_version：调用方（Electron 主进程）传入自身期望的代理版本号，
// 服务端据此计算 version_mismatch，用于向界面提示"代理版本与客户端不一致"
// （陈旧代理的典型特征，见 frontend/apps/client/electron/agent.ts 的说明）。
func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	server := a.store.Server()
	payload := map[string]any{
		"ok":            true,
		"agent_version": a.version,
		"platform":      runtime.GOOS,
		"admin":         a.admin,
		// 本机 iSCSI 发起端就绪状态（见 /agent/state 的 host）：Electron 主进程与
		// 渲染层都能据此在启动时给出横幅提示。
		"host": a.hostState(),
		"server": map[string]any{
			"connected":  server.Connected,
			"url":        server.URL,
			"name":       server.Name,
			"last_error": server.LastError,
		},
	}
	if expected := strings.TrimSpace(r.URL.Query().Get("expected_version")); expected != "" {
		payload["expected_version"] = expected
		payload["version_mismatch"] = !sameVersion(a.version, expected)
	}
	a.writeJSON(w, http.StatusOK, payload)
}

// handleState 返回完整本地状态。
func (a *Agent) handleState(w http.ResponseWriter, _ *http.Request) {
	server := a.store.Server()
	user := a.store.User()
	payload := map[string]any{
		"server": map[string]any{
			"url":         server.URL,
			"instance_id": server.InstanceID,
			"name":        server.Name,
			"connected":   server.Connected,
			"last_error":  server.LastError,
		},
		"user":       map[string]any{"id": user.ID, "username": user.Username},
		"mounts":     a.store.ListMounts(),
		"auto_mount": a.cfg.Get().AutoMount,
		// host 是本机就绪状态（当前只有 iSCSI 发起端）：界面据此在启动时挂横幅，
		// 而不是等挂载失败（阶段：connect）才知道 MSiSCSI 没启动。
		"host":       a.hostState(),
		"update":     a.updateInfo(),
		"web_update": a.webUpdateState(),
	}
	// 本机 127.0.0.1 本地接口：透出当前会话（含令牌），渲染进程据此在续期后更新自己的令牌。
	if sess, ok := a.store.Session(); ok {
		payload["session"] = sessionView(sess)
	}
	a.writeJSON(w, http.StatusOK, payload)
}

// ---- 会话 ----

// handleSetSession 接收前端推送的服务端会话。
func (a *Agent) handleSetSession(w http.ResponseWriter, r *http.Request) {
	var in Session
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, err)
		return
	}
	if strings.TrimSpace(in.ServerURL) == "" {
		a.writeError(w, apperr.InvalidParam("server_url"))
		return
	}
	if strings.TrimSpace(in.Token) == "" {
		a.writeError(w, apperr.InvalidParam("token"))
		return
	}
	// 校验地址与证书指纹合法性（不要求此刻可达：服务端暂时离线时仍允许推送会话）。
	if _, err := newServerClient(in.ServerURL, in.Token, in.CertSHA256, a.logger); err != nil {
		a.writeError(w, err)
		return
	}

	a.setSession(&in)
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleClearSession 退出登录：停止心跳并解除订阅（不卸载已有挂载）。
func (a *Agent) handleClearSession(w http.ResponseWriter, _ *http.Request) {
	a.store.ClearSession()
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 挂载与卸载 ----

// handleMount 挂载指定分配。
func (a *Agent) handleMount(w http.ResponseWriter, r *http.Request) {
	var req MountRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, err)
		return
	}
	// 用户主动挂载：失败即清理、不留记录（见 mountEngine.mount 的 keepRecordOnFailure）。
	ms, err := a.engine.mount(r.Context(), req, false)
	if err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"mount": ms})
}

// unmountRequest 是卸载请求。
//
// 没有 force 字段：卸载只有一种语义（见 mountEngine.unmount 的注释），
// 客户端旧版本带上的 force 会被忽略（decodeJSON 不做未知字段报错）。
type unmountRequest struct {
	AllocationID string `json:"allocation_id"`
}

// handleUnmount 卸载指定分配。
func (a *Agent) handleUnmount(w http.ResponseWriter, r *http.Request) {
	var req unmountRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, err)
		return
	}
	if err := a.engine.unmount(r.Context(), req.AllocationID); err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRemount 重新挂载指定分配。
func (a *Agent) handleRemount(w http.ResponseWriter, r *http.Request) {
	var req unmountRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, err)
		return
	}
	ms, err := a.engine.remount(r.Context(), req.AllocationID)
	if err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"mount": ms})
}

// handleListMounts 列出全部挂载。
func (a *Agent) handleListMounts(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]any{"items": a.store.ListMounts()})
}

// ---- 本地配置 ----

// handleGetConfig 返回本地配置。
func (a *Agent) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, a.cfg.Get())
}

// handlePatchConfig 部分更新本地配置。
func (a *Agent) handlePatchConfig(w http.ResponseWriter, r *http.Request) {
	var patch ConfigPatch
	if err := decodeJSON(r, &patch); err != nil {
		a.writeError(w, err)
		return
	}
	cfg, err := a.cfg.Patch(patch)
	if err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"config": cfg})
}

// ---- 本地事件流 ----

// handleEvents 本地 SSE 事件流。
func (a *Agent) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		a.writeError(w, apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError))
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id, ch := a.hub.Subscribe()
	defer a.hub.Unsubscribe(id)

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(ev.Data)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- 服务端连通性测试 ----

// handleServerTest 代前端做匿名连通性测试（规避跨域与证书问题）。
//
// 会自动处理协议：无协议默认 https；明文 http 遇到 HTTP 400 时自动升级为 https 重试，
// 并把实际生效地址、证书指纹等返回前端。
func (a *Agent) handleServerTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, err)
		return
	}
	if strings.TrimSpace(in.URL) == "" {
		a.writeError(w, apperr.InvalidParam("url"))
		return
	}
	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()
	result, err := a.probeServer(ctx, in.URL)
	if err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, result)
}

// ---- 自更新 ----

// handleUpdateCheck 检查更新。
func (a *Agent) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	result, err := a.checkUpdate(r.Context())
	if err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, result)
}

// handleUpdateApply 应用更新。
//
// 全部前置步骤（再次验签 / 下载校验 / 写 pending / 启动 updater）都在 applyUpdate 内完成，
// 因此本接口是**同步**的：返回 {started:true} 时替换流程已经就绪。
// 响应写出后再通知主程序退出（而不是在 applyUpdate 内部退出），
// 避免本地 HTTP 服务在响应尚未落盘时被 Shutdown 掐断。
func (a *Agent) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if err := a.applyUpdate(r.Context()); err != nil {
		a.writeError(w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"started": true})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	a.signalDone()
}

// ---- 渲染层资源热更 ----

// handleWebUpdateState 返回渲染层资源热更状态（页面刷新后据此恢复展示）。
func (a *Agent) handleWebUpdateState(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]any{"web_update": a.webUpdateState()})
}

// handleWebUpdateApply 启动渲染层资源热更（异步，立即返回 202）。
//
// 与自更新 apply 不同：热更**不重启进程**，因此这里不 signalDone；
// "下载 → 校验 → 解压 → 激活"全部在后台协程完成，进度经 web_update 事件推送。
func (a *Agent) handleWebUpdateApply(w http.ResponseWriter, _ *http.Request) {
	state := a.startWebUpdate()
	a.writeJSON(w, http.StatusAccepted, map[string]any{"web_update": state})
}

// ---- 退出 ----

// handleShutdown 托盘「退出」：通知主程序卸载全部挂载并退出。
//
// 真正的「先卸载再停止」由主程序完成（见 cmd/vault-agent/main.go），
// 这样无论通过界面退出还是收到系统信号，退出路径只有一条。
func (a *Agent) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	a.signalDone()
}

// ---- 辅助 ----

// writeJSON 输出 JSON 响应。
func (a *Agent) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.logger.Warn("写本地响应失败", "error", err)
	}
}

// writeError 输出统一错误体 {"error":{"code":...,"args":{}}}。
func (a *Agent) writeError(w http.ResponseWriter, err error) {
	detail := map[string]any{"code": apperr.CodeOf(err)}
	if e, ok := apperr.As(err); ok && len(e.Args) > 0 {
		detail["args"] = e.Args
	}
	a.writeJSON(w, apperr.HTTPStatus(err), map[string]any{"error": detail})
}

// decodeJSON 解析本地请求体；空体视为零值结构。
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxLocalBodyBytes))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return apperr.InvalidParam("body").WithCause(err)
	}
	return nil
}

// contextWithTimeout 基于请求上下文（r 可为 nil）派生带超时的上下文。
func contextWithTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	base := context.Background()
	if r != nil {
		base = r.Context()
	}
	return context.WithTimeout(base, timeout)
}

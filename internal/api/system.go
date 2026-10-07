package api

import (
	"io"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/version"
)

// clientCheckRequest 是匿名版本协商请求（见 3.4.2）。
type clientCheckRequest struct {
	APIVersion    int    `json:"api_version"`
	ClientVersion string `json:"client_version"`
}

// clientCheckResponse 是版本协商结果。
type clientCheckResponse struct {
	State         string               `json:"state"`
	Compatible    bool                 `json:"compatible"`
	APIVersion    int                  `json:"api_version"`
	ServerVersion string               `json:"server_version"`
	ClientCompat  app.ClientCompatInfo `json:"client_compat"`
	// Reason 不兼容时的稳定错误码（文案由客户端翻译）。
	Reason string `json:"reason,omitempty"`
}

// handleSystemInfo 返回版本契约与运行期能力探测（匿名可访问）。
func (r *Router) handleSystemInfo(w http.ResponseWriter, req *http.Request) {
	r.writeJSON(w, http.StatusOK, r.deps.App.SystemInfo(req.Context()))
}

// handleSystemDeps 返回系统依赖自检报告（前端据此显示"缺了什么"的横幅）。
//
// 只读：**不在请求里修复**。修复发生在启动期（受 platform.auto_repair / auto_install 控制）
// 与 `vault-server doctor`；这里每次都重新探测，运维补齐缺项后横幅刷新即消失。
//
// 始终返回 200：报告本身含 ok / supported 字段，缺项是"服务端部分功能不可用"这一事实的
// 描述，不是这次请求的失败——用 503 会让前端把它当成接口故障而弹错误提示。
func (r *Router) handleSystemDeps(w http.ResponseWriter, req *http.Request) {
	r.writeJSON(w, http.StatusOK, r.deps.App.SysDeps(req.Context()))
}

// handleSystemHealth 健康检查。
//
// 全部检查通过返回 200，否则返回 503（响应体始终含 ok 字段与各项结果）。
func (r *Router) handleSystemHealth(w http.ResponseWriter, req *http.Request) {
	status := r.deps.App.Health(req.Context())
	code := http.StatusOK
	if !status.OK {
		code = http.StatusServiceUnavailable
	}
	r.writeJSON(w, code, status)
}

// handleClientCheck 匿名版本兼容判定。
func (r *Router) handleClientCheck(w http.ResponseWriter, req *http.Request) {
	var in clientCheckRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	state, err := r.deps.App.CheckClientCompat(in.APIVersion, in.ClientVersion)
	resp := clientCheckResponse{
		State:         string(state),
		Compatible:    state == app.CompatStateOK,
		APIVersion:    version.APIVersion,
		ServerVersion: version.Version,
		ClientCompat:  r.clientCompat(),
	}
	if err != nil {
		resp.Reason = apperr.CodeOf(err)
	}
	r.writeJSON(w, http.StatusOK, resp)
}

// clientCompat 从当前配置快照读取客户端兼容区间（不触发能力探测）。
func (r *Router) clientCompat() app.ClientCompatInfo {
	out := app.ClientCompatInfo{}
	if r.deps.Cfg == nil {
		return out
	}
	loaded := r.deps.Cfg.Current()
	if loaded == nil {
		return out
	}
	out.Enabled = loaded.Compat.Enabled
	if loaded.Compat.Min != nil {
		out.Min = loaded.Compat.Min.String()
	}
	if loaded.Compat.Max != nil {
		out.Max = loaded.Compat.Max.String()
	}
	return out
}

// settingsResponse 是运行时设置视图。
//
// client_compat 为**只读展示**，真源在 config.yaml（见 3.4.2 约束 2）。
type settingsResponse struct {
	Server       settingsServer       `json:"server"`
	ClientCompat app.ClientCompatInfo `json:"client_compat"`
	Storage      settingsStorage      `json:"storage"`
	Overrides    map[string]string    `json:"overrides,omitempty"`
}

type settingsServer struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
}

type settingsStorage struct {
	WhitelistRoot  string   `json:"whitelist_root"`
	WhitelistRoots []string `json:"whitelist_roots"`
}

// handleGetSettings 返回可展示的运行期设置。
func (r *Router) handleGetSettings(w http.ResponseWriter, req *http.Request) {
	raw := r.deps.App.RawConfig()
	overrides, err := r.deps.Store.AllSettings(req.Context())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	roots := raw.Storage.EffectiveRoots()
	root := ""
	if len(roots) > 0 {
		root = roots[0]
	}
	r.writeJSON(w, http.StatusOK, settingsResponse{
		Server:       settingsServer{InstanceID: raw.Server.InstanceID, Name: raw.Server.Name},
		ClientCompat: r.clientCompat(),
		Storage:      settingsStorage{WhitelistRoot: root, WhitelistRoots: roots},
		Overrides:    overrides,
	})
}

// patchSettingsRequest 是运行期设置变更请求。
type patchSettingsRequest struct {
	ServerName *string `json:"server_name"`
	// Settings 通用键值对：仅接受白名单键（当前为 server.name）。
	Settings map[string]string `json:"settings"`
}

// allowedSettingKeys 是允许在运行期修改的键。
//
// 其余配置项以 config.yaml 为唯一真源，改动必须落文件并热重载。
var allowedSettingKeys = map[string]bool{
	"server.name": true,
}

// handlePatchSettings 修改运行时可改项。
func (r *Router) handlePatchSettings(w http.ResponseWriter, req *http.Request) {
	var in patchSettingsRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}

	updates := make(map[string]string, len(in.Settings)+1)
	for k, v := range in.Settings {
		if !allowedSettingKeys[k] {
			r.writeError(w, req, apperr.InvalidParam("settings."+k))
			return
		}
		updates[k] = v
	}
	if in.ServerName != nil {
		updates["server.name"] = *in.ServerName
	}
	if len(updates) == 0 {
		r.writeError(w, req, apperr.InvalidParam("settings"))
		return
	}

	for k, v := range updates {
		if err := r.deps.Store.SetSetting(req.Context(), k, v); err != nil {
			r.writeError(w, req, err)
			return
		}
	}
	if p, err := r.principal(req); err == nil {
		for k, v := range updates {
			r.deps.App.Audit(req.Context(), p.UserID, "system.settings.patch", k, "value="+v, "ok")
		}
	}
	r.handleGetSettings(w, req)
}

// handleListAudit 查询审计记录。
func (r *Router) handleListAudit(w http.ResponseWriter, req *http.Request) {
	logs, err := r.deps.Store.ListAuditLogs(req.Context(),
		queryString(req, "user_id"), queryString(req, "action"),
		queryInt(req, "limit", 200), queryInt(req, "offset", 0))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toAuditDTOs(logs)})
}

// handleEvents SSE 事件流（任务进度、租约变更、母盘条件变化）。
//
// 说明（见 docs/implementation.md 5.7 与 6.3）：
//   - 事件通道是**进程内**的（EventHub），单实例部署下够用；不保留历史，
//     因此 Last-Event-ID 仅用于记录客户端断线位置，不做回放——权威状态以 DB 为准；
//   - 每 20s 发送一行 SSE 注释 `: ping` 作为心跳，避免中间设备因空闲掐断连接。
func (r *Router) handleEvents(w http.ResponseWriter, req *http.Request) {
	// 事件流是长连接 handler：写失败已在下方的 return 中处理；这里再兜一层 panic，
	// 保证任何异常都只终止本连接并留一条带栈的 ERROR，而不会波及服务端其它任务。
	defer func() {
		if rec := recover(); rec != nil {
			r.deps.Log.Error("SSE 事件流处理 panic，已终止该连接",
				"panic", rec, "stack", string(debug.Stack()))
		}
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		r.writeError(w, req, apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError))
		return
	}
	lastEventID := strings.TrimSpace(req.Header.Get("Last-Event-ID"))
	if lastEventID != "" {
		r.deps.Log.Info("SSE 订阅携带 Last-Event-ID（本通道不回放历史事件）",
			"last_event_id", lastEventID, "request_id", requestIDFrom(req.Context()))
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	// 提示中间代理不要缓冲事件流。
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id, ch := r.deps.Events.Subscribe()
	defer r.deps.Events.Unsubscribe(id)

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	ctx := req.Context()
	enc := newSSEEncoder(w)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			if err := enc.write(ev); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleGetReconcile 返回最近一次对账报告（仅超级管理员）。
//
// 危险操作默认只报告；报告中 AutoFixed=false 表示仅记录未执行修复（见 5.11）。
func (r *Router) handleGetReconcile(w http.ResponseWriter, req *http.Request) {
	r.writeJSON(w, http.StatusOK, r.deps.App.LastReconcileReport())
}

// handleScanOrphans 现扫一遍孤儿磁盘文件（仅超级管理员）。
//
// 与管理端「孤儿磁盘」页面一一对应：对账只报告（见 5.11），这里给出一份**可操作清单**
// （路径、所属根、分类、大小、修改时间），由管理员确认后逐个删除。刻意不复用对账报告的
// 旧快照：用户点开页面时要看到的是"此刻磁盘上真实存在什么"。
func (r *Router) handleScanOrphans(w http.ResponseWriter, req *http.Request) {
	scan, err := r.deps.App.ScanOrphanFiles(req.Context())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, scan)
}

// orphanDeleteRequest 是删除孤儿文件的请求体。
type orphanDeleteRequest struct {
	// Path 要删除的文件绝对路径（来自扫描结果，服务端会重新校验它仍在存储根的 disks 下）。
	Path string `json:"path"`
}

// handleDeleteOrphan 删除一个孤儿磁盘文件（仅超级管理员）。
//
// 不可逆，因此必须留下**谁删了哪个文件**的审计；服务端在删除前会重新确认该文件确实
// 还没有被数据库登记（页面清单可能已经过期，见 app.DeleteOrphanFile 的四道闸门）。
func (r *Router) handleDeleteOrphan(w http.ResponseWriter, req *http.Request) {
	var in orphanDeleteRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	res, err := r.deps.App.DeleteOrphanFile(req.Context(), in.Path)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if p, err := r.principal(req); err == nil {
		r.deps.App.Audit(req.Context(), p.UserID, "system.orphan_delete",
			"file:"+res.Path, "freed_bytes="+strconv.FormatInt(res.FreedBytes, 10), domain.AuditResultOK)
	}
	r.writeJSON(w, http.StatusOK, res)
}

// ---- 初始化（Bootstrap）----
//
// 架构调整说明：服务端**不再承载 Web 静态资源**，只提供 API。
// 管理界面与用户界面合并在同一个客户端应用内，因此初始化向导也由客户端呈现。
// 服务端只负责提供状态查询与一次性初始化接口。

// handleBootstrapStatus 返回系统初始化状态（匿名可访问）。
//
// 客户端连接服务端后首先调用本接口：
//   - needs_bootstrap=true  → 进入初始化向导
//   - needs_bootstrap=false → 进入登录页（blocked_reason 说明原因）
func (r *Router) handleBootstrapStatus(w http.ResponseWriter, req *http.Request) {
	st, err := r.deps.App.BootstrapStatus(req.Context())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, st)
}

// bootstrapRequest 是初始化请求体。
type bootstrapRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	ServerName string `json:"server_name"`
}

// handleBootstrap 执行一次性初始化：创建首个超级管理员并直接下发会话。
//
// 安全约束（见 app/bootstrap.go）：
//   - 仅在"未初始化"时可用，且受配置开关与时间窗口约束；
//   - 借助数据库主键约束原子抢占，避免并发抢注；
//   - 已有数据后一律返回 409，不再开放。
func (r *Router) handleBootstrap(w http.ResponseWriter, req *http.Request) {
	var body bootstrapRequest
	if err := r.decodeJSON(req, &body); err != nil {
		r.writeError(w, req, err)
		return
	}

	res, err := r.deps.App.Bootstrap(req.Context(), app.BootstrapInput{
		Username:   body.Username,
		Password:   body.Password,
		ServerName: body.ServerName,
	}, clientIP(req))
	if err != nil {
		r.writeError(w, req, err)
		return
	}

	r.writeJSON(w, http.StatusCreated, map[string]any{
		"token":      res.Token,
		"expires_at": res.ExpiresAt,
		"user": map[string]any{
			"id":       res.UserID,
			"username": res.Username,
			"role":     string(domain.RoleSuperAdmin),
		},
	})
}

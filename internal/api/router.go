package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/config"
	"vault/internal/domain"
	"vault/internal/store"
)

// Deps 是 HTTP 层的依赖集合。
type Deps struct {
	App    *app.App
	Log    *slog.Logger
	Cfg    *config.Watcher
	Store  *store.Store
	Events *EventHub
	// Logs 服务端日志只读访问（管理端「服务日志」页）；可为 nil（不提供日志查询）。
	Logs LogSource
}

// Router 是 HTTP 路由器（实现 http.Handler）。
type Router struct {
	chi.Router
	deps Deps

	globalLimiter *rateLimiter
	ipLimiter     *rateLimiter
	loginLimiter  *rateLimiter
	// uploadLimiter 用于分块上传的独立限流：比普通接口更宽松但仍有上限，
	// 避免单用户以并发分块打满磁盘或连接（见 docs/implementation.md 5.10 要点）。
	uploadLimiter *rateLimiter
}

// New 构造路由器并注册全部路由。
func New(d Deps) *Router {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Events == nil {
		d.Events = NewEventHub()
	}
	r := &Router{
		deps:          d,
		globalLimiter: newRateLimiter(200, 400),
		ipLimiter:     newRateLimiter(40, 80),
		loginLimiter:  newRateLimiter(2, 10),
		uploadLimiter: newRateLimiter(60, 300),
	}
	r.Router = chi.NewRouter()
	r.Router.Use(middlewareRequestID)
	r.Router.Use(r.middlewareRecover)
	r.Router.Use(r.middlewareAccessLog)
	r.Router.Use(r.middlewareRateLimit)
	r.routes()
	return r
}

// Events 返回事件广播器，供 main 把任务进度等推送给 SSE 订阅者。
func (r *Router) Events() *EventHub { return r.deps.Events }

func (r *Router) routes() {
	// ---- 匿名接口（认证之前必须可访问，便于排障，见 3.4.2 约束 3）----
	r.Get("/v1/system/info", r.handleSystemInfo)
	r.Get("/v1/system/health", r.handleSystemHealth)
	// 极简探针：客户端顶栏的延迟指标每 10 秒打一次（见 handleSystemPing 的说明）。
	r.Get(systemPingPath, r.handleSystemPing)
	r.Post("/v1/system/client-check", r.handleClientCheck)

	// 更新分发（匿名可访问，见 api/update.go 的取舍说明）。
	// 服务端只做 mirror：不解析、不改写、不签名。
	r.Get("/v1/system/update/manifest", r.handleUpdateManifest)
	r.Get("/v1/system/update/signature", r.handleUpdateSignature)
	r.Get("/v1/system/update/artifacts/{filename}", r.handleUpdateArtifact)

	// 初始化（匿名，但仅未初始化时可用；见 docs 架构调整：服务端不再承载 Web 静态资源）
	r.Get("/v1/system/bootstrap", r.handleBootstrapStatus)
	r.With(r.middlewareLoginRateLimit).Post("/v1/system/bootstrap", r.handleBootstrap)

	r.With(r.middlewareLoginRateLimit).Post("/v1/auth/login", r.handleLogin)
	r.With(r.middlewareLoginRateLimit).Post("/v1/auth/enroll", r.handleEnroll)
	// 证书免密登录：匿名可访问，身份完全由 TLS 客户端证书（mTLS）承担。
	r.With(r.middlewareLoginRateLimit).Post("/v1/auth/cert-login", r.handleCertLogin)

	// ---- 已认证接口 ----
	r.Group(func(g chi.Router) {
		g.Use(r.middlewareAuthn)
		g.Use(r.middlewareAudit)

		g.Post("/v1/auth/logout", r.handleLogout)
		// 任何登录用户查看自己的信息（含配额与已用）：客户端「我的存储库」要展示
		// "已用多少 / 总共可用多少"，而 /v1/users/{id} 只对超管开放。
		g.Get("/v1/auth/me", r.handleMe)
		// 任何登录用户为自己换取客户端证书（无需超管）。
		g.Post("/v1/auth/client-certificate", r.handleClientCertificate)
		g.Get("/v1/system/events", r.handleEvents)
		// 系统依赖自检（缺 configfs 挂载 / LIO 内核模块 / LVM 工具等）：
		// 任何登录用户可读——横幅要告诉所有使用者"哪些功能不可用、怎么补"，
		// 而不是只有超管能看到原因。只读：修复只在启动期与 doctor 里做。
		g.Get("/v1/system/deps", r.handleSystemDeps)
		// 按需安装依赖（当前用于 targetcli 等命令行工具）：会真的调用宿主机包管理器，
		// 因此仅超级管理员，且返回 202 + 任务对象——进度走 SSE 的 job 事件，不轮询。
		g.With(r.requireSuperAdminMW).Post("/v1/system/deps/install", r.handleInstallSystemDeps)
		g.Get("/v1/system/settings", r.requireSuperAdmin(r.handleGetSettings))
		g.Patch("/v1/system/settings", r.requireSuperAdmin(r.handlePatchSettings))
		// 全量配置表（读）与在线改配置（写回 config.yaml）：仅超级管理员。
		g.Get("/v1/system/config", r.requireSuperAdmin(r.handleGetConfig))
		g.Patch("/v1/system/config", r.requireSuperAdmin(r.handlePatchConfig))
		g.Get("/v1/system/audit", r.requireSuperAdmin(r.handleListAudit))
		// 服务端日志（管理端「服务日志」页）：读的是服务端自己的按天日志文件。
		// 仅超级管理员——日志含内部实现细节，且只有管理员才需要对服务端排障。
		g.Get("/v1/system/logs", r.requireSuperAdmin(r.handleSystemLogs))
		g.With(r.requireSuperAdminMW).Get("/v1/system/reconcile", r.handleGetReconcile)
		// 孤儿磁盘文件：对账只报告（见 5.11），这里由管理员在「孤儿磁盘」页面上逐个手动删除。
		g.With(r.requireSuperAdminMW).Get("/v1/system/orphans", r.handleScanOrphans)
		g.With(r.requireSuperAdminMW).Post("/v1/system/orphans/delete", r.handleDeleteOrphan)
		// 导入已有磁盘（管理端「存储库」页的"导入"入口）：把 disks/ 下已存在的 .vhdx
		// 原地接管成存储库（不复制、不移动），用于换机器迁移母盘。
		// 与上面的孤儿清理是**同一批文件**的两种处置：一个删除、一个接管。
		g.With(r.requireSuperAdminMW).Get("/v1/system/disks/import/scan", r.handleScanDiskImports)
		g.With(r.requireSuperAdminMW).Post("/v1/system/disks/import", r.handleImportDisks)

		// 平台存储池（LVM thin pool / dm-cache）：读取现状与块设备列表、初始化池。
		// 仅超级管理员；Windows 后端返回 501 platform.unsupported，前端据此隐藏入口。
		g.With(r.requireSuperAdminMW).Get("/v1/system/lvm", r.handleLvmStatus)
		g.With(r.requireSuperAdminMW).Get("/v1/system/block-devices", r.handleListBlockDevices)
		// 释放设备：卸载/关 swap/抹签名/移出卷组，把"用过又不再需要"的盘清回可选状态。
		// 破坏性操作，前端必须二次确认（仅超级管理员）。
		g.With(r.requireSuperAdminMW).Post("/v1/system/block-devices/release", r.handleReleaseBlockDevice)
		g.With(r.requireSuperAdminMW).Post("/v1/system/lvm/initialize", r.handleInitializePool)
		// 多存储池："列出全部池 + 卷组"（创建存储时要"选择或新建池"）、"新建一个池"。
		// initialize 与 POST /pools 是同一个动作（前者保留兼容），区别只是语义更明确的入口。
		g.With(r.requireSuperAdminMW).Get("/v1/system/pools", r.handleListPools)
		g.With(r.requireSuperAdminMW).Post("/v1/system/pools", r.handleInitializePool)
		// 新建卷组前估算可建池容量：此刻卷组还不存在，前端拿不到 pool_max_bytes，
		// 而容量口径必须与建池时的校验一致，否则"自动填好的容量"一提交就超限。
		g.With(r.requireSuperAdminMW).Post("/v1/system/pools/estimate", r.handleEstimatePoolSize)
		// 删除存储池：可选连带删除卷组并把物理卷清回"可选"。
		// 破坏性操作，前端必须二次确认；服务端另有默认池/占用/系统盘三道拦截（仅超级管理员）。
		g.With(r.requireSuperAdminMW).Post("/v1/system/pools/delete", r.handleDeletePool)

		// 用户与证书（管理员）
		g.With(r.requireSuperAdminMW).Get("/v1/users", r.handleListUsers)
		g.With(r.requireSuperAdminMW).Post("/v1/users", r.handleCreateUser)
		g.With(r.requireSuperAdminMW).Get("/v1/users/{id}", r.handleGetUser)
		g.With(r.requireSuperAdminMW).Patch("/v1/users/{id}", r.handleUpdateUser)
		g.With(r.requireSuperAdminMW).Delete("/v1/users/{id}", r.handleDeleteUser)
		g.With(r.requireSuperAdminMW).Get("/v1/users/{id}/certificates", r.handleListCertificates)
		g.With(r.requireSuperAdminMW).Post("/v1/users/{id}/certificates", r.handleCreateCertificate)
		g.With(r.requireSuperAdminMW).Delete("/v1/users/{id}/certificates/{cid}", r.handleRevokeCertificate)
		g.With(r.requireSuperAdminMW).Post("/v1/users/{id}/enrollment-token", r.handleEnrollmentToken)

		// 存储：读取对任何登录用户开放（新建存储库表单需要），写操作仅超管。
		// Linux 上存储是 thin LV（格式化后挂载为目录），因此另有 mount/unmount/resize；
		// Windows 上存储就是普通目录，这三个接口返回 501 platform.unsupported。
		g.Get("/v1/storages", r.handleListStorages)
		g.With(r.requireSuperAdminMW).Post("/v1/storages", r.handleCreateStorage)
		g.With(r.requireSuperAdminMW).Patch("/v1/storages/{id}", r.handleUpdateStorage)
		g.With(r.requireSuperAdminMW).Delete("/v1/storages/{id}", r.handleDeleteStorage)
		g.With(r.requireSuperAdminMW).Post("/v1/storages/{id}/mount", r.handleMountStorage)
		g.With(r.requireSuperAdminMW).Post("/v1/storages/{id}/unmount", r.handleUnmountStorage)
		g.With(r.requireSuperAdminMW).Post("/v1/storages/{id}/resize", r.handleResizeStorage)

		// 服务端本地目录浏览与统计（「源目录选择」；白名单见 storage.source_roots）。
		g.Get("/v1/fs/roots", r.handleFSRoots)
		g.Get("/v1/fs/browse", r.handleFSBrowse)
		g.Get("/v1/fs/stat", r.handleFSStat)

		// 存储库
		g.Get("/v1/repos", r.handleListRepos)
		g.Post("/v1/repos", r.handleCreateRepo)
		g.Get("/v1/repos/{id}", r.handleGetRepo)
		g.Patch("/v1/repos/{id}", r.handleUpdateRepo)
		g.Delete("/v1/repos/{id}", r.handleDeleteRepo)
		g.Get("/v1/repos/{id}/members", r.handleListMembers)
		g.Put("/v1/repos/{id}/members", r.handleSetMembers)
		g.Get("/v1/repos/{id}/allocations", r.handleListAllocations)
		g.Post("/v1/repos/{id}/allocations", r.handleAllocate)
		g.Post("/v1/repos/{id}/parent/actions", r.handleParentAction)
		g.Post("/v1/repos/{id}/parent/cleanup-diffs", r.handleCleanupDiffs)
		g.Post("/v1/repos/{id}/parent/resize", r.handleResizeParent)
		// 维护态下的母盘文件管理（服务端本地挂载，不再把母盘挂到客户端）。
		g.Get("/v1/repos/{id}/parent/files", r.handleListParentFiles)
		g.Get("/v1/repos/{id}/parent/files/content", r.handleDownloadParentFile)
		g.Put("/v1/repos/{id}/parent/files/content", r.handleUploadParentFile)
		g.Get("/v1/repos/{id}/parent/files/text", r.handleReadParentText)
		g.Put("/v1/repos/{id}/parent/files/text", r.handleWriteParentText)
		g.Delete("/v1/repos/{id}/parent/files", r.handleDeleteParentEntry)
		g.Post("/v1/repos/{id}/parent/files/dir", r.handleMkdirParent)
		g.Post("/v1/repos/{id}/parent/files/move", r.handleMoveParentEntry)
		g.Post("/v1/repos/{id}/parent/files/copy", r.handleCopyParentEntry)
		// 上传核对：临时差异盘 + 与客户端清单逐文件对比。
		g.Post("/v1/repos/{id}/diff", r.handleStartVerifyDiff)
		g.Get("/v1/repos/{id}/diff/progress", r.handleVerifyDiffProgress)
		g.Get("/v1/repos/{id}/diff/result", r.handleVerifyDiffResult)
		g.Post("/v1/repos/{id}/copy", r.handleCopyRepo)
		// 存储库迁移：复制全部磁盘到另一个存储后删除源文件（异步、可中止）。
		g.Post("/v1/repos/{id}/move", r.handleMoveRepo)
		g.Post("/v1/repos/{id}/move/cancel", r.handleCancelRepoMove)
		g.Get("/v1/repos/{id}/disks", r.handleListRepoDisks)
		g.Get("/v1/repos/{id}/iscsi", r.handleListRepoIscsi)

		// iSCSI 目标授权与会话路由（见 docs/implementation.md 5.3 / 5.8）
		g.Get("/v1/iscsi/targets/{id}", r.handleGetIscsiTarget)
		g.Put("/v1/iscsi/targets/{id}/authorization", r.handleReplaceAuthorization)
		g.Post("/v1/iscsi/targets/{id}/authorization", r.handleAddAuthorization)
		g.Delete("/v1/iscsi/targets/{id}/authorization", r.handleRemoveAuthorization)
		g.Put("/v1/iscsi/targets/{id}/auth", r.handleSetIscsiAuth)
		g.Post("/v1/iscsi/targets/{id}/disable", r.handleDisableIscsiTarget)

		// 分块上传（独立限流）
		g.Post("/v1/uploads", r.handleCreateUpload)
		g.Get("/v1/uploads/{id}", r.handleGetUpload)
		g.With(r.middlewareUploadRateLimit).Put("/v1/uploads/{id}/chunks/{index}", r.handlePutUploadChunk)
		g.Post("/v1/uploads/{id}/complete", r.handleCompleteUpload)
		g.Delete("/v1/uploads/{id}", r.handleAbortUpload)

		// 磁盘
		g.Get("/v1/disks/{id}", r.handleGetDisk)
		g.Delete("/v1/disks/{id}", r.handleDeleteDisk)
		g.Post("/v1/disks/{id}/compact", r.handleCompactDisk)
		g.Get("/v1/disks/{id}/content", r.handleDownloadDiskContent)

		// 分配
		g.Delete("/v1/allocations/{id}", r.handleReleaseAllocation)

		// 挂载与租约
		g.Post("/v1/allocations/{id}/mount", r.handleMount)
		g.Get("/v1/leases", r.handleListLeases)
		g.Post("/v1/leases/{id}/heartbeat", r.handleHeartbeat)
		g.Post("/v1/leases/{id}/mounted", r.handleMounted)
		g.Post("/v1/leases/{id}/release", r.handleRelease)
		g.With(r.requireSuperAdminMW).Delete("/v1/leases/{id}", r.handleRevokeLease)

		// 任务
		g.Get("/v1/jobs", r.handleListJobs)
		g.Get("/v1/jobs/{id}", r.handleGetJob)
		// 取消任务仅超管：会杀掉正在执行的平台命令并释放锁，属运维级操作。
		g.With(r.requireSuperAdminMW).Post("/v1/jobs/{id}/cancel", r.handleCancelJob)
	})

	// 架构调整：服务端**只提供 API**，不再承载任何 Web 静态资源。
	// 管理界面与用户界面已合并在同一个客户端应用内，因此不存在 /admin 静态路由；
	// 未匹配路径统一走 NotFound，避免暴露"存在管理页"的误导信息。
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		r.writeError(w, req, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		r.writeError(w, req, apperr.New("system.method_not_allowed", http.StatusMethodNotAllowed))
	})
}

// ---- 鉴权辅助 ----

// principal 取出调用者身份。
func (r *Router) principal(req *http.Request) (*app.Principal, error) {
	p, ok := app.PrincipalFrom(req.Context())
	if !ok || p == nil {
		return nil, apperr.AuthRequired()
	}
	return p, nil
}

// requireSuperAdminMW 是"仅超级管理员"中间件。
func (r *Router) requireSuperAdminMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p, err := r.principal(req)
		if err != nil {
			r.writeError(w, req, err)
			return
		}
		if !p.IsSuperAdmin() {
			r.writeError(w, req, apperr.AuthForbidden().WithArg("reason", "super_admin_required"))
			return
		}
		next.ServeHTTP(w, req)
	})
}

// requireSuperAdmin 把 handler 包装为"仅超级管理员可调用"。
func (r *Router) requireSuperAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.requireSuperAdminMW(h).ServeHTTP(w, req)
	}
}

// requireRepoPerm 校验调用者对存储库的权限，返回调用者。
func (r *Router) requireRepoPerm(req *http.Request, repoID string, min domain.Permission) (*app.Principal, error) {
	p, _, err := r.repoPerm(req, repoID, min)
	return p, err
}

// repoPerm 校验调用者对存储库的权限，返回调用者与**该库上的有效权限等级**。
//
// 详情类接口要把这个等级透给界面（见 repoDTO.Permission）：界面据此决定展示哪些管理入口。
// 只关心"放不放行"的调用点用 requireRepoPerm。
func (r *Router) repoPerm(req *http.Request, repoID string, min domain.Permission) (*app.Principal, domain.Permission, error) {
	p, err := r.principal(req)
	if err != nil {
		return nil, "", err
	}
	allowed, perm, err := r.deps.App.Repos().CanAccess(req.Context(), repoID, p.UserID, p.Role)
	if err != nil {
		return nil, "", err
	}
	if !allowed || !app.PermAtLeast(perm, min) {
		return nil, "", apperr.AuthForbidden().WithArg("reason", "repo_permission_denied")
	}
	return p, perm, nil
}

// requireDiskPerm 校验调用者对某磁盘所属存储库的权限。
func (r *Router) requireDiskPerm(req *http.Request, diskID string, min domain.Permission) (*app.Principal, error) {
	disk, err := r.deps.App.Disks().Get(req.Context(), diskID)
	if err != nil {
		return nil, err
	}
	return r.requireRepoPerm(req, disk.RepoID, min)
}

// requireAllocationPerm 校验调用者对某分配所属存储库的权限。
func (r *Router) requireAllocationPerm(req *http.Request, allocationID string, min domain.Permission) (*app.Principal, error) {
	alloc, err := r.deps.Store.GetAllocation(req.Context(), allocationID)
	if err != nil {
		return nil, err
	}
	return r.requireRepoPerm(req, alloc.RepoID, min)
}

// routePattern 返回当前请求命中的路由模板，用于访问日志与审计。
func routePattern(req *http.Request) string {
	if rc := chi.RouteContext(req.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return req.URL.Path
}

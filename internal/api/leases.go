package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"vault/internal/domain"
)

// mountRequest 是获取挂载参数的请求。
type mountRequest struct {
	// ClientID 客户端实例标识，全局唯一且稳定（不使用 MAC/IP）。
	ClientID string `json:"client_id"`
}

// handleMount 创建/续租租约并返回挂载参数。
func (r *Router) handleMount(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in mountRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	spec, err := r.deps.App.Leases().RequestMount(req.Context(), chi.URLParam(req, "id"), in.ClientID, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	// 响应内含一次性下发的 CHAP 密钥，禁止任何中间层缓存。
	w.Header().Set("Cache-Control", "no-store")
	r.writeJSON(w, http.StatusOK, spec)
}

// leaseClientRequest 是租约类接口的公共请求体。
type leaseClientRequest struct {
	ClientID string `json:"client_id"`
}

// heartbeatResponse 是心跳响应。
type heartbeatResponse struct {
	LeaseTTLSeconds int   `json:"lease_ttl_seconds"`
	ExpiresAt       int64 `json:"expires_at"`
}

// handleHeartbeat 心跳续期。
func (r *Router) handleHeartbeat(w http.ResponseWriter, req *http.Request) {
	var in leaseClientRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	ttl, expiresAt, err := r.deps.App.Leases().Heartbeat(req.Context(), chi.URLParam(req, "id"), in.ClientID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, heartbeatResponse{LeaseTTLSeconds: ttl, ExpiresAt: expiresAt})
}

// mountedRequest 是挂载点回写请求。
type mountedRequest struct {
	ClientID   string `json:"client_id"`
	MountPoint string `json:"mount_point"`
}

// handleMounted 记录客户端上报的挂载点。
func (r *Router) handleMounted(w http.ResponseWriter, req *http.Request) {
	var in mountedRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Leases().ReportMounted(req.Context(), chi.URLParam(req, "id"), in.ClientID, in.MountPoint); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// handleRelease 客户端主动卸载。
func (r *Router) handleRelease(w http.ResponseWriter, req *http.Request) {
	var in leaseClientRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Leases().Release(req.Context(), chi.URLParam(req, "id"), in.ClientID); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// handleListLeases 列出在线租约。
//
// 超级管理员可见全部；普通用户只能看到自己的租约。
func (r *Router) handleListLeases(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	leases, err := r.deps.App.Leases().List(req.Context(), queryString(req, "repo_id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if !p.IsSuperAdmin() {
		filtered := make([]domain.Lease, 0, len(leases))
		for _, l := range leases {
			if l.UserID == p.UserID {
				filtered = append(filtered, l)
			}
		}
		leases = filtered
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toLeaseDTOs(leases)})
}

// handleRevokeLease 管理员强制下线。
func (r *Router) handleRevokeLease(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Leases().Revoke(req.Context(), chi.URLParam(req, "id"), p.UserID); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// handleListJobs 列出任务。
func (r *Router) handleListJobs(w http.ResponseWriter, req *http.Request) {
	jobs, err := r.deps.Store.ListJobs(req.Context(),
		domain.JobState(queryString(req, "state")), queryInt(req, "limit", 100), queryInt(req, "offset", 0))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toJobDTOs(jobs)})
}

// handleGetJob 查询任务详情。
func (r *Router) handleGetJob(w http.ResponseWriter, req *http.Request) {
	job, err := r.deps.Store.GetJob(req.Context(), chi.URLParam(req, "id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toJobDTO(job))
}

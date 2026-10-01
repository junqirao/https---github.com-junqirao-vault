package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
)

// createUserRequest 是创建用户请求。
type createUserRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	Role       string `json:"role"`
	QuotaBytes int64  `json:"quota_bytes"`
	Remark     string `json:"remark"`
}

// updateUserRequest 是更新用户请求；nil 表示不修改。
type updateUserRequest struct {
	Role       *string `json:"role"`
	QuotaBytes *int64  `json:"quota_bytes"`
	Remark     *string `json:"remark"`
	Enabled    *bool   `json:"enabled"`
	Password   *string `json:"password"`
}

// handleListUsers 列出用户。
func (r *Router) handleListUsers(w http.ResponseWriter, req *http.Request) {
	users, err := r.deps.App.Users().List(req.Context(),
		queryString(req, "keyword"), queryInt(req, "limit", 100), queryInt(req, "offset", 0))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toUserDTOs(users)})
}

// createUserResponse 是创建用户的响应。
//
// 采用嵌入方式：JSON 与旧的裸用户对象保持同层级、同字段名（向后兼容），
// 仅在自动签发成功时多出可选字段 certificate。
type createUserResponse struct {
	userDTO
	Certificate *IssuedCertificateDTO `json:"certificate,omitempty"`
}

// handleCreateUser 创建用户，并在成功后自动签发一张客户端证书。
//
// 签发失败**不会**导致建用户失败：仅记 WARN（错误码，不含私钥）并省略 certificate。
func (r *Router) handleCreateUser(w http.ResponseWriter, req *http.Request) {
	var in createUserRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	role := domain.Role(in.Role)
	if role == "" {
		role = domain.RoleUser
	}
	user, err := r.deps.App.Users().Create(req.Context(), app.CreateUserInput{
		Username:   in.Username,
		Password:   in.Password,
		Role:       role,
		QuotaBytes: in.QuotaBytes,
		Remark:     in.Remark,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}

	resp := createUserResponse{userDTO: toUserDTO(user)}
	if issued, issueErr := r.deps.App.Auth().IssueClientCertificate(req.Context(), user.ID, "", ""); issueErr != nil {
		r.deps.Log.Warn("用户已创建，但自动签发客户端证书失败",
			"user_id", user.ID, "code", apperr.CodeOf(issueErr))
	} else {
		resp.Certificate = toIssuedCertificateDTO(issued)
	}
	r.writeJSON(w, http.StatusCreated, resp)
}

// createCertificateRequest 是手动签发客户端证书请求；字段均可选。
type createCertificateRequest struct {
	BoundIP  string `json:"bound_ip"`
	BoundMAC string `json:"bound_mac"`
}

// handleCreateCertificate 为指定用户生成密钥对并签发客户端证书。
//
// 私钥（key_pem）仅在本次响应中返回一次。
func (r *Router) handleCreateCertificate(w http.ResponseWriter, req *http.Request) {
	var in createCertificateRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	issued, err := r.deps.App.Auth().IssueClientCertificate(
		req.Context(), chi.URLParam(req, "id"), in.BoundIP, in.BoundMAC)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toIssuedCertificateDTO(issued))
}

// handleGetUser 查询用户详情。
func (r *Router) handleGetUser(w http.ResponseWriter, req *http.Request) {
	user, err := r.deps.App.Users().Get(req.Context(), chi.URLParam(req, "id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toUserDTO(user))
}

// handleUpdateUser 更新用户。
func (r *Router) handleUpdateUser(w http.ResponseWriter, req *http.Request) {
	var in updateUserRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	input := app.UpdateUserInput{ID: chi.URLParam(req, "id")}
	if in.Role != nil {
		role := domain.Role(*in.Role)
		input.Role = &role
	}
	input.QuotaBytes = in.QuotaBytes
	input.Remark = in.Remark
	input.Enabled = in.Enabled
	input.Password = in.Password

	user, err := r.deps.App.Users().Update(req.Context(), input)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toUserDTO(user))
}

// handleDeleteUser 删除用户。
//
// 前置：该用户不能有未释放的分配（否则会产生悬空分配）。
func (r *Router) handleDeleteUser(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	user, err := r.deps.App.Users().Get(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if user.IsSuperAdmin() {
		r.writeError(w, req, apperrForbiddenReason("super_admin_undeletable"))
		return
	}
	allocs, err := r.deps.Store.ListAllocationsByUser(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	for _, a := range allocs {
		if a.State != domain.AllocationStateReleased {
			r.writeError(w, req, apperrConflict("user.has_allocations"))
			return
		}
	}
	if err := r.deps.App.Users().Delete(req.Context(), id); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// handleListCertificates 列出用户证书。
func (r *Router) handleListCertificates(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.deps.App.Users().Get(req.Context(), id); err != nil {
		r.writeError(w, req, err)
		return
	}
	certs, err := r.deps.Store.ListCertificatesByUser(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toCertificateDTOs(certs)})
}

// handleRevokeCertificate 吊销证书。
func (r *Router) handleRevokeCertificate(w http.ResponseWriter, req *http.Request) {
	if err := r.deps.Store.RevokeCertificate(req.Context(), chi.URLParam(req, "cid")); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// enrollmentTokenResponse 是一次性注册令牌响应。
type enrollmentTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	ServerID  string `json:"server_instance_id"`
}

// handleEnrollmentToken 为用户签发一次性注册令牌。
func (r *Router) handleEnrollmentToken(w http.ResponseWriter, req *http.Request) {
	token, expiresAt, err := r.deps.App.Auth().IssueEnrollmentToken(req.Context(), chi.URLParam(req, "id"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusCreated, enrollmentTokenResponse{
		Token:     token,
		ExpiresAt: expiresAt.UnixMilli(),
		ServerID:  r.deps.App.RawConfig().Server.InstanceID,
	})
}

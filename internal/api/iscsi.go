package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
)

// initiatorDTO 是一条 initiator 白名单项。
type initiatorDTO struct {
	// Type 必须是 IQN | IPAddress | IPv6Address | DNSName | MACAddress。
	Type  string `json:"type"`
	Value string `json:"value"`
}

// setAuthorizationRequest 是「全量替换 initiator 白名单」请求。
type setAuthorizationRequest struct {
	Initiators []initiatorDTO `json:"initiators"`
}

// setAuthRequest 是设置 iSCSI 鉴权请求。
//
// ⚠️ chap_secret 只在请求中出现，**绝不会出现在任何响应中**（见 docs/implementation.md 5.3）。
type setAuthRequest struct {
	AuthMode   string `json:"auth_mode"`
	ChapUser   string `json:"chap_user"`
	ChapSecret string `json:"chap_secret"`
}

// requireTargetPerm 校验调用者对目标所属存储库的权限。
//
// 目标无法归属到存储库时（如历史脏数据）仅超级管理员可操作。
func (r *Router) requireTargetPerm(req *http.Request, targetID string, min domain.Permission) (*app.Principal, error) {
	p, err := r.principal(req)
	if err != nil {
		return nil, err
	}
	repoID, err := r.deps.App.Iscsi().RepoIDOfTarget(req.Context(), targetID)
	if err != nil {
		return nil, err
	}
	if repoID == "" {
		if !p.IsSuperAdmin() {
			return nil, apperrForbiddenReason("target_owner_required")
		}
		return p, nil
	}
	return r.requireRepoPerm(req, repoID, min)
}

// toInitiatorID 把 DTO 转为领域值对象并校验。
func toInitiatorID(d initiatorDTO) (domain.InitiatorID, error) {
	id := domain.InitiatorID{Type: domain.InitiatorIDType(strings.TrimSpace(d.Type)), Value: strings.TrimSpace(d.Value)}
	if !id.Type.Valid() {
		return domain.InitiatorID{}, apperr.InvalidParam("type")
	}
	if id.Value == "" {
		return domain.InitiatorID{}, apperr.InvalidParam("value")
	}
	return id, nil
}

// handleListRepoIscsi 列出某存储库下的 iSCSI 目标（含映射与授权）。
func (r *Router) handleListRepoIscsi(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	items, err := r.deps.App.Iscsi().ListSummaries(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleGetIscsiTarget 返回单个目标详情（授权列表、映射设备、启用状态）。
func (r *Router) handleGetIscsiTarget(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireTargetPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	sum, err := r.deps.App.Iscsi().Summary(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, sum)
}

// handleReplaceAuthorization 全量替换 initiator 白名单（对应 -InitiatorIds 覆盖语义）。
func (r *Router) handleReplaceAuthorization(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireTargetPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in setAuthorizationRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	ids := make([]domain.InitiatorID, 0, len(in.Initiators))
	for _, d := range in.Initiators {
		one, err := toInitiatorID(d)
		if err != nil {
			r.writeError(w, req, err)
			return
		}
		ids = append(ids, one)
	}
	if err := r.deps.App.Iscsi().SetAuthorization(req.Context(), id, ids); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.Audit(req.Context(), p.UserID, "iscsi.authorization.replace", "target:"+id, "", domain.AuditResultOK)
	r.writeTargetSummary(w, req, id)
}

// handleAddAuthorization 追加一条 initiator 白名单。
func (r *Router) handleAddAuthorization(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireTargetPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in initiatorDTO
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	one, err := toInitiatorID(in)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Iscsi().AddInitiator(req.Context(), id, one); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.Audit(req.Context(), p.UserID, "iscsi.authorization.add", "target:"+id, one.String(), domain.AuditResultOK)
	r.writeTargetSummary(w, req, id)
}

// handleRemoveAuthorization 移除一条 initiator 白名单（可来自请求体或查询参数）。
func (r *Router) handleRemoveAuthorization(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireTargetPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in initiatorDTO
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if in.Type == "" {
		in.Type = queryString(req, "type")
	}
	if in.Value == "" {
		in.Value = queryString(req, "value")
	}
	one, err := toInitiatorID(in)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Iscsi().RemoveInitiator(req.Context(), id, one); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.Audit(req.Context(), p.UserID, "iscsi.authorization.remove", "target:"+id, one.String(), domain.AuditResultOK)
	r.writeTargetSummary(w, req, id)
}

// handleSetIscsiAuth 设置鉴权（none | ip | chap）。
func (r *Router) handleSetIscsiAuth(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireTargetPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in setAuthRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	mode := domain.AuthMode(strings.TrimSpace(in.AuthMode))
	if !mode.Valid() {
		r.writeError(w, req, apperr.InvalidParam("auth_mode"))
		return
	}
	if _, err := r.deps.App.Iscsi().SetAuth(req.Context(), id, app.SetAuthInput{
		AuthMode:   mode,
		ChapUser:   in.ChapUser,
		ChapSecret: in.ChapSecret,
	}); err != nil {
		r.writeError(w, req, err)
		return
	}
	// 审计只记录模式，绝不记录 chap_secret。
	r.deps.App.Audit(req.Context(), p.UserID, "iscsi.set_auth", "target:"+id, in.AuthMode, domain.AuditResultOK)
	r.writeTargetSummary(w, req, id)
}

// handleDisableIscsiTarget 停用目标（等价踢下线；见 docs/implementation.md 5.8）。
func (r *Router) handleDisableIscsiTarget(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireTargetPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	sum, err := r.deps.App.Iscsi().Summary(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Iscsi().DisableTarget(req.Context(), sum.TargetName); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.Audit(req.Context(), p.UserID, "iscsi.disable_target", "target:"+id, "", domain.AuditResultOK)
	r.writeTargetSummary(w, req, id)
}

// writeTargetSummary 输出目标视图（不含任何密钥材料）。
func (r *Router) writeTargetSummary(w http.ResponseWriter, req *http.Request, targetID string) {
	sum, err := r.deps.App.Iscsi().Summary(req.Context(), targetID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, sum)
}

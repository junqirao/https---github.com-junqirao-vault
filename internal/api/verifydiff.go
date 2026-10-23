package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
	"vault/internal/domain"
)

// maxVerifyDiffBodyBytes 是上传核对清单的请求体上限。
//
// 清单可能包含几万个文件（含 SHA-256），比通用 8MiB 大；客户端侧已在 10 万文件处封顶，
// 这里给 32MiB 足够覆盖“路径 + 大小 + 哈希”的最坏情况。
const maxVerifyDiffBodyBytes int64 = 32 << 20

type verifyDiffRequest struct {
	// Directory 是客户端选定的目录（仅用于结果展示）。
	Directory string `json:"directory"`
	// Mode 是 size | hash；hash 表示清单里带了 SHA-256。
	Mode  string               `json:"mode"`
	Files []app.VerifyDiffFile `json:"files"`
}

// handleStartVerifyDiff 提交一次上传核对（异步任务）。
func (r *Router) handleStartVerifyDiff(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in verifyDiffRequest
	if err := r.decodeJSONLimit(req, &in, maxVerifyDiffBodyBytes); err != nil {
		r.writeError(w, req, err)
		return
	}
	j, err := r.deps.App.Disks().StartVerifyDiff(req.Context(), id, in.Directory, in.Mode, in.Files, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, toJobDTO(j))
}

// handleVerifyDiffProgress 返回核对任务的细粒度进度（阶段/当前文件/已处理字节）。
func (r *Router) handleVerifyDiffProgress(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	res, err := r.deps.App.Disks().GetVerifyDiffProgress(req.Context(), id, queryString(req, "job"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, res)
}

// handleVerifyDiffResult 读取一次成功核对的结果。
func (r *Router) handleVerifyDiffResult(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	res, err := r.deps.App.Disks().GetVerifyDiffResult(req.Context(), id, queryString(req, "job"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, res)
}

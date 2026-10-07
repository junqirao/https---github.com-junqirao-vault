package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
)

// maxChunkBodyBytes 是单个分块请求体的读取上限。
//
// 分块大小由 app 层决定（默认 8MiB），这里预留 1MiB 余量以便对"超出预期大小"的
// 请求给出明确错误而不是无限读取。
const maxChunkBodyBytes = app.DefaultUploadChunkSize + (1 << 20)

// uploadManifestEntryDTO 是清单中的单条文件记录。
type uploadManifestEntryDTO struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// uploadManifestDTO 是上传清单。
type uploadManifestDTO struct {
	Root  string                   `json:"root"`
	Files []uploadManifestEntryDTO `json:"files"`
}

// createUploadRequest 是创建上传会话请求。
//
// repo_mode / storage_id / quota_bytes 为**可选**扩展字段（向后兼容）：
//   - repo_mode：建库模式 shared | exclusive，留空默认 shared；
//   - storage_id：目标存储 ID，留空自动选（校验同 RepoService.Create）；
//   - quota_bytes：应用层配额，0 表示不限。
type createUploadRequest struct {
	RepoName string `json:"repo_name"`
	// Mode copy | move。
	Mode       string            `json:"mode"`
	Manifest   uploadManifestDTO `json:"manifest"`
	RepoMode   string            `json:"repo_mode"`
	StorageID  string            `json:"storage_id"`
	QuotaBytes int64             `json:"quota_bytes"`
	// ClientOS 将使用该库的客户端操作系统（windows | linux，留空按 windows）：
	// 决定建盘用的文件系统并记在盘上（见 domain.ClientOS），建完之后不能改。
	ClientOS string `json:"client_os"`
}

// parsePathInt 解析路径中的整型参数。
func parsePathInt(raw string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(raw))
}

// requireUploadOwner 校验当前调用者是否为上传会话的发起人（或超级管理员）。
func (r *Router) requireUploadOwner(req *http.Request, uploadID string) (*app.Principal, error) {
	p, err := r.principal(req)
	if err != nil {
		return nil, err
	}
	u, err := r.deps.Store.GetUpload(req.Context(), uploadID)
	if err != nil {
		return nil, err
	}
	if !p.IsSuperAdmin() && u.UserID != p.UserID {
		return nil, apperrForbiddenReason("upload_owner_mismatch")
	}
	return p, nil
}

// handleCreateUpload 创建分块上传会话（见 docs/implementation.md 5.10 步骤 ①）。
func (r *Router) handleCreateUpload(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in createUploadRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	files := make([]domain.ManifestEntry, 0, len(in.Manifest.Files))
	for _, f := range in.Manifest.Files {
		files = append(files, domain.ManifestEntry{Path: f.Path, Size: f.Size, Mtime: f.Mtime, SHA256: f.SHA256})
	}
	session, err := r.deps.App.Uploads().CreateUpload(req.Context(), app.CreateUploadInput{
		UserID:     p.UserID,
		RepoName:   in.RepoName,
		Mode:       in.Mode,
		Manifest:   domain.UploadManifest{Root: in.Manifest.Root, Files: files},
		RepoMode:   in.RepoMode,
		StorageID:  in.StorageID,
		QuotaBytes: in.QuotaBytes,
		ClientOS:   in.ClientOS,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusCreated, session)
}

// handleGetUpload 查询上传会话状态（断点续传用）。
func (r *Router) handleGetUpload(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireUploadOwner(req, id); err != nil {
		r.writeError(w, req, err)
		return
	}
	st, err := r.deps.App.Uploads().Status(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, st)
}

// handlePutUploadChunk 上传单个分块（原始字节；X-Chunk-SHA256 为十六进制小写）。
//
// 幂等：同 index 且校验一致时直接返回 200。
func (r *Router) handlePutUploadChunk(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireUploadOwner(req, id); err != nil {
		r.writeError(w, req, err)
		return
	}
	index, err := parsePathInt(chi.URLParam(req, "index"))
	if err != nil {
		r.writeError(w, req, apperr.InvalidParam("index"))
		return
	}
	sha := strings.TrimSpace(req.Header.Get("X-Chunk-SHA256"))
	if sha == "" {
		r.writeError(w, req, apperr.InvalidParam("X-Chunk-SHA256"))
		return
	}
	var body io.Reader = io.LimitReader(req.Body, maxChunkBodyBytes)
	res, err := r.deps.App.Uploads().PutChunk(req.Context(), id, index, sha, body)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	r.writeJSON(w, http.StatusOK, res)
}

// handleCompleteUpload 完成上传并触发建盘任务（返回 202 + job_id）。
func (r *Router) handleCompleteUpload(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireUploadOwner(req, id); err != nil {
		r.writeError(w, req, err)
		return
	}
	j, err := r.deps.App.Uploads().Complete(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, map[string]any{"job_id": j.ID})
}

// handleAbortUpload 中止上传并清理暂存目录。
func (r *Router) handleAbortUpload(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireUploadOwner(req, id); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Uploads().Abort(req.Context(), id); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

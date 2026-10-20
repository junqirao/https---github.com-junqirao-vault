package api

import (
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// handleListParentFiles 列出母盘某个目录（维护态，服务端本地挂载）。
func (r *Router) handleListParentFiles(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	listing, err := r.deps.App.Repos().ListParentFiles(req.Context(), id, queryString(req, "path"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, listing)
}

// handleDownloadParentFile 下载母盘里的一个文件（支持 Range）。
func (r *Router) handleDownloadParentFile(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	f, info, err := r.deps.App.Repos().OpenParentFile(req.Context(), id, queryString(req, "path"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	defer func() { _ = f.Close() }()

	name := safeContentDispositionName(info.Name())
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "no-store")
	r.deps.Log.Info("下载母盘文件", "repo_id", id, "path", queryString(req, "path"), "size", info.Size())
	http.ServeContent(w, req, info.Name(), info.ModTime(), f)
}

// handleUploadParentFile 上传/覆盖母盘里的一个文件（请求体即文件内容）。
func (r *Router) handleUploadParentFile(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	rel := queryString(req, "path")
	if strings.TrimSpace(rel) == "" || strings.HasSuffix(strings.TrimSpace(rel), "/") {
		r.writeError(w, req, apperr.InvalidParam("path"))
		return
	}
	if req.ContentLength > maxParentUploadBytes {
		r.writeError(w, req, apperr.New("parent.file_too_large", http.StatusRequestEntityTooLarge).
			WithArg("limit", maxParentUploadBytes))
		return
	}
	n, err := r.deps.App.Repos().SaveParentFile(req.Context(), id, rel,
		io.LimitReader(req.Body, maxParentUploadBytes+1))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if n > maxParentUploadBytes {
		// 没有 Content-Length（分块上传）时超限：把已写入的文件删掉，避免留下半个大文件。
		_ = r.deps.App.Repos().DeleteParentEntry(req.Context(), id, rel)
		r.writeError(w, req, apperr.New("parent.file_too_large", http.StatusRequestEntityTooLarge).
			WithArg("limit", maxParentUploadBytes))
		return
	}
	r.deps.App.Audit(req.Context(), p.UserID, "repo.parent.file.upload", "repo:"+id, "path="+rel, domain.AuditResultOK)
	r.writeJSON(w, http.StatusOK, map[string]any{"path": rel, "size": n})
}

// handleReadParentText 读取文本文件（网页编辑器）。
func (r *Router) handleReadParentText(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	content, err := r.deps.App.Repos().ReadParentText(req.Context(), id, queryString(req, "path"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"content": content})
}

type writeParentTextRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// handleWriteParentText 保存文本文件（网页编辑器）。
func (r *Router) handleWriteParentText(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in writeParentTextRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Repos().WriteParentText(req.Context(), id, in.Path, []byte(in.Content)); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.Audit(req.Context(), p.UserID, "repo.parent.file.write", "repo:"+id, "path="+in.Path, domain.AuditResultOK)
	r.writeNoContent(w)
}

// handleDeleteParentEntry 删除母盘里的文件或空目录。
func (r *Router) handleDeleteParentEntry(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	rel := queryString(req, "path")
	if err := r.deps.App.Repos().DeleteParentEntry(req.Context(), id, rel); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.Audit(req.Context(), p.UserID, "repo.parent.file.delete", "repo:"+id, "path="+rel, domain.AuditResultOK)
	r.writeNoContent(w)
}

type parentFileDirRequest struct {
	Path string `json:"path"`
}

// handleMkdirParent 在母盘里新建目录。
func (r *Router) handleMkdirParent(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	var in parentFileDirRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Repos().MkdirParent(req.Context(), id, in.Path); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

type parentFileMoveRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// handleMoveParentEntry 重命名/移动母盘里的文件或目录。
func (r *Router) handleMoveParentEntry(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in parentFileMoveRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	to, err := r.deps.App.Repos().RenameParentEntry(req.Context(), id, in.From, in.To)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.deps.App.Audit(req.Context(), p.UserID, "repo.parent.file.move", "repo:"+id,
		"from="+in.From+" to="+to, domain.AuditResultOK)
	r.writeJSON(w, http.StatusOK, map[string]any{"path": to})
}

// maxParentUploadBytes 是单次网页上传的上限（8 GiB）。更大的内容建议先下载/拷贝到磁盘再整体替换。
const maxParentUploadBytes int64 = 8 << 30

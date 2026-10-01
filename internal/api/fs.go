package api

import "net/http"

// 「源目录选择」的服务端接口（见 docs/implementation.md 6.3）：
//   - GET /v1/fs/roots  → 列出可浏览的源目录根（白名单见 config.storage.source_roots）
//   - GET /v1/fs/browse → 列出某目录下的子目录（只列目录；path 必填）
//   - GET /v1/fs/stat   → 递归统计某目录的文件数与总字节数（带上限/时间预算）
//
// 安全约束（由 app.FSService 统一实施）：路径绝对、Clean、禁止 UNC、必须落在某个
// source root 之下（大小写不敏感、按目录边界比较），`..` 与符号链接不跟随。

// handleFSRoots 列出可浏览的源目录根。
func (r *Router) handleFSRoots(w http.ResponseWriter, req *http.Request) {
	if _, err := r.principal(req); err != nil {
		r.writeError(w, req, err)
		return
	}
	roots, err := r.deps.App.FS().Roots(req.Context())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	items := make([]fsRootDTO, 0, len(roots))
	for _, root := range roots {
		items = append(items, fsRootDTO{Name: root.Name, Path: root.Path, Exists: root.Exists})
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleFSBrowse 列出指定目录下的子目录（只列目录，不列文件）。
//
// 路径不存在时返回 200 + exists=false（items 为空），不是错误。
func (r *Router) handleFSBrowse(w http.ResponseWriter, req *http.Request) {
	if _, err := r.principal(req); err != nil {
		r.writeError(w, req, err)
		return
	}
	res, err := r.deps.App.FS().Browse(req.Context(), queryString(req, "path"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	items := make([]fsEntryDTO, 0, len(res.Items))
	for _, it := range res.Items {
		items = append(items, fsEntryDTO{Name: it.Name, Path: it.Path, HasChildren: it.HasChildren})
	}
	out := map[string]any{"path": res.Path, "root": res.Root, "exists": res.Exists, "items": items}
	if res.Parent != "" {
		out["parent"] = res.Parent
	}
	r.writeJSON(w, http.StatusOK, out)
}

// handleFSStat 递归统计指定目录的文件数与总字节数。
//
// 路径不存在时返回 200 + exists=false（计数为 0），不是错误。
func (r *Router) handleFSStat(w http.ResponseWriter, req *http.Request) {
	if _, err := r.principal(req); err != nil {
		r.writeError(w, req, err)
		return
	}
	res, err := r.deps.App.FS().Stat(req.Context(), queryString(req, "path"))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{
		"path":        res.Path,
		"exists":      res.Exists,
		"file_count":  res.FileCount,
		"total_bytes": res.TotalBytes,
		"truncated":   res.Truncated,
	})
}

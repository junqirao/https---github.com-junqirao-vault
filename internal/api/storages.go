package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
)

// createStorageRequest 是创建存储请求。
type createStorageRequest struct {
	Name string `json:"name"`
	// Path 挂载点（绝对路径；禁止 UNC）。留空时由服务端按名称推导默认挂载点。
	//
	// 目录模式（register_dir）必须显式给出：它没有"卷"可用于推导。
	Path string `json:"path"`
	// MountPoint 与 Path 同义，供前端「高级 → 自定义挂载点」使用；两者都填时以 mount_point 为准。
	MountPoint string `json:"mount_point"`
	// Enabled 留空视为 true。
	Enabled *bool `json:"enabled"`
	// Mode 见 app.StorageMode*（thin | register_dir | register_lv）；留空按平台取默认。
	//
	// Windows 上没有底层卷概念，thin / register_lv 会返回 501 platform.unsupported。
	Mode string `json:"mode"`
	// SizeBytes 分配容量（字节）；mode=thin 时必填且 > 0。
	SizeBytes int64 `json:"size_bytes"`
	// FileSystem 存储卷文件系统（ext4 默认 / xfs）；仅 mode=thin 使用。
	FileSystem string `json:"file_system"`
	// Ref 已有 LV 引用（如 /dev/mapper/<vg>-<lv>）；mode=register_lv 时必填。
	Ref string `json:"ref"`
}

// updateStorageRequest 是更新存储请求；nil 表示不修改。
type updateStorageRequest struct {
	Name    *string `json:"name"`
	Path    *string `json:"path"`
	Enabled *bool   `json:"enabled"`
}

// resizeStorageRequest 是存储扩容请求（仅 thin 卷）。
type resizeStorageRequest struct {
	SizeBytes int64 `json:"size_bytes"`
}

// handleListStorages 列出存储（任何登录用户可读：新建存储库表单需要下拉数据）。
func (r *Router) handleListStorages(w http.ResponseWriter, req *http.Request) {
	views, err := r.deps.App.Storages().List(req.Context())
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toStorageDTOs(views)})
}

// handleCreateStorage 创建存储（仅超级管理员）。
func (r *Router) handleCreateStorage(w http.ResponseWriter, req *http.Request) {
	var in createStorageRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	// mount_point 是「高级」选项的同义字段，优先于 path。
	path := in.Path
	if in.MountPoint != "" {
		path = in.MountPoint
	}
	view, err := r.deps.App.Storages().Create(req.Context(), app.CreateStorageInput{
		Name:       in.Name,
		Path:       path,
		Enabled:    enabled,
		Mode:       in.Mode,
		SizeBytes:  in.SizeBytes,
		FileSystem: in.FileSystem,
		Ref:        in.Ref,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusCreated, toStorageDTO(*view))
}

// handleUpdateStorage 更新存储（仅超级管理员；可改 name / path / enabled）。
func (r *Router) handleUpdateStorage(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	var in updateStorageRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	view, err := r.deps.App.Storages().Update(req.Context(), id, app.UpdateStorageInput{
		Name:    in.Name,
		Path:    in.Path,
		Enabled: in.Enabled,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toStorageDTO(*view))
}

// handleDeleteStorage 删除存储（仅超级管理员）。仅删记录，绝不删除磁盘文件。
func (r *Router) handleDeleteStorage(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if err := r.deps.App.Storages().Delete(req.Context(), id); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// handleMountStorage 挂载存储的底层卷（仅超级管理员）。
//
// 目录模式 / Windows（平台无存储卷概念）返回 501 platform.unsupported，
// 未登记底层卷返回 404 storage.volume_not_found，均由 app 层产生。
func (r *Router) handleMountStorage(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	view, err := r.deps.App.Storages().Mount(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toStorageDTO(*view))
}

// handleUnmountStorage 卸载存储的底层卷（仅超级管理员）。
//
// 有进行中的上传会话时返回 409 storage.volume_busy：暂存目录就建在该卷上，
// 卸载会让写入静默落到宿主根文件系统。
func (r *Router) handleUnmountStorage(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	view, err := r.deps.App.Storages().Unmount(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toStorageDTO(*view))
}

// handleResizeStorage 扩容存储的底层卷（仅超级管理员；仅支持系统创建的 thin 卷）。
func (r *Router) handleResizeStorage(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	var in resizeStorageRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	view, err := r.deps.App.Storages().Resize(req.Context(), id, in.SizeBytes)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toStorageDTO(*view))
}

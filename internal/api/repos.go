package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"vault/internal/app"
	"vault/internal/apperr"
	"vault/internal/domain"
)

// createRepoRequest 是创建存储库请求。
type createRepoRequest struct {
	Name string `json:"name"`
	// Mode shared | exclusive。
	Mode string `json:"mode"`
	// OwnerID 仅超级管理员可指定；普通用户只能为自己建库。
	OwnerID      string `json:"owner_id"`
	SourceDir    string `json:"source_dir"`
	SizeBytes    int64  `json:"size_bytes"`
	MaxDiffDisks int    `json:"max_diff_disks"`
	QuotaBytes   int64  `json:"quota_bytes"`
	Group        string `json:"group"`
	// StorageID 可选的目标存储 ID（留空则在所有启用存储中自动选根）。
	StorageID string `json:"storage_id"`
}

// updateRepoRequest 是更新存储库请求；nil 表示不修改。
type updateRepoRequest struct {
	Name         *string `json:"name"`
	QuotaBytes   *int64  `json:"quota_bytes"`
	MaxDiffDisks *int    `json:"max_diff_disks"`
	Group        *string `json:"group"`
}

// handleListRepos 列出存储库。
//
// 可见性：超级管理员可见全部；普通用户仅可见自己拥有的库（成员库通过 GET /{id} 访问）。
func (r *Router) handleListRepos(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	ownerID := queryString(req, "owner_id")
	if !p.IsSuperAdmin() {
		ownerID = p.UserID
	}
	repos, err := r.deps.App.Repos().List(req.Context(), ownerID, queryInt(req, "limit", 100), queryInt(req, "offset", 0))
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toRepoDTOs(repos)})
}

// handleCreateRepo 创建存储库。
func (r *Router) handleCreateRepo(w http.ResponseWriter, req *http.Request) {
	p, err := r.principal(req)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in createRepoRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	owner := in.OwnerID
	if owner == "" {
		owner = p.UserID
	}
	if !p.IsSuperAdmin() && owner != p.UserID {
		r.writeError(w, req, apperrForbiddenReason("owner_mismatch"))
		return
	}

	repo, disk, err := r.deps.App.Repos().Create(req.Context(), app.CreateRepoInput{
		Name:         in.Name,
		Mode:         domain.RepoMode(in.Mode),
		OwnerID:      owner,
		SourceDir:    in.SourceDir,
		SizeBytes:    in.SizeBytes,
		MaxDiffDisks: in.MaxDiffDisks,
		QuotaBytes:   in.QuotaBytes,
		Group:        in.Group,
		StorageID:    in.StorageID,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusCreated, map[string]any{
		"repo": toRepoDTO(repo),
		"disk": toDiskDTO(disk),
	})
}

// handleGetRepo 查询存储库详情。
func (r *Router) handleGetRepo(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	repo, err := r.deps.App.Repos().Get(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toRepoDTO(repo))
}

// handleUpdateRepo 更新存储库。
func (r *Router) handleUpdateRepo(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	var in updateRepoRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	repo, err := r.deps.App.Repos().Update(req.Context(), id, app.UpdateRepoInput{
		Name:         in.Name,
		QuotaBytes:   in.QuotaBytes,
		MaxDiffDisks: in.MaxDiffDisks,
		Group:        in.Group,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toRepoDTO(repo))
}

// handleDeleteRepo 删除存储库（异步回收）。
func (r *Router) handleDeleteRepo(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Repos().Delete(req.Context(), id); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

// handleListMembers 列出可管理列表。
func (r *Router) handleListMembers(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	members, err := r.deps.App.Repos().ListMembers(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	out := make([]memberDTO, 0, len(members))
	for _, m := range members {
		out = append(out, memberDTO{UserID: m.UserID, Perm: string(m.Perm)})
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// setMembersRequest 是覆盖式设置成员请求。
type setMembersRequest struct {
	Members []memberDTO `json:"members"`
}

// handleSetMembers 覆盖式设置可管理列表（仅 owner / 超级管理员）。
func (r *Router) handleSetMembers(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in setMembersRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	members := make([]domain.RepoMember, 0, len(in.Members))
	for _, m := range in.Members {
		members = append(members, domain.RepoMember{
			RepoID: id,
			UserID: m.UserID,
			Perm:   domain.Permission(m.Perm),
		})
	}
	if err := r.deps.App.Repos().SetMembers(req.Context(), id, p.UserID, members); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.handleListMembers(w, req)
}

// handleListAllocations 列出存储库的分配。
func (r *Router) handleListAllocations(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	allocs, err := r.deps.App.Repos().ListAllocations(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toAllocationDTOs(allocs)})
}

// allocateRequest 是创建分配请求。
type allocateRequest struct {
	UserID string `json:"user_id"`
}

// handleAllocate 创建分配（共享库派生差异盘 / 独享库建立映射）。
func (r *Router) handleAllocate(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in allocateRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	alloc, err := r.deps.App.Repos().Allocate(req.Context(), app.AllocateInput{
		RepoID:     id,
		UserID:     in.UserID,
		OperatorID: p.UserID,
	})
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusCreated, toAllocationDTO(alloc))
}

// parentActionRequest 是母盘用途迁移请求。
type parentActionRequest struct {
	// Action derive | temp_share | unshare | maintenance | finish_maintenance | cleanup_diffs。
	Action string `json:"action"`
}

// handleParentAction 执行母盘用途迁移。
func (r *Router) handleParentAction(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in parentActionRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Repos().TransitionParent(req.Context(), id, app.ParentAction(in.Action), p.UserID); err != nil {
		r.writeError(w, req, err)
		return
	}
	repo, err := r.deps.App.Repos().Get(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toRepoDTO(repo))
}

// copyRepoRequest 是复制存储库请求。
type copyRepoRequest struct {
	NewRepoName string `json:"new_repo_name"`
}

// handleCopyRepo 复制母盘为新的独立存储库（返回任务）。
func (r *Router) handleCopyRepo(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireRepoPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	var in copyRepoRequest
	if err := r.decodeJSON(req, &in); err != nil {
		r.writeError(w, req, err)
		return
	}
	repo, err := r.deps.App.Repos().Get(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if repo.ParentDiskID == nil {
		r.writeError(w, req, apperr.New("repo.disk_missing", http.StatusConflict))
		return
	}
	j, err := r.deps.App.Disks().StartCopy(req.Context(), *repo.ParentDiskID, in.NewRepoName, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, toJobDTO(j))
}

// handleListRepoDisks 列出存储库下的磁盘。
func (r *Router) handleListRepoDisks(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireRepoPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	disks, err := r.deps.App.Disks().ListByRepo(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, map[string]any{"items": toDiskDTOs(disks)})
}

// handleGetDisk 查询磁盘。
func (r *Router) handleGetDisk(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireDiskPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	disk, err := r.deps.App.Disks().Get(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusOK, toDiskDTO(disk))
}

// handleDeleteDisk 删除磁盘（异步）。
func (r *Router) handleDeleteDisk(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	p, err := r.requireDiskPerm(req, id, domain.PermManage)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	j, err := r.deps.App.Disks().StartDelete(req.Context(), id, p.UserID)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, toJobDTO(j))
}

// handleCompactDisk 回收磁盘空间（异步）。
func (r *Router) handleCompactDisk(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireDiskPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	j, err := r.deps.App.Disks().StartCompact(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeJSON(w, http.StatusAccepted, toJobDTO(j))
}

// handleDownloadDiskContent 输出母盘 VHDX 的原始字节，支持 Range 断点续传。
//
// 只允许母盘：差异盘/独立盘不提供内容下载（见 docs/agent-api.md「磁盘内容下载」）。
// 用 http.ServeContent 让标准库处理 Range / Content-Length / 206 / 416 / If-Range，
// 与 api/update.go 的 handleUpdateArtifact 保持同一范式。
func (r *Router) handleDownloadDiskContent(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireDiskPerm(req, id, domain.PermRead); err != nil {
		r.writeError(w, req, err)
		return
	}
	disk, err := r.deps.App.Disks().Get(req.Context(), id)
	if err != nil {
		r.writeError(w, req, err)
		return
	}
	if disk.Kind != domain.DiskKindParent {
		r.writeError(w, req, apperr.DiskNotParent().WithArg("kind", string(disk.Kind)))
		return
	}

	path := disk.VHDXPath
	f, err := os.Open(path)
	if err != nil {
		r.deps.Log.Warn("打开母盘失败", "disk_id", id, "path", path, "error", err)
		r.writeError(w, req, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		r.writeError(w, req, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
		return
	}

	name := safeContentDispositionName(filepath.Base(path))
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "no-store")

	// 日志只记录元信息，绝不记录文件内容。
	r.deps.Log.Info("下载母盘内容",
		"disk_id", id, "size_bytes", info.Size(), "range", req.Header.Get("Range") != "")

	http.ServeContent(w, req, name, info.ModTime(), f)
}

// safeContentDispositionName 清洗将写入 Content-Disposition 的文件名。
//
// 去掉路径分隔符、引号、反斜杠与控制字符，避免响应头注入；清洗后为空时回退为 disk.vhdx。
func safeContentDispositionName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r == '/' {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "disk.vhdx"
	}
	return name
}

// handleReleaseAllocation 释放分配（异步删除差异盘）。
func (r *Router) handleReleaseAllocation(w http.ResponseWriter, req *http.Request) {
	id := chi.URLParam(req, "id")
	if _, err := r.requireAllocationPerm(req, id, domain.PermManage); err != nil {
		r.writeError(w, req, err)
		return
	}
	if err := r.deps.App.Repos().ReleaseAllocation(req.Context(), id); err != nil {
		r.writeError(w, req, err)
		return
	}
	r.writeNoContent(w)
}

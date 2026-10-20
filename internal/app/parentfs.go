package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
)

// parentTextLimit 是网页编辑器单文件读取/保存上限（超过请下载后用本地编辑器）。
const parentTextLimit = 2 << 20 // 2 MiB

// ParentFileEntry 是母盘文件列表中的一项。
type ParentFileEntry struct {
	Name string `json:"name"`
	// Path 是相对挂载根的路径，统一用 "/" 分隔（根为 "/"）。
	Path       string `json:"path"`
	IsDir      bool   `json:"is_dir"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modified_at"`
}

// ParentFileListing 是母盘某个目录的列表。
type ParentFileListing struct {
	Path  string            `json:"path"`
	Items []ParentFileEntry `json:"items"`
}

// ParentMount 返回（必要时建立）母盘卷在服务端本地的挂载根。
//
// 只允许在维护态调用：这是"管理员直接改母盘内容"的入口，正常态下母盘是所有差异盘的基线。
// 挂载是懒建立的（第一次访问文件页面时才 attach/挂载），退出维护时由
// UnmountParentVolume 卸载。
func (s *RepoService) ParentMount(ctx context.Context, repoID string) (string, error) {
	if s.Vol == nil {
		return "", apperr.New(apperr.CodeUnavailable, http.StatusServiceUnavailable).
			WithArg("component", "volume")
	}
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return "", err
	}
	if !repo.IsShared() {
		return "", apperr.New("repo.not_shared", http.StatusConflict).WithArg("mode", string(repo.Mode))
	}
	if repo.Condition() != domain.ParentMaintenance {
		return "", apperr.RepoParentConditionViolation(
			string(repo.Condition()), string(domain.ParentMaintenance))
	}
	if repo.ParentDiskID == nil {
		return "", apperr.New("repo.disk_missing", http.StatusInternalServerError)
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return "", err
	}

	// 整段持锁：挂载是命令级重活，不能让两个并发请求各 attach 一次。
	s.parentMountMu.Lock()
	defer s.parentMountMu.Unlock()
	if m, ok := s.parentMounts[repoID]; ok && m.root != "" && dirExists(m.root) {
		return m.root, nil
	}
	fs := parent.FileSystemOrDefault().String()
	root, err := s.Vol.MountVolume(ctx, parent.VHDXPath, fs)
	if err != nil {
		return "", err
	}
	if s.parentMounts == nil {
		s.parentMounts = make(map[string]parentVolumeMount)
	}
	s.parentMounts[repoID] = parentVolumeMount{root: root, ref: parent.VHDXPath}
	s.Log.Info("母盘已在服务端挂载", "repo_id", repoID, "root", root, "file_system", fs)
	return root, nil
}

// UnmountParentVolume 卸载母盘卷的服务端本地挂载（幂等、尽力而为）。
func (s *RepoService) UnmountParentVolume(ctx context.Context, repoID string) {
	// 与 ParentMount 用同一把锁：避免"正在卸载时另一个请求又把它挂上"。
	s.parentMountMu.Lock()
	defer s.parentMountMu.Unlock()
	m, ok := s.parentMounts[repoID]
	if !ok || s.Vol == nil {
		return
	}
	delete(s.parentMounts, repoID)
	if err := s.Vol.UnmountVolume(ctx, m.ref, m.root); err != nil {
		s.Log.Warn("卸载母盘本地挂载失败", "repo_id", repoID, "root", m.root, "error", err)
	}
}

// ListParentFiles 列出母盘某个目录下的文件（目录在前，名称升序）。
func (s *RepoService) ListParentFiles(ctx context.Context, repoID, rel string) (*ParentFileListing, error) {
	root, err := s.ParentMount(ctx, repoID)
	if err != nil {
		return nil, err
	}
	abs, clean, err := resolveParentPath(root, rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound).WithArg("path", clean)
		}
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "read").WithCause(err)
	}
	items := make([]ParentFileEntry, 0, len(entries))
	for _, e := range entries {
		// 不跟随、也不展示符号链接（避免通过链接逃出挂载根）。
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, iErr := e.Info()
		if iErr != nil {
			continue
		}
		items = append(items, ParentFileEntry{
			Name:       e.Name(),
			Path:       path.Join(clean, e.Name()),
			IsDir:      e.IsDir(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UnixMilli(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return &ParentFileListing{Path: clean, Items: items}, nil
}

// OpenParentFile 打开一个文件用于下载；调用方负责关闭返回的 ReadSeekCloser。
func (s *RepoService) OpenParentFile(ctx context.Context, repoID, rel string) (io.ReadSeekCloser, os.FileInfo, error) {
	root, err := s.ParentMount(ctx, repoID)
	if err != nil {
		return nil, nil, err
	}
	abs, clean, err := resolveParentPath(root, rel)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound).WithArg("path", clean)
		}
		return nil, nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, apperr.InvalidParam("path")
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	return f, info, nil
}

// SaveParentFile 把 r 的内容写入 rel（覆盖或新建），父目录不存在时自动创建。
func (s *RepoService) SaveParentFile(ctx context.Context, repoID, rel string, r io.Reader) (int64, error) {
	root, err := s.ParentMount(ctx, repoID)
	if err != nil {
		return 0, err
	}
	abs, _, err := resolveParentPath(root, rel)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "mkdir").WithCause(err)
	}
	n, err := writeParentFileAtomic(abs, r)
	if err != nil {
		return n, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "write").WithCause(err)
	}
	return n, nil
}

// ReadParentText 读取文本文件供网页编辑器展示（上限 parentTextLimit）。
func (s *RepoService) ReadParentText(ctx context.Context, repoID, rel string) (string, error) {
	f, info, err := s.OpenParentFile(ctx, repoID, rel)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if info.Size() > parentTextLimit {
		return "", apperr.New("parent.file_too_large", http.StatusRequestEntityTooLarge).
			WithArg("limit", parentTextLimit)
	}
	b, err := io.ReadAll(io.LimitReader(f, parentTextLimit+1))
	if err != nil {
		return "", apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "read").WithCause(err)
	}
	if int64(len(b)) > parentTextLimit {
		return "", apperr.New("parent.file_too_large", http.StatusRequestEntityTooLarge).
			WithArg("limit", parentTextLimit)
	}
	return string(b), nil
}

// WriteParentText 覆盖写入文本文件（网页编辑器保存）。
func (s *RepoService) WriteParentText(ctx context.Context, repoID, rel string, data []byte) error {
	if int64(len(data)) > parentTextLimit {
		return apperr.New("parent.file_too_large", http.StatusRequestEntityTooLarge).
			WithArg("limit", parentTextLimit)
	}
	_, err := s.SaveParentFile(ctx, repoID, rel, bytes.NewReader(data))
	return err
}

// DeleteParentEntry 删除文件或**空**目录（不允许删挂载根；非空目录要求先清空）。
func (s *RepoService) DeleteParentEntry(ctx context.Context, repoID, rel string) error {
	root, err := s.ParentMount(ctx, repoID)
	if err != nil {
		return err
	}
	abs, clean, err := resolveParentPath(root, rel)
	if err != nil {
		return err
	}
	if samePath(abs, root) {
		return apperr.InvalidParam("path")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 幂等
		}
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	if info.IsDir() {
		entries, rErr := os.ReadDir(abs)
		if rErr != nil {
			return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(rErr)
		}
		if len(entries) > 0 {
			return apperr.New("parent.dir_not_empty", http.StatusConflict).WithArg("path", clean)
		}
	}
	if err := os.Remove(abs); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	return nil
}

// MkdirParent 创建目录（父目录不存在时一并创建）。
func (s *RepoService) MkdirParent(ctx context.Context, repoID, rel string) error {
	root, err := s.ParentMount(ctx, repoID)
	if err != nil {
		return err
	}
	abs, _, err := resolveParentPath(root, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "mkdir").WithCause(err)
	}
	return nil
}

// RenameParentEntry 重命名/移动文件或目录（目标已存在时拒绝），返回新的相对路径。
func (s *RepoService) RenameParentEntry(ctx context.Context, repoID, from, to string) (string, error) {
	root, err := s.ParentMount(ctx, repoID)
	if err != nil {
		return "", err
	}
	absFrom, _, err := resolveParentPath(root, from)
	if err != nil {
		return "", err
	}
	absTo, cleanTo, err := resolveParentPath(root, to)
	if err != nil {
		return "", err
	}
	if samePath(absFrom, root) {
		return "", apperr.InvalidParam("path")
	}
	if _, err := os.Lstat(absTo); err == nil {
		return "", apperr.New("parent.path_exists", http.StatusConflict).WithArg("path", cleanTo)
	}
	if err := os.MkdirAll(filepath.Dir(absTo), 0o755); err != nil {
		return "", apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "mkdir").WithCause(err)
	}
	if err := os.Rename(absFrom, absTo); err != nil {
		return "", apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "rename").WithCause(err)
	}
	return cleanTo, nil
}

// ---- 路径与文件工具 ----

// resolveParentPath 把"相对挂载根的路径"解析为绝对路径，并拒绝任何越界（`..`、绝对路径、NUL）。
func resolveParentPath(root, rel string) (abs, clean string, err error) {
	root = filepath.Clean(root)
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" {
		rel = "/"
	}
	if strings.ContainsRune(rel, 0) {
		return "", "", apperr.InvalidParam("path")
	}
	// 显式拒绝 `..`：虽然 path.Clean 会把它们夹在根内，但"请求 ../x 却操作了 /x"不符合预期。
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", "", apperr.InvalidParam("path")
		}
	}
	clean = path.Clean("/" + strings.TrimPrefix(rel, "/"))
	abs = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	abs = filepath.Clean(abs)
	if !samePath(abs, root) && !domain.UnderRoot(root, abs) {
		return "", "", apperr.InvalidParam("path")
	}
	return abs, clean, nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// writeParentFileAtomic 先写临时文件再改名，避免上传中途失败留下半个文件。
func writeParentFileAtomic(abs string, r io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".vault-upload-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		return n, err
	}
	return n, nil
}

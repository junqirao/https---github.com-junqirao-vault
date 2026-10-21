package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/job"
	"vault/internal/lock"
)

const (
	// verifyDiffMaxFiles 限制一次核对的文件数（清单走 HTTP JSON，过大没有意义）。
	verifyDiffMaxFiles = 100000
	// verifyDiffMaxList 限制结果里每一类差异返回的条目数（避免几万条差异把响应撑爆）。
	verifyDiffMaxList = 5000
	// verifyDiffBaseDir 是清单与结果文件的根目录（系统临时目录下）。
	verifyDiffBaseDir = "vault-verifydiff"
)

// VerifyDiffFile 是客户端清单里的一个文件（路径统一用 "/" 分隔）。
type VerifyDiffFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Hash 是 SHA-256（十六进制）；mode=size 或本地读失败时为空（只比大小）。
	Hash string `json:"hash,omitempty"`
}

// VerifyDiffEntry 是一条差异。
type VerifyDiffEntry struct {
	Path      string `json:"path"`
	LocalSize int64  `json:"local_size,omitempty"`
	DiskSize  int64  `json:"disk_size,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// VerifyDiffResult 是一次核对的完整结果。
type VerifyDiffResult struct {
	RepoID    string `json:"repo_id"`
	Directory string `json:"directory"`
	Mode      string `json:"mode"`
	Same      int    `json:"same"`
	// Missing 是"本地有、盘里没有"；Extra 是"盘里有、本地没有"；
	// Different 是两边都有但大小/哈希不同。
	Missing    []VerifyDiffEntry `json:"missing"`
	Extra      []VerifyDiffEntry `json:"extra"`
	Different  []VerifyDiffEntry `json:"different"`
	LocalFiles int               `json:"local_files"`
	DiskFiles  int               `json:"disk_files"`
	LocalBytes int64             `json:"local_bytes"`
	DiskBytes  int64             `json:"disk_bytes"`
	Warnings   []string          `json:"warnings"`
	Truncated  bool              `json:"truncated"`
}

type verifyDiffManifest struct {
	Directory string           `json:"directory"`
	Mode      string           `json:"mode"`
	Files     []VerifyDiffFile `json:"files"`
}

// verifyDiffPayload 是 JobVerifyDiff 的 payload。清单/结果都放临时文件，避免把几 MB
// 的 JSON 塞进 jobs 表。
type verifyDiffPayload struct {
	RepoID       string `json:"repo_id"`
	ManifestPath string `json:"manifest_path"`
	ResultPath   string `json:"result_path"`
	Mode         string `json:"mode"`
}

// StartVerifyDiff 提交一次"上传核对"任务。
//
// 客户端先在本机扫描选定目录（可选计算 SHA-256），把清单交给服务端；服务端随后从母盘
// 派生一块临时差异盘、在本地挂载并逐文件对比，结果写到临时 JSON 供界面查询。
func (s *DiskService) StartVerifyDiff(ctx context.Context, repoID, directory, mode string,
	files []VerifyDiffFile, operatorID string) (*domain.Job, error) {
	repo, err := s.Store.GetRepository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if !repo.IsShared() {
		return nil, apperr.New("repo.not_shared", http.StatusConflict).WithArg("mode", string(repo.Mode))
	}
	if repo.ParentDiskID == nil {
		return nil, apperr.New("repo.disk_missing", http.StatusInternalServerError)
	}
	if repo.State != domain.RepoStateActive {
		return nil, apperr.New("repo.state_invalid", http.StatusConflict).WithArg("state", string(repo.State))
	}
	mode = normalizeVerifyMode(mode)
	if len(files) == 0 {
		return nil, apperr.InvalidParam("files")
	}
	if len(files) > verifyDiffMaxFiles {
		return nil, apperr.New("verifydiff.too_many_files", http.StatusRequestEntityTooLarge).
			WithArg("limit", verifyDiffMaxFiles)
	}

	dir := filepath.Join(verifyDiffTempRoot(), uuid.NewString())
	// 结果文件要留给界面查询，不能立刻删；顺手清掉一天前的旧目录，避免无限堆积。
	cleanupOldVerifyDiffRuns(24 * time.Hour)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "mkdir").WithCause(err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := json.Marshal(verifyDiffManifest{Directory: directory, Mode: mode, Files: files})
	if err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "manifest").WithCause(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "write").WithCause(err)
	}

	payload := verifyDiffPayload{
		RepoID:       repo.ID,
		ManifestPath: manifestPath,
		ResultPath:   filepath.Join(dir, "result.json"),
		Mode:         mode,
	}
	j, _, err := s.Jobs.EnqueueWith(ctx, domain.JobVerifyDiff, repo.ID,
		"verifydiff:"+repo.ID+":"+uuid.NewString(), lock.RepoKey(repo.ID), payload)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, operatorID, "repo.verify_diff", "repo:"+repo.ID,
		fmt.Sprintf("files=%d mode=%s job=%s", len(files), mode, j.ID), domain.AuditResultOK)
	return j, nil
}

// GetVerifyDiffResult 读取一次核对的成功结果。
//
// 通过任务 payload 取结果路径（不拼接用户输入），并校验任务类型/所属库，避免跨库读取。
func (s *DiskService) GetVerifyDiffResult(ctx context.Context, repoID, jobID string) (*VerifyDiffResult, error) {
	jobID = strings.TrimSpace(jobID)
	if _, err := uuid.Parse(jobID); err != nil {
		return nil, apperr.InvalidParam("job_id")
	}
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if j.Type != domain.JobVerifyDiff || j.RefID != repoID {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	if j.State != domain.JobStateSucceeded {
		return nil, apperr.New("verifydiff.result_not_ready", http.StatusConflict).
			WithArg("state", string(j.State))
	}
	var p verifyDiffPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return nil, apperr.InvalidParam("payload").WithCause(err)
	}
	raw, err := os.ReadFile(p.ResultPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
		}
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	var out VerifyDiffResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	if out.RepoID != repoID {
		return nil, apperr.New(apperr.CodeNotFound, http.StatusNotFound)
	}
	return &out, nil
}

// runVerifyDiff 是 JobVerifyDiff 的实现：建临时差异盘 → 服务端挂载 → 遍历 → 对比。
func (s *DiskService) runVerifyDiff(ctx context.Context, j *domain.Job, rep job.Reporter) error {
	var p verifyDiffPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	manifestRaw, err := os.ReadFile(p.ManifestPath)
	if err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	var manifest verifyDiffManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return apperr.InvalidParam("manifest").WithCause(err)
	}
	// 清单可能有几 MB，任务读完就删；结果文件保留给界面查询。
	_ = os.Remove(p.ManifestPath)
	repo, err := s.Store.GetRepository(ctx, p.RepoID)
	if err != nil {
		return err
	}
	if repo.ParentDiskID == nil {
		return apperr.New("repo.disk_missing", http.StatusInternalServerError)
	}
	parent, err := s.Store.GetDisk(ctx, *repo.ParentDiskID)
	if err != nil {
		return err
	}

	rep.Progress(3)
	ref, cleanup, err := s.createVerifyDiff(ctx, repo, parent)
	if err != nil {
		return err
	}
	defer cleanup()

	fsName := parent.FileSystemOrDefault().String()
	rep.Progress(10)
	root, err := s.Vol.MountVolume(ctx, ref, fsName)
	if err != nil {
		return err
	}
	defer func() {
		// 用 WithoutCancel：请求/任务被取消时也要把临时卷卸掉，避免留下脏挂载。
		_ = s.Vol.UnmountVolume(context.WithoutCancel(ctx), ref, root)
	}()

	diskFiles, diskBytes, warnings, err := walkVerifyRoot(ctx, root, manifest.Mode, len(manifest.Files), rep)
	if err != nil {
		return err
	}
	rep.Progress(96)
	result := compareVerify(manifest, diskFiles, diskBytes, warnings)
	result.RepoID = repo.ID
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(err)
	}
	if err := os.WriteFile(p.ResultPath, raw, 0o600); err != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "write").WithCause(err)
	}
	rep.Progress(100)
	s.Log.Info("上传核对完成", "repo_id", repo.ID, "mode", manifest.Mode,
		"local_files", result.LocalFiles, "disk_files", result.DiskFiles,
		"missing", len(result.Missing), "extra", len(result.Extra), "different", len(result.Different))
	return nil
}

// createVerifyDiff 从母盘派生一块不登记的临时差异盘，返回其引用与清理函数。
func (s *DiskService) createVerifyDiff(ctx context.Context, repo *domain.Repository, parent *domain.Disk) (string, func(), error) {
	guard, err := s.Repos.pickGuardForPath(ctx, parent.VHDXPath, parent.SizeBytes)
	if err != nil {
		return "", nil, err
	}
	// 放在**独立目录**（不在 disks/ 下）：孤儿扫描会把 disks/ 里没有 DB 记录的 VHDX
	// 移进 meta/orphan，而核对可能跑很久，绝不能让它把正在读的临时盘搬走。
	rel := filepath.Join("verify", repo.ID, uuid.NewString()+".vhdx")
	ref, err := s.Disk.DiskRef(guard.Root, rel)
	if err != nil {
		return "", nil, apperr.New(apperr.CodeInternal, http.StatusInternalServerError).
			WithArg("reason", "disk_ref").WithCause(err)
	}
	if err := s.createDiffWithRetry(ctx, ref, parent.VHDXPath); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		if err := s.Disk.Delete(context.WithoutCancel(ctx), ref); err != nil {
			s.Log.Warn("删除核对用临时差异盘失败", "repo_id", repo.ID, "ref", ref, "error", err)
		}
	}
	return ref, cleanup, nil
}

type verifyDiskFile struct {
	size int64
	hash string
}

// walkVerifyRoot 遍历服务端挂载根，返回 路径→{大小,哈希}。
func walkVerifyRoot(ctx context.Context, root, mode string, total int, rep job.Reporter) (
	map[string]verifyDiskFile, int64, []string, error) {
	files := make(map[string]verifyDiskFile, total)
	warnings := make([]string, 0)
	var diskBytes int64
	seen := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			if p != root {
				warnings = append(warnings, "unreadable:"+verifyRel(root, p))
				return nil
			}
			return walkErr
		}
		rel := verifyRel(root, p)
		if rel == "." {
			return nil
		}
		// 卷根上的系统元数据是"格式化/挂载"自己产生的，不是上传内容；不跳过的话每次核对
		// 都会稳定多出 $RECYCLE.BIN / System Volume Information（Linux 上是 lost+found），
		// 把真正的差异淹没掉。
		if isVerifyRootMetadata(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			warnings = append(warnings, "symlink_skipped:"+rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, iErr := d.Info()
		if iErr != nil {
			warnings = append(warnings, "stat_skipped:"+rel)
			return nil
		}
		hash := ""
		if mode == "hash" {
			h, hErr := hashFileSHA256(p)
			if hErr != nil {
				warnings = append(warnings, "hash_failed:"+rel)
			} else {
				hash = h
			}
		}
		files[rel] = verifyDiskFile{size: info.Size(), hash: hash}
		diskBytes += info.Size()
		seen++
		if total > 0 && seen%200 == 0 {
			percent := 10 + 80*seen/total
			if percent > 90 {
				percent = 90
			}
			rep.Progress(percent)
		}
		return nil
	})
	return files, diskBytes, warnings, err
}

// compareVerify 对比客户端清单与服务端盘内容。差异按路径排序，列表有上限。
func compareVerify(m verifyDiffManifest, disk map[string]verifyDiskFile, diskBytes int64, warnings []string) *VerifyDiffResult {
	truncated := false
	res := &VerifyDiffResult{
		Directory:  m.Directory,
		Mode:       m.Mode,
		Missing:    []VerifyDiffEntry{},
		Extra:      []VerifyDiffEntry{},
		Different:  []VerifyDiffEntry{},
		DiskFiles:  len(disk),
		DiskBytes:  diskBytes,
		LocalFiles: len(m.Files),
		Warnings:   warnings,
	}
	local := make(map[string]VerifyDiffFile, len(m.Files))
	for _, f := range m.Files {
		local[f.Path] = f
		res.LocalBytes += f.Size
		d, ok := disk[f.Path]
		if !ok {
			res.Missing = appendVerifyDiff(res.Missing, VerifyDiffEntry{Path: f.Path, LocalSize: f.Size, Reason: "missing"}, &truncated)
			continue
		}
		if f.Size != d.size {
			res.Different = appendVerifyDiff(res.Different, VerifyDiffEntry{
				Path: f.Path, LocalSize: f.Size, DiskSize: d.size, Reason: "size"}, &truncated)
			continue
		}
		// 只有两边都算出哈希时才比哈希；本地读失败（空哈希）退化为只比大小。
		if m.Mode == "hash" && f.Hash != "" && d.hash != "" && !strings.EqualFold(f.Hash, d.hash) {
			res.Different = appendVerifyDiff(res.Different, VerifyDiffEntry{
				Path: f.Path, LocalSize: f.Size, DiskSize: d.size, Reason: "hash"}, &truncated)
			continue
		}
		res.Same++
	}
	for path, d := range disk {
		if _, ok := local[path]; ok {
			continue
		}
		res.Extra = appendVerifyDiff(res.Extra, VerifyDiffEntry{Path: path, DiskSize: d.size, Reason: "extra"}, &truncated)
	}
	sort.Slice(res.Missing, func(i, j int) bool { return res.Missing[i].Path < res.Missing[j].Path })
	sort.Slice(res.Extra, func(i, j int) bool { return res.Extra[i].Path < res.Extra[j].Path })
	sort.Slice(res.Different, func(i, j int) bool { return res.Different[i].Path < res.Different[j].Path })
	res.Truncated = truncated
	return res
}

func appendVerifyDiff(list []VerifyDiffEntry, e VerifyDiffEntry, truncated *bool) []VerifyDiffEntry {
	if len(list) >= verifyDiffMaxList {
		*truncated = true
		return list
	}
	return append(list, e)
}

func normalizeVerifyMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "hash":
		return "hash"
	default:
		return "size"
	}
}

func verifyDiffTempRoot() string {
	return filepath.Join(os.TempDir(), verifyDiffBaseDir)
}

// cleanupOldVerifyDiffRuns 清理超过 maxAge 的核对目录（旧清单/结果）。
func cleanupOldVerifyDiffRuns(maxAge time.Duration) {
	root := verifyDiffTempRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

func verifyRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

// isVerifyRootMetadata 判断路径是否属于**卷根上的系统元数据**（不属于上传内容）。
//
// 这些目录/文件由格式化或挂载自己产生（Windows 的 $RECYCLE.BIN、System Volume
// Information，Linux 的 lost+found），本地清单里不会出现。核对时若不过滤，每次都会
// 稳定多出一堆“多余”条目，掩盖真正的问题。
func isVerifyRootMetadata(rel string) bool {
	rel = filepath.ToSlash(rel)
	first := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		first = rel[:i]
	}
	switch strings.ToLower(first) {
	case "$recycle.bin", "system volume information", "lost+found":
		return true
	}
	if !strings.Contains(rel, "/") {
		switch strings.ToLower(rel) {
		case "pagefile.sys", "hiberfil.sys", "swapfile.sys", "dumpstack.log.tmp":
			return true
		}
	}
	return false
}

func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

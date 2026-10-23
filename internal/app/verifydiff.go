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
	"sync"
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
	// ProgressPath 是给界面轮询的细粒度进度文件（阶段/当前文件/已读字节）。
	ProgressPath string `json:"progress_path"`
	Mode         string `json:"mode"`
}

// VerifyDiffProgress 是核对任务的细粒度进度（GET /v1/repos/{id}/diff/progress）。
//
// 为什么不只用任务表里的 progress：百分比说明不了"现在在干什么"。核对 1000GB 时用户
// 需要看到阶段、当前文件、已处理字节，否则只能盯着一个几乎不动的数字。
type VerifyDiffProgress struct {
	// State 是 queued | running | succeeded | failed | cancelled。
	State string `json:"state"`
	// Phase 是 prepare | create | mount | walk | compare | done。
	Phase      string `json:"phase"`
	Message    string `json:"message,omitempty"`
	Current    string `json:"current,omitempty"`
	Done       int    `json:"done"`
	Total      int    `json:"total"`
	Bytes      int64  `json:"bytes"`
	TotalBytes int64  `json:"total_bytes"`
	Error      string `json:"error,omitempty"`
}

// verifyProgressInterval 限制进度文件写盘频率：4MiB 一块的读取回调非常密集，
// 每次都落盘会把磁盘打满；200ms 对界面（1s 轮询）已经足够顺滑。
const verifyProgressInterval = 200 * time.Millisecond

// verifyProgressWriter 把进度原子地写到临时 JSON 文件。
type verifyProgressWriter struct {
	path string
	mu   sync.Mutex
	last time.Time
	data VerifyDiffProgress
}

func newVerifyProgressWriter(path string) *verifyProgressWriter {
	w := &verifyProgressWriter{path: path}
	w.update(true, func(p *VerifyDiffProgress) {
		p.State = "running"
		p.Phase = "prepare"
		p.Message = "正在读取本地清单…"
	})
	return w
}

func (w *verifyProgressWriter) update(force bool, mutate func(*VerifyDiffProgress)) {
	if w == nil || w.path == "" {
		// 升级前入队的历史任务没有 progress_path：只更新内存，不落盘（也绝不能把
		// 临时文件写到进程工作目录里）。
		return
	}
	w.mu.Lock()
	mutate(&w.data)
	if !force && time.Since(w.last) < verifyProgressInterval {
		w.mu.Unlock()
		return
	}
	w.last = time.Now()
	raw, err := json.Marshal(w.data)
	w.mu.Unlock()
	if err != nil {
		return
	}
	// 先写临时文件再改名：界面随时可能来读，绝不能读到写了一半的 JSON。
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, w.path); err != nil {
		// 读侧占用导致改名失败时退化为直接覆盖：宁可偶发一次"读到半截 JSON"（读侧会
		// 忽略并等下一轮），也不能让进度永久停在旧值上。
		_ = os.WriteFile(w.path, raw, 0o600)
	}
}

func (w *verifyProgressWriter) phase(phase, message string, total int, totalBytes int64) {
	w.update(true, func(p *VerifyDiffProgress) {
		p.Phase = phase
		p.Message = message
		p.Total = total
		p.TotalBytes = totalBytes
		p.Current = ""
	})
}

func (w *verifyProgressWriter) walk(done int, bytes int64, current string) {
	w.update(false, func(p *VerifyDiffProgress) {
		p.Phase = "walk"
		p.Done = done
		p.Bytes = bytes
		p.Current = current
	})
}

func (w *verifyProgressWriter) succeed(total int) {
	w.update(true, func(p *VerifyDiffProgress) {
		p.State = "succeeded"
		p.Phase = "done"
		p.Message = "核对完成"
		p.Done = total
		p.Current = ""
	})
}

func (w *verifyProgressWriter) fail(err error) {
	w.update(true, func(p *VerifyDiffProgress) {
		p.State = "failed"
		p.Message = "核对失败"
		p.Current = ""
		p.Error = err.Error()
	})
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
		ProgressPath: filepath.Join(dir, "progress.json"),
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
	// 结果文件由任务在"返回成功"之前写出，任务状态落库在之后：这中间有一个极短的
	// 窗口，文件已经可读而任务仍显示 running。此时直接返回文件，避免界面吃到一次
	// 无意义的 409（真实反馈：核对刚结束时会弹 result_not_ready）。
	var p verifyDiffPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return nil, apperr.InvalidParam("payload").WithCause(err)
	}
	if j.State != domain.JobStateSucceeded {
		if _, statErr := os.Stat(p.ResultPath); statErr != nil {
			return nil, apperr.New("verifydiff.result_not_ready", http.StatusConflict).
				WithArg("state", string(j.State))
		}
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

// GetVerifyDiffProgress 读取核对任务的细粒度进度。
//
// 任务状态以 jobs 表为准（进度文件只负责"当前在干什么"）：文件还没写出来时返回 queued，
// 任务已失败/取消时即使文件停在 running 也返回终态。
func (s *DiskService) GetVerifyDiffProgress(ctx context.Context, repoID, jobID string) (*VerifyDiffProgress, error) {
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
	var p verifyDiffPayload
	if err := decodePayload(j.Payload, &p); err != nil {
		return nil, apperr.InvalidParam("payload").WithCause(err)
	}
	out := VerifyDiffProgress{State: "queued", Phase: "queued", Message: "等待任务开始…"}
	if raw, readErr := os.ReadFile(p.ProgressPath); readErr == nil {
		var saved VerifyDiffProgress
		if json.Unmarshal(raw, &saved) == nil {
			out = saved
		}
	}
	switch j.State {
	case domain.JobStateSucceeded:
		out.State = "succeeded"
		out.Phase = "done"
		out.Message = "核对完成"
		if out.Total > 0 {
			out.Done = out.Total
		}
		out.Current = ""
	case domain.JobStateFailed:
		out.State = "failed"
		out.Message = "核对失败"
		out.Error = j.LastError
		out.Current = ""
	case domain.JobStateCancelled:
		out.State = "cancelled"
		out.Message = "已取消"
		out.Current = ""
	default:
		if out.State == "" || out.State == "succeeded" {
			out.State = "running"
		}
	}
	return &out, nil
}

// runVerifyDiff 是 JobVerifyDiff 的实现：建临时差异盘 → 服务端挂载 → 遍历 → 对比。
func (s *DiskService) runVerifyDiff(ctx context.Context, j *domain.Job, rep job.Reporter) (err error) {
	var p verifyDiffPayload
	if err = decodePayload(j.Payload, &p); err != nil {
		return apperr.InvalidParam("payload").WithCause(err)
	}
	prog := newVerifyProgressWriter(p.ProgressPath)
	defer func() {
		if err != nil {
			prog.fail(err)
		}
	}()

	manifestRaw, readErr := os.ReadFile(p.ManifestPath)
	if readErr != nil {
		return apperr.New(apperr.CodeInternal, http.StatusInternalServerError).WithCause(readErr)
	}
	var manifest verifyDiffManifest
	if err = json.Unmarshal(manifestRaw, &manifest); err != nil {
		return apperr.InvalidParam("manifest").WithCause(err)
	}
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

	total := len(manifest.Files)
	var totalBytes int64
	for _, f := range manifest.Files {
		totalBytes += f.Size
	}

	prog.phase("create", "正在从母盘创建临时差异盘…", total, totalBytes)
	rep.Progress(3)
	ref, cleanup, err := s.createVerifyDiff(ctx, repo, parent)
	if err != nil {
		return err
	}
	defer cleanup()

	fsName := parent.FileSystemOrDefault().String()
	prog.phase("mount", "正在挂载临时差异盘…", total, totalBytes)
	rep.Progress(8)
	root, err := s.Vol.MountVolume(ctx, ref, fsName)
	if err != nil {
		return err
	}
	defer func() {
		// 用 WithoutCancel：请求/任务被取消时也要把临时卷卸掉，避免留下脏挂载。
		_ = s.Vol.UnmountVolume(context.WithoutCancel(ctx), ref, root)
	}()

	prog.phase("walk", "正在读取差异盘并逐个文件对比…", total, totalBytes)
	diskFiles, diskBytes, warnings, err := walkVerifyRoot(ctx, root, manifest.Mode, total, totalBytes, rep,
		func(done int, bytes int64, current string) {
			prog.walk(done, bytes, current)
		})
	if err != nil {
		return err
	}
	prog.phase("compare", "正在汇总差异…", total, totalBytes)
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
	prog.succeed(len(diskFiles))
	// 清单只在**成功之后**才删：任务失败会被 worker 自动重试，提前删会让第二次尝试直接
	// 报 "manifest.json: The system cannot find the file specified"，把真正的失败原因盖掉。
	_ = os.Remove(p.ManifestPath)
	rep.Progress(100)
	s.Log.Info("上传核对完成", "repo_id", repo.ID, "mode", manifest.Mode,
		"local_files", result.LocalFiles, "disk_files", result.DiskFiles,
		"missing", len(result.Missing), "extra", len(result.Extra), "different", len(result.Different))
	return nil
}

// verifyDiffNeedBytes 是创建核对用临时差异盘时要求的空闲空间。
//
// 差异盘是**稀疏快照**：初始只占元数据，核对又只读不写，实际增长几乎为零。
// 如果按母盘大小（例如 1TB）去要求空闲空间，那么"库很大、剩余空间不够再放一个整盘"
// 时核对会直接失败 —— 而这恰恰是最需要核对的场景（真实反馈：1000GB 的库核对失败）。
const verifyDiffNeedBytes int64 = 1 << 30

// createVerifyDiff 从母盘派生一块不登记的临时差异盘，返回其引用与清理函数。
func (s *DiskService) createVerifyDiff(ctx context.Context, repo *domain.Repository, parent *domain.Disk) (string, func(), error) {
	guard, err := s.Repos.pickGuardForPath(ctx, parent.VHDXPath, verifyDiffNeedBytes)
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
//
// onFile 是细粒度进度回调（已处理文件数、已读字节、当前文件）：hash 模式下每个 4MiB
// 读块都会回调一次，所以 100GB 的单文件也能看到字节在走，而不是卡住不动。
func walkVerifyRoot(ctx context.Context, root, mode string, total int, totalBytes int64, rep job.Reporter,
	onFile func(done int, bytes int64, current string)) (
	map[string]verifyDiskFile, int64, []string, error) {
	files := make(map[string]verifyDiskFile, total)
	warnings := make([]string, 0)
	var diskBytes int64
	var processed int64
	seen := 0
	lastPercent := -1
	report := func() {
		if totalBytes > 0 {
			percent := 8 + int(86*processed/totalBytes)
			if percent > 94 {
				percent = 94
			}
			if percent != lastPercent {
				lastPercent = percent
				rep.Progress(percent)
			}
			return
		}
		if total > 0 {
			percent := 8 + 86*seen/total
			if percent > 94 {
				percent = 94
			}
			if percent != lastPercent {
				lastPercent = percent
				rep.Progress(percent)
			}
		}
	}
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
		before := processed
		if mode == "hash" {
			h, hErr := hashFileSHA256Progress(p, func(n int64) {
				processed += n
				if onFile != nil {
					onFile(seen, processed, rel)
				}
				report()
			})
			if hErr != nil {
				warnings = append(warnings, "hash_failed:"+rel)
			} else {
				hash = h
			}
		}
		// 读失败时也按文件大小推进（避免进度卡死）；正常情况下这里只是把"已读字节"
		// 对齐到文件大小，防止回调计数与 stat 结果有差异。
		processed = before + info.Size()
		files[rel] = verifyDiskFile{size: info.Size(), hash: hash}
		diskBytes += info.Size()
		seen++
		if onFile != nil {
			onFile(seen, processed, rel)
		}
		report()
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

// verifyHashBufferBytes 是服务端算哈希时的读块大小（与代理侧同量级）。
const verifyHashBufferBytes = 4 << 20

// hashFileSHA256Progress 计算 SHA-256，并在每个读块后回调已读字节数。
func hashFileSHA256Progress(path string, onRead func(int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, verifyHashBufferBytes)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, err := h.Write(buf[:n]); err != nil {
				return "", err
			}
			if onRead != nil {
				onRead(int64(n))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

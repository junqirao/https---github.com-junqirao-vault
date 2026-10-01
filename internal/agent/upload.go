package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	"vault/internal/apperr"
)

// 分块上传建库的参数（见 docs/agent-api.md「本地目录扫描与上传建库」）。
const (
	// uploadChunkSize 分块大小，必须与服务端 app.DefaultUploadChunkSize（8MiB）一致。
	uploadChunkSize int64 = 8 << 20
	// uploadChunkConcurrency 并发分块数（服务端按 IP 限流 60r/s、突发 300）。
	uploadChunkConcurrency = 4
	// uploadMaxAttempts 单块最大尝试次数（含首次）。
	uploadMaxAttempts = 5
	// uploadJobPollInterval 建库任务轮询间隔。
	uploadJobPollInterval = 2 * time.Second
	// uploadChunkTimeout 单次分块 PUT 的超时（8MiB 在慢链路上可能较久）。
	uploadChunkTimeout = 5 * time.Minute
	// uploadsFileName 续传记录文件名（位于代理数据目录）。
	uploadsFileName = "uploads.json"
	// serverUploadNotFoundCode 服务端"上传会话不存在"错误码。
	serverUploadNotFoundCode = "upload.not_found"
)

// uploadRetryDelays 各次重试前的退避时长（第 k 次重试用下标 k-1）。
var uploadRetryDelays = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
}

// uploadBufPool 复用 8MiB 分块缓冲：并发上传时各 worker 各取一个，避免重复分配。
var uploadBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, uploadChunkSize)
		return &b
	},
}

// 上传任务状态（见 docs/agent-api.md「本地目录扫描与上传建库」）。
const (
	// UploadStateScanning 正在扫描本地目录。
	UploadStateScanning = "scanning"
	// UploadStateCreating 正在创建服务端上传会话。
	UploadStateCreating = "creating"
	// UploadStateUploading 正在上传分块。
	UploadStateUploading = "uploading"
	// UploadStateCompleting 已提交建库，等待服务端任务完成。
	UploadStateCompleting = "completing"
	// UploadStateDone 建库完成。
	UploadStateDone = "done"
	// UploadStateFailed 失败（error 为稳定错误码）。
	UploadStateFailed = "failed"
	// UploadStateCanceled 已取消（服务端会话与已传分块按语义保留或放弃，见文档）。
	UploadStateCanceled = "canceled"
)

// UploadState 是单个"本地目录 → 存储库"上传任务的状态（对外契约类型）。
type UploadState struct {
	UploadID      string `json:"upload_id"`
	RepoName      string `json:"repo_name"`
	LocalDir      string `json:"local_dir"`
	State         string `json:"state"`
	TotalFiles    int    `json:"total_files"`
	TotalBytes    int64  `json:"total_bytes"`
	UploadedBytes int64  `json:"uploaded_bytes"`
	ChunkTotal    int    `json:"chunk_total"`
	ChunkDone     int    `json:"chunk_done"`
	JobID         string `json:"job_id,omitempty"`
	RepoID        string `json:"repo_id,omitempty"`
	Error         string `json:"error,omitempty"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at,omitempty"`
}

// uploadStartRequest 是 POST /agent/uploads/start 的请求体。
type uploadStartRequest struct {
	LocalDir string `json:"local_dir"`
	RepoName string `json:"repo_name"`
	// RepoMode shared | exclusive；留空默认 shared。
	RepoMode string `json:"repo_mode"`
	// StorageID 可选目标存储 ID；留空自动选。
	StorageID string `json:"storage_id"`
	// QuotaBytes 应用层配额；0=不限。
	QuotaBytes int64 `json:"quota_bytes"`
	// SourceMode copy | move；留空默认 copy。move 仅在服务端建库成功后删除本地源目录。
	SourceMode string `json:"source_mode"`
}

// uploadSessionPlan 是一次上传要使用的服务端会话计划。
type uploadSessionPlan struct {
	uploadID   string
	totalBytes int64
	missing    []int
	reused     bool
}

// ---- 任务管理（内存态 + 续传记录） ----

// uploadManager 管理本机的上传任务，并维护跨重启的续传记录。
type uploadManager struct {
	mu     sync.Mutex
	tasks  map[string]*uploadTask
	resume *uploadResumeStore
}

// uploadTask 是单个上传任务的运行时状态。
type uploadTask struct {
	mu     sync.Mutex
	state  UploadState
	cancel context.CancelFunc
	// abort 为 true 表示"显式取消"（DELETE 接口）：需尽力调用服务端 DELETE 放弃会话并删除续传记录。
	abort bool
	// submitted 为 true 表示"完成建库任务已提交"（job 可能已在跑）：
	// 此时**不得**再调用服务端 DELETE（会删掉正在被 job 使用的暂存目录）。
	submitted bool
}

func newUploadManager(resumePath string) *uploadManager {
	return &uploadManager{
		tasks:  make(map[string]*uploadTask),
		resume: newUploadResumeStore(resumePath),
	}
}

func (t *uploadTask) snapshot() UploadState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *uploadTask) update(mutate func(*UploadState)) UploadState {
	t.mu.Lock()
	defer t.mu.Unlock()
	mutate(&t.state)
	return t.state
}

func (t *uploadTask) setCancel(cancel context.CancelFunc) {
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()
}

// cancelTask 取消进行中的任务；explicit 表示由 DELETE 接口触发。
func (t *uploadTask) cancelTask(explicit bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !isActiveUploadState(t.state.State) || t.cancel == nil {
		return false
	}
	if explicit {
		t.abort = true
	}
	t.cancel()
	return true
}

// aborted 返回是否为"显式取消"。
func (t *uploadTask) aborted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.abort
}

// markSubmitted 标记"完成建库任务已提交"。
func (t *uploadTask) markSubmitted() {
	t.mu.Lock()
	t.submitted = true
	t.mu.Unlock()
}

// wasSubmitted 返回完成建库任务是否已提交。
func (t *uploadTask) wasSubmitted() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.submitted
}

// reserve 为 key（小写 local_dir）预留任务槽位：已有进行中的任务时返回该任务与 false。
func (m *uploadManager) reserve(key string, initial UploadState) (*uploadTask, UploadState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.tasks[key]; ok {
		if snap := existing.snapshot(); isActiveUploadState(snap.State) {
			return existing, snap, false
		}
	}
	t := &uploadTask{state: initial}
	m.tasks[key] = t
	return t, initial, true
}

// findByLocalDir 查找同一 local_dir 上进行中的任务（大小写不敏感）。
func (m *uploadManager) findByLocalDir(dir string) (*uploadTask, bool) {
	m.mu.Lock()
	tasks := make([]*uploadTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()
	for _, t := range tasks {
		if snap := t.snapshot(); strings.EqualFold(snap.LocalDir, dir) && isActiveUploadState(snap.State) {
			return t, true
		}
	}
	return nil, false
}

// cancel 取消指定 upload_id 进行中的任务；不存在或非进行中返回 false。
func (m *uploadManager) cancel(uploadID string) bool {
	m.mu.Lock()
	tasks := make([]*uploadTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()
	for _, t := range tasks {
		if strings.EqualFold(t.snapshot().UploadID, uploadID) {
			return t.cancelTask(true)
		}
	}
	return false
}

// list 返回全部上传状态（按开始时间倒序）。
func (m *uploadManager) list() []UploadState {
	m.mu.Lock()
	tasks := make([]*uploadTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()
	out := make([]UploadState, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// uploadResumeRecord 是持久化的续传记录（同一 local_dir + repo_name 唯一）。
type uploadResumeRecord struct {
	UploadID   string `json:"upload_id"`
	LocalDir   string `json:"local_dir"`
	RepoName   string `json:"repo_name"`
	TotalBytes int64  `json:"total_bytes"`
	TotalFiles int    `json:"total_files"`
	StartedAt  int64  `json:"started_at"`
	RepoMode   string `json:"repo_mode,omitempty"`
	StorageID  string `json:"storage_id,omitempty"`
	QuotaBytes int64  `json:"quota_bytes,omitempty"`
	SourceMode string `json:"source_mode,omitempty"`
}

// uploadResumeStore 负责续传记录的读写（<DataDir>/uploads.json，原子替换写盘）。
type uploadResumeStore struct {
	path string

	mu      sync.Mutex
	records []uploadResumeRecord
}

func newUploadResumeStore(path string) *uploadResumeStore {
	s := &uploadResumeStore{path: path}
	if data, err := os.ReadFile(path); err == nil {
		var recs []uploadResumeRecord
		if json.Unmarshal(data, &recs) == nil {
			s.records = recs
		}
	}
	return s
}

func (s *uploadResumeStore) find(dir, repoName string) (uploadResumeRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if strings.EqualFold(r.LocalDir, dir) && r.RepoName == repoName {
			return r, true
		}
	}
	return uploadResumeRecord{}, false
}

// save 写入/覆盖一条续传记录并落盘。
func (s *uploadResumeStore) save(rec uploadResumeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	replaced := false
	for i := range s.records {
		if strings.EqualFold(s.records[i].LocalDir, rec.LocalDir) && s.records[i].RepoName == rec.RepoName {
			s.records[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		s.records = append(s.records, rec)
	}
	return s.persistLocked()
}

// remove 删除一条续传记录并落盘（幂等）。
func (s *uploadResumeStore) remove(dir, repoName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.records[:0]
	for _, r := range s.records {
		if strings.EqualFold(r.LocalDir, dir) && r.RepoName == repoName {
			continue
		}
		out = append(out, r)
	}
	s.records = out
	return s.persistLocked()
}

// persistLocked 原子替换式写盘。调用方需持有锁。
func (s *uploadResumeStore) persistLocked() error {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	to, err := windows.UTF16PtrFromString(s.path)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ---- 本地接口 ----

// handleUploadStart 启动一个"本地目录 → 存储库"的后台上传（异步，立即返回 202）。
func (a *Agent) handleUploadStart(w http.ResponseWriter, r *http.Request) {
	var req uploadStartRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, err)
		return
	}
	dir, err := validateAbsDir(req.LocalDir, "local_dir")
	if err != nil {
		a.writeError(w, err)
		return
	}
	repoName := strings.TrimSpace(req.RepoName)
	if repoName == "" {
		a.writeError(w, apperr.InvalidParam("repo_name"))
		return
	}
	mode, err := normalizeUploadRepoMode(req.RepoMode)
	if err != nil {
		a.writeError(w, err)
		return
	}
	sourceMode, err := normalizeUploadSourceMode(req.SourceMode)
	if err != nil {
		a.writeError(w, err)
		return
	}
	if req.QuotaBytes < 0 {
		a.writeError(w, apperr.InvalidParam("quota_bytes"))
		return
	}
	req.RepoMode = mode
	req.SourceMode = sourceMode
	req.StorageID = strings.TrimSpace(req.StorageID)

	// 无会话或会话不可用时明确报错，绝不静默失败。
	client, err := a.serverClient()
	if err != nil {
		a.writeError(w, err)
		return
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		a.writeError(w, apperr.InvalidParam("local_dir"))
		return
	}

	// 去重：同一 local_dir 同时只允许一个进行中的上传。
	if existing, ok := a.uploads.findByLocalDir(dir); ok {
		a.writeJSON(w, http.StatusAccepted, map[string]any{"upload": existing.snapshot()})
		return
	}

	initial := UploadState{
		LocalDir:  dir,
		RepoName:  repoName,
		State:     UploadStateScanning,
		StartedAt: time.Now().UnixMilli(),
	}
	task, state, created := a.uploads.reserve(strings.ToLower(dir), initial)
	if !created {
		a.writeJSON(w, http.StatusAccepted, map[string]any{"upload": state})
		return
	}
	ctx, cancel := context.WithCancel(a.bgContext())
	task.setCancel(cancel)

	a.logger.Info("开始上传本地目录并建库", "local_dir", dir, "repo_name", repoName, "source_mode", sourceMode)
	a.hub.Publish(Event{Type: "upload", Data: state})
	safeGo(a.logger, "upload_run", func() { a.runUpload(ctx, task, client, req, dir) })
	a.writeJSON(w, http.StatusAccepted, map[string]any{"upload": state})
}

// handleListUploads 列出全部上传任务。
func (a *Agent) handleListUploads(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]any{"items": a.uploads.list()})
}

// handleCancelUpload 取消指定 upload_id 的上传（幂等：不存在也返回 ok）。
//
// 语义：停止本地上传并**尽力**调用服务端 DELETE /v1/uploads/{id} 放弃会话（清理服务端暂存），
// 同时删除本地续传记录；已传分块在服务端被放弃后不再可续传。
func (a *Agent) handleCancelUpload(w http.ResponseWriter, r *http.Request) {
	uploadID := strings.TrimSpace(r.PathValue("upload_id"))
	if uploadID == "" {
		a.writeError(w, apperr.InvalidParam("upload_id"))
		return
	}
	if a.uploads.cancel(uploadID) {
		a.logger.Info("已请求取消上传", "upload_id", uploadID)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 上传执行 ----

// runUpload 在后台执行：扫描 → 建/续会话 → 并发分块 → 完成建库 → 轮询任务 → 终态。
func (a *Agent) runUpload(ctx context.Context, task *uploadTask, client *serverClient, req uploadStartRequest, dir string) {
	scan, err := scanLocalDir(dir)
	if err != nil {
		a.failOrCancelUpload(ctx, client, task, dir, req.RepoName, "", err)
		return
	}
	task.update(func(s *UploadState) {
		s.TotalFiles = scan.TotalFiles
		s.TotalBytes = scan.TotalBytes
	})
	a.publishUpload(task)
	a.logger.Info("本地目录扫描完成", "local_dir", dir, "file_count", scan.TotalFiles, "total_bytes", scan.TotalBytes)

	plan, err := a.ensureUploadSession(ctx, client, task, req, dir, scan)
	if err != nil {
		a.failOrCancelUpload(ctx, client, task, dir, req.RepoName, currentUploadID(task), err)
		return
	}

	if err := a.uploadChunks(ctx, client, task, plan, scan); err != nil {
		a.failOrCancelUpload(ctx, client, task, dir, req.RepoName, plan.uploadID, err)
		return
	}

	if err := a.completeUpload(ctx, client, task, plan, dir, req.RepoName, req.SourceMode); err != nil {
		a.failOrCancelUpload(ctx, client, task, dir, req.RepoName, plan.uploadID, err)
		return
	}
}

// ensureUploadSession 复用同一 local_dir + repo_name 的未完成会话，否则新建。
func (a *Agent) ensureUploadSession(ctx context.Context, client *serverClient, task *uploadTask, req uploadStartRequest, dir string, scan *scanResult) (*uploadSessionPlan, error) {
	if rec, ok := a.uploads.resume.find(dir, req.RepoName); ok {
		st, err := client.UploadStatus(ctx, rec.UploadID)
		switch {
		case err == nil:
			if !isTerminalServerUploadState(st.State) && st.TotalBytes == scan.TotalBytes && st.TotalFiles == scan.TotalFiles {
				a.logger.Info("复用未完成上传会话，从缺失分块续传",
					"upload_id", rec.UploadID, "missing_chunks", len(st.MissingChunks))
				task.update(func(s *UploadState) { s.UploadID = rec.UploadID })
				return &uploadSessionPlan{uploadID: rec.UploadID, totalBytes: st.TotalBytes, missing: st.MissingChunks, reused: true}, nil
			}
			// 会话已终态，或与本地现有内容不一致：丢弃记录并重建会话。
			a.logger.Info("已有上传会话不可复用，重新创建",
				"upload_id", rec.UploadID, "state", st.State,
				"server_bytes", st.TotalBytes, "local_bytes", scan.TotalBytes)
			_ = a.uploads.resume.remove(dir, req.RepoName)
		case apperr.CodeOf(err) == serverUploadNotFoundCode:
			_ = a.uploads.resume.remove(dir, req.RepoName)
		default:
			// 网络等非确定性错误：明确失败，不静默新建（避免重复占用服务端暂存）。
			return nil, err
		}
	}

	task.update(func(s *UploadState) { s.State = UploadStateCreating })
	a.publishUpload(task)

	manifest := uploadManifest{Root: sanitizeScanRootBase(scan.Root)}
	manifest.Files = make([]uploadManifestEntry, 0, len(scan.Entries))
	for _, e := range scan.Entries {
		manifest.Files = append(manifest.Files, uploadManifestEntry{Path: e.Rel, Size: e.Size, Mtime: e.Mtime})
	}
	sess, err := client.CreateUpload(ctx, createUploadRequest{
		RepoName:   req.RepoName,
		Mode:       req.SourceMode,
		Manifest:   manifest,
		RepoMode:   req.RepoMode,
		StorageID:  req.StorageID,
		QuotaBytes: req.QuotaBytes,
	})
	if err != nil {
		return nil, err
	}
	if sess.ChunkSize != 0 && sess.ChunkSize != uploadChunkSize {
		return nil, errUploadFailed("chunk_size", fmt.Errorf("服务端分块大小 %d 与代理预期 %d 不一致", sess.ChunkSize, uploadChunkSize))
	}
	task.update(func(s *UploadState) { s.UploadID = sess.UploadID })
	_ = a.uploads.resume.save(uploadResumeRecord{
		UploadID:   sess.UploadID,
		LocalDir:   dir,
		RepoName:   req.RepoName,
		TotalBytes: scan.TotalBytes,
		TotalFiles: scan.TotalFiles,
		StartedAt:  time.Now().UnixMilli(),
		RepoMode:   req.RepoMode,
		StorageID:  req.StorageID,
		QuotaBytes: req.QuotaBytes,
		SourceMode: req.SourceMode,
	})
	a.logger.Info("已创建上传会话", "upload_id", sess.UploadID, "total_bytes", sess.TotalBytes, "missing_chunks", len(sess.MissingChunks))
	return &uploadSessionPlan{uploadID: sess.UploadID, totalBytes: sess.TotalBytes, missing: sess.MissingChunks}, nil
}

// uploadChunks 并发上传 missing_chunks（4 路），每块独立重试与退避。
func (a *Agent) uploadChunks(ctx context.Context, client *serverClient, task *uploadTask, plan *uploadSessionPlan, scan *scanResult) error {
	total := plan.totalBytes
	chunkTotal := uploadChunkCount(total, uploadChunkSize)
	var missingBytes int64
	for _, idx := range plan.missing {
		missingBytes += uploadChunkSizeAt(idx, total, uploadChunkSize)
	}
	done := chunkTotal - len(plan.missing)
	prog := newUploadProgress(a, task)
	task.update(func(s *UploadState) {
		s.State = UploadStateUploading
		s.ChunkTotal = chunkTotal
	})
	prog.reset(total, total-missingBytes, done)

	if len(plan.missing) == 0 {
		return nil
	}
	planFiles := buildUploadPlan(scan.Entries)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan int)
	var (
		wg      sync.WaitGroup
		errOnce sync.Once
		runErr  error
	)
	fail := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() {
			runErr = err
			cancel()
		})
	}

	for i := 0; i < uploadChunkConcurrency; i++ {
		wg.Add(1)
		safeGo(a.logger, "upload_chunk", func() {
			defer wg.Done()
			bp := uploadBufPool.Get().(*[]byte)
			defer uploadBufPool.Put(bp)
			buf := *bp
			for idx := range work {
				if runCtx.Err() != nil {
					return
				}
				size := uploadChunkSizeAt(idx, total, uploadChunkSize)
				if size <= 0 {
					continue
				}
				if err := readUploadChunk(planFiles, int64(idx)*uploadChunkSize, buf[:size]); err != nil {
					fail(errUploadFailed("read_local", err))
					return
				}
				if err := a.putUploadChunkWithRetry(runCtx, client, plan.uploadID, idx, buf[:size]); err != nil {
					fail(err)
					return
				}
				prog.addChunk(size)
			}
		})
	}

sendLoop:
	for _, idx := range plan.missing {
		select {
		case <-runCtx.Done():
			break sendLoop
		case work <- idx:
		}
	}
	close(work)
	wg.Wait()

	if runErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return runErr
	}
	return nil
}

// putUploadChunkWithRetry 上传单个分块：计算 SHA256 → PUT → 失败按退避重试。
//
// 可重试：429 / 5xx / 网络错误 / 读超时；不可重试：4xx 确定性错误与上下文取消。
func (a *Agent) putUploadChunkWithRetry(ctx context.Context, client *serverClient, uploadID string, index int, body []byte) error {
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	var lastErr error
	for attempt := 1; attempt <= uploadMaxAttempts; attempt++ {
		if attempt > 1 {
			delay := uploadRetryDelay(attempt - 1)
			a.logger.Warn("分块上传失败，准备重试",
				"upload_id", uploadID, "index", index, "attempt", attempt, "max", uploadMaxAttempts,
				"delay", delay.String(), "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		reqCtx, cancel := context.WithTimeout(ctx, uploadChunkTimeout)
		err := client.PutUploadChunk(reqCtx, uploadID, index, sha, body)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isRetryableUploadError(err) {
			return err
		}
		lastErr = err
	}
	return errUploadFailed("attempts_exhausted", lastErr)
}

// completeUpload 提交建库任务并轮询至终态；成功后记录 repo_id 并按 move 语义清理本地源目录。
func (a *Agent) completeUpload(ctx context.Context, client *serverClient, task *uploadTask, plan *uploadSessionPlan, dir, repoName, sourceMode string) error {
	task.update(func(s *UploadState) { s.State = UploadStateCompleting })
	a.publishUpload(task)

	jobID, err := client.CompleteUpload(ctx, plan.uploadID)
	if err != nil {
		return err
	}
	task.markSubmitted()
	task.update(func(s *UploadState) { s.JobID = jobID })
	a.publishUpload(task)
	a.logger.Info("已提交建库任务，等待完成", "upload_id", plan.uploadID, "job_id", jobID)

	job, err := a.waitUploadJob(ctx, client, jobID)
	if err != nil {
		return err
	}
	if job.State != "succeeded" {
		return errUploadFailed("job", fmt.Errorf("建库任务状态: %s", job.State))
	}

	// repo_id：从仓库列表按名称反查（拿不到不影响建库结果，仅缺失该字段）。
	if id, rErr := client.FindRepoID(ctx, repoName); rErr == nil {
		if id != "" {
			task.update(func(s *UploadState) { s.RepoID = id })
		}
	} else {
		a.logger.Warn("查询存储库 ID 失败（不影响建库结果）", "repo_name", repoName, "error", rErr)
	}

	// move 语义：仅在服务端任务**成功**后删除本地源目录；失败/取消绝不删。
	if sourceMode == "move" {
		a.logger.Info("服务端建库成功，删除本地源目录（move 模式）", "local_dir", dir)
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			a.logger.Warn("删除本地源目录失败", "local_dir", dir, "error", rmErr)
		}
	}
	_ = a.uploads.resume.remove(dir, repoName)

	now := time.Now().UnixMilli()
	state := task.update(func(s *UploadState) {
		s.State = UploadStateDone
		s.UploadedBytes = s.TotalBytes
		s.FinishedAt = now
	})
	a.hub.Publish(Event{Type: "upload", Data: state})
	a.logger.Info("本地目录上传建库完成", "local_dir", dir, "repo_name", repoName, "repo_id", state.RepoID)
	return nil
}

// waitUploadJob 轮询 GET /v1/jobs/{id} 直到终态。
func (a *Agent) waitUploadJob(ctx context.Context, client *serverClient, jobID string) (*serverJob, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, errUploadFailed("complete", errors.New("服务端未返回 job_id"))
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		j, err := client.Job(ctx, jobID)
		if err != nil {
			return nil, err
		}
		switch j.State {
		case "succeeded", "failed", "cancelled":
			return j, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(uploadJobPollInterval):
		}
	}
}

// failOrCancelUpload 统一处理失败/取消：上下文取消走取消路径，其余置 failed 终态。
func (a *Agent) failOrCancelUpload(ctx context.Context, client *serverClient, task *uploadTask, dir, repoName, uploadID string, err error) {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		a.cancelUploadTask(client, task, dir, repoName, uploadID)
		return
	}
	a.finishUpload(task, UploadStateFailed, apperr.CodeOf(err))
	a.logger.Warn("本地目录上传建库失败", "local_dir", dir, "repo_name", repoName, "error", err)
}

// cancelUploadTask 置 canceled 终态。
//
// 显式取消（DELETE）：尽力调用服务端 DELETE 放弃会话并删除续传记录；
// 非显式取消（如进程退出）：保留服务端会话与已传分块，续传记录保留，下次可继续。
func (a *Agent) cancelUploadTask(client *serverClient, task *uploadTask, dir, repoName, uploadID string) {
	if uploadID == "" {
		uploadID = currentUploadID(task)
	}
	if task.aborted() {
		// 仅在"完成建库任务尚未提交"时才放弃服务端会话：
		// job 已在跑时删掉暂存目录会破坏建库，因此此时只停止客户端跟踪（服务端任务自行结束）。
		if uploadID != "" && client != nil && !task.wasSubmitted() {
			ctx, cancel := context.WithTimeout(a.bgContext(), 15*time.Second)
			if err := client.AbortUpload(ctx, uploadID); err != nil {
				a.logger.Warn("放弃服务端上传会话失败（服务端将在 24h 后 GC 清理）", "upload_id", uploadID, "error", err)
			}
			cancel()
		}
		_ = a.uploads.resume.remove(dir, repoName)
	}
	now := time.Now().UnixMilli()
	state := task.update(func(s *UploadState) {
		s.State = UploadStateCanceled
		s.Error = ""
		s.FinishedAt = now
	})
	a.hub.Publish(Event{Type: "upload", Data: state})
	a.logger.Info("本地目录上传已取消", "local_dir", dir, "upload_id", uploadID)
}

// finishUpload 置终态并推送 upload 事件。
func (a *Agent) finishUpload(task *uploadTask, state, errCode string) {
	now := time.Now().UnixMilli()
	snap := task.update(func(s *UploadState) {
		s.State = state
		s.Error = errCode
		s.FinishedAt = now
	})
	a.hub.Publish(Event{Type: "upload", Data: snap})
}

// publishUpload 推送当前上传状态（不做节流；用于状态跃迁）。
func (a *Agent) publishUpload(task *uploadTask) {
	a.hub.Publish(Event{Type: "upload", Data: task.snapshot()})
}

// currentUploadID 读取任务当前的 upload_id（会话尚未创建时为空）。
func currentUploadID(task *uploadTask) string {
	if task == nil {
		return ""
	}
	return task.snapshot().UploadID
}

// ---- 进度 ----

// uploadProgress 汇总上传进度：uploaded 为已完成分块字节之和，统一经 progressThrottle
// 节流后才推送 SSE，避免多 worker 并发推送与高频率刷屏。
type uploadProgress struct {
	a    *Agent
	task *uploadTask

	total      atomic.Int64
	uploaded   atomic.Int64
	chunksDone atomic.Int64

	mu       sync.Mutex
	throttle progressThrottle
}

func newUploadProgress(a *Agent, task *uploadTask) *uploadProgress {
	return &uploadProgress{a: a, task: task, throttle: newProgressThrottle(0)}
}

// reset 重置进度基线并立即推送一次。
func (p *uploadProgress) reset(total, uploaded int64, done int) {
	p.total.Store(total)
	p.uploaded.Store(uploaded)
	p.chunksDone.Store(int64(done))
	p.push(true)
}

// addChunk 记录一个分块完成。
func (p *uploadProgress) addChunk(size int64) {
	p.uploaded.Add(size)
	p.chunksDone.Add(1)
	p.push(false)
}

// push 在节流允许时推送进度。
func (p *uploadProgress) push(force bool) {
	uploaded := p.uploaded.Load()
	p.mu.Lock()
	ok := p.throttle.allow(force, uploaded, time.Now())
	p.mu.Unlock()
	if !ok {
		return
	}

	done := p.chunksDone.Load()
	total := p.total.Load()
	state := p.task.update(func(s *UploadState) {
		s.TotalBytes = total
		s.UploadedBytes = uploaded
		s.ChunkDone = int(done)
	})
	p.a.hub.Publish(Event{Type: "upload", Data: state})
}

// ---- 纯函数辅助 ----

// readUploadChunk 按**全局偏移量**从文件计划中读取 [start, start+len(buf)) 区间
// （可能跨多个文件）填充到 buf。
func readUploadChunk(plan []uploadPlanFile, start int64, buf []byte) error {
	need := len(buf)
	cur := start
	out := 0
	for need > 0 {
		i := sort.Search(len(plan), func(i int) bool { return plan[i].Offset+plan[i].Size > cur })
		if i >= len(plan) {
			return io.ErrUnexpectedEOF
		}
		f := plan[i]
		off := cur - f.Offset
		if off < 0 {
			off = 0
		}
		n := int64(need)
		if rem := f.Size - off; rem < n {
			n = rem
		}
		if n <= 0 {
			return io.ErrUnexpectedEOF
		}
		fh, err := os.Open(f.Abs)
		if err != nil {
			return err
		}
		var readErr error
		if _, seekErr := fh.Seek(off, io.SeekStart); seekErr != nil {
			readErr = seekErr
		} else {
			_, readErr = io.ReadFull(fh, buf[out:out+int(n)])
		}
		_ = fh.Close()
		if readErr != nil {
			return readErr
		}
		out += int(n)
		need -= int(n)
		cur += n
	}
	return nil
}

// uploadChunkCount 计算分块总数。
func uploadChunkCount(total, chunkSize int64) int {
	if total <= 0 || chunkSize <= 0 {
		return 0
	}
	return int((total + chunkSize - 1) / chunkSize)
}

// uploadChunkSizeAt 返回指定索引分块的字节数。
func uploadChunkSizeAt(index int, total, chunkSize int64) int64 {
	if total <= 0 || chunkSize <= 0 || index < 0 {
		return 0
	}
	offset := int64(index) * chunkSize
	if remaining := total - offset; remaining < chunkSize {
		return remaining
	}
	return chunkSize
}

// uploadRetryDelay 返回第 retry 次重试（1-based）前的退避时长。
func uploadRetryDelay(retry int) time.Duration {
	if retry < 1 {
		retry = 1
	}
	if retry > len(uploadRetryDelays) {
		retry = len(uploadRetryDelays)
	}
	return uploadRetryDelays[retry-1]
}

// isRetryableUploadError 判断分块上传错误是否值得重试。
//
// 可重试：429 / 5xx / 网络不可达 / 读超时（服务端慢）。
// 不重试：4xx 确定性错误与上下文取消。
func isRetryableUploadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if e, ok := apperr.As(err); ok {
		switch {
		case e.Code == CodeServerUnreachable:
			return true
		case e.HTTP == http.StatusTooManyRequests:
			return true
		case e.HTTP >= 500:
			return true
		default:
			return false
		}
	}
	// 非结构化错误（网络/本地 IO 中断）：重试通常可恢复。
	return true
}

// isActiveUploadState 判断状态是否为"进行中"。
func isActiveUploadState(state string) bool {
	switch state {
	case UploadStateScanning, UploadStateCreating, UploadStateUploading, UploadStateCompleting:
		return true
	}
	return false
}

// isTerminalServerUploadState 判断服务端上传会话是否已终态。
func isTerminalServerUploadState(state string) bool {
	return state == "complete" || state == "aborted"
}

// normalizeUploadRepoMode 归一化建库模式（留空默认 shared）。
func normalizeUploadRepoMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "shared":
		return "shared", nil
	case "exclusive":
		return "exclusive", nil
	}
	return "", apperr.InvalidParam("repo_mode")
}

// normalizeUploadSourceMode 归一化源目录处理模式（留空默认 copy）。
func normalizeUploadSourceMode(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "copy":
		return "copy", nil
	case "move":
		return "move", nil
	}
	return "", apperr.InvalidParam("source_mode")
}

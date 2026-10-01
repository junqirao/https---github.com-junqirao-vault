package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	"vault/internal/apperr"
)

// 磁盘内容下载相关常量。
const (
	// downloadPartSuffix 下载中的临时文件后缀（完成后改名去掉它）。
	downloadPartSuffix = ".part"
	// downloadPartMetaSuffix 断点元数据后缀（附在 .part 之后），见 downloadPartMeta。
	downloadPartMetaSuffix = ".meta"
	// downloadPartMetaVersion 断点元数据格式版本；版本不符则视为不可用（从头下载）。
	downloadPartMetaVersion = 1
	// downloadProbeTimeout 解析远端文件名/大小的探测超时（只读到响应头）。
	downloadProbeTimeout = 15 * time.Second
	// downloadContentPathPrefix 服务端母盘内容下载路径前缀。
	downloadContentPathPrefix = "/v1/disks/"

	// downloadCopyBufferBytes 单次拷贝使用的缓冲区大小（1 MiB）。
	// Go 默认 32 KiB 在高带宽链路上 syscall/CPU 开销明显，固定 1 MiB 后吞吐显著提升。
	downloadCopyBufferBytes = 1 << 20

	// downloadDefaultConnections 分段并发默认值。
	downloadDefaultConnections = 4
	// downloadMaxConnections 分段并发上限。
	downloadMaxConnections = 8
	// downloadSegmentedMinBytes 触发分段下载的最小总大小（小于它收益不抵开销）。
	downloadSegmentedMinBytes = 64 << 20

	// downloadMaxAttempts 单次下载任务的最大尝试次数（含首次）。
	downloadMaxAttempts = 5

	// downloadSpaceReserveFloor 空间预检的最小预留（1 GiB）。
	downloadSpaceReserveFloor = 1 << 30
	// downloadSpaceReserveRatio 空间预检预留比例的分母（total/20 == 5%）。
	downloadSpaceReserveRatio = 20
)

// downloadRetryDelays 各次重试前的退避时长（第 k 次重试用下标 k-1）。
// 最多 5 次尝试 → 最多 4 次退避，因此实际用到 1s/2s/4s/8s；16s 为上限预留。
var downloadRetryDelays = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
}

// downloadCopyPool 复用 1 MiB 拷贝缓冲：分段并发时各 goroutine 各取一个，
// 既避免每次拷贝重复分配，又不会让多个 goroutine 共享同一块内存（数据竞争）。
var downloadCopyPool = sync.Pool{
	New: func() any { return make([]byte, downloadCopyBufferBytes) },
}

// 下载过程中使用的哨兵错误（在 downloadAttempt 中统一翻译为稳定错误码）。
var (
	// errDownloadResetNeeded 需丢弃本地断点后从头下载：
	// 远端母盘已变更（If-Range 未命中 / 总长变化），或本地断点状态不可信。
	errDownloadResetNeeded = errors.New("agent: 需重置本地下载断点")
	// errDownloadRangeNotSatisfiable 服务端返回 416（Range 不满足）：本地前缀超出远端，确定性失败。
	errDownloadRangeNotSatisfiable = errors.New("agent: Range 不满足")
	// errDownloadDiskFull 写盘时命中 ENOSPC（映射为 agent.insufficient_local_space）。
	errDownloadDiskFull = errors.New("agent: 目标卷空间不足")
	// errDownloadSizeMismatch 收到的字节数与预期不符（确定性损坏，删除断点后失败）。
	errDownloadSizeMismatch = errors.New("agent: 下载字节数与预期不符")
)

// 下载任务状态（见 docs/agent-api.md「磁盘内容下载」）。
const (
	// DownloadStateRunning 下载进行中。
	DownloadStateRunning = "running"
	// DownloadStateDone 下载并校验完成，文件已落地。
	DownloadStateDone = "done"
	// DownloadStateFailed 下载失败（error 为稳定错误码）。
	DownloadStateFailed = "failed"
	// DownloadStateCanceled 下载被取消（保留 .part 供续传）。
	DownloadStateCanceled = "canceled"
)

// DownloadState 是单个磁盘下载任务的状态（对外契约类型）。
type DownloadState struct {
	DiskID        string `json:"disk_id"`
	FileName      string `json:"file_name"`
	TargetPath    string `json:"target_path"`
	State         string `json:"state"`
	ReceivedBytes int64  `json:"received_bytes"`
	TotalBytes    int64  `json:"total_bytes"`
	Error         string `json:"error,omitempty"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at,omitempty"`
}

// diskDownloadRequest 是 POST /agent/disks/download 的请求体。
type diskDownloadRequest struct {
	DiskID    string `json:"disk_id"`
	TargetDir string `json:"target_dir"`
	FileName  string `json:"file_name"`
	// ServerURL 预留字段：下载源始终取本地会话对应的服务端，此字段暂不生效。
	ServerURL string `json:"server_url"`
}

// ---- 下载任务管理（内存态） ----

// downloadManager 管理每个磁盘的下载任务。
//
// 任务只在内存中保留（进程重启后清空），但 <target_dir>/<file_name>.part 会留在磁盘上，
// 因此重启后重新发起同一磁盘的下载会自动从断点续传。
type downloadManager struct {
	mu    sync.Mutex
	tasks map[string]*downloadTask
}

// downloadTask 是单个下载任务的运行时状态。
type downloadTask struct {
	mu     sync.Mutex
	state  DownloadState
	cancel context.CancelFunc
}

func newDownloadManager() *downloadManager {
	return &downloadManager{tasks: make(map[string]*downloadTask)}
}

func (t *downloadTask) snapshot() DownloadState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (t *downloadTask) setCancel(cancel context.CancelFunc) {
	t.mu.Lock()
	t.cancel = cancel
	t.mu.Unlock()
}

// update 在锁内变更任务状态并返回变更后的快照。
func (t *downloadTask) update(mutate func(*DownloadState)) DownloadState {
	t.mu.Lock()
	defer t.mu.Unlock()
	mutate(&t.state)
	return t.state
}

// cancelTask 取消进行中的任务；非进行中返回 false。
func (t *downloadTask) cancelTask() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state.State != DownloadStateRunning || t.cancel == nil {
		return false
	}
	t.cancel()
	return true
}

// reserve 为 disk 预留下载槽位：已有进行中的任务时返回该任务与 false。
func (m *downloadManager) reserve(diskID string, initial DownloadState) (*downloadTask, DownloadState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.tasks[diskID]; ok {
		if snap := existing.snapshot(); snap.State == DownloadStateRunning {
			return existing, snap, false
		}
	}
	t := &downloadTask{state: initial}
	m.tasks[diskID] = t
	return t, initial, true
}

// update 更新任务状态；任务不存在时返回 false。
func (m *downloadManager) update(diskID string, mutate func(*DownloadState)) (DownloadState, bool) {
	m.mu.Lock()
	t, ok := m.tasks[diskID]
	m.mu.Unlock()
	if !ok {
		return DownloadState{}, false
	}
	return t.update(mutate), true
}

// get 返回指定磁盘的下载状态。
func (m *downloadManager) get(diskID string) (DownloadState, bool) {
	m.mu.Lock()
	t, ok := m.tasks[diskID]
	m.mu.Unlock()
	if !ok {
		return DownloadState{}, false
	}
	return t.snapshot(), true
}

// list 返回全部下载状态（按开始时间倒序，保证展示稳定）。
func (m *downloadManager) list() []DownloadState {
	m.mu.Lock()
	tasks := make([]*downloadTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()
	out := make([]DownloadState, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// cancel 取消进行中的任务；不存在或非进行中返回 false。
func (m *downloadManager) cancel(diskID string) bool {
	m.mu.Lock()
	t, ok := m.tasks[diskID]
	m.mu.Unlock()
	if !ok {
		return false
	}
	return t.cancelTask()
}

// drop 移除已预留但预检失败的任务。
func (m *downloadManager) drop(diskID string) {
	m.mu.Lock()
	delete(m.tasks, diskID)
	m.mu.Unlock()
}

// ---- 本地接口 ----

// handleDiskDownload 启动一个后台母盘下载任务（异步，立即返回 202）。
func (a *Agent) handleDiskDownload(w http.ResponseWriter, r *http.Request) {
	var req diskDownloadRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, err)
		return
	}
	diskID := strings.TrimSpace(req.DiskID)
	if diskID == "" {
		a.writeError(w, apperr.InvalidParam("disk_id"))
		return
	}

	// 目标目录：请求覆盖 > 本地配置默认值；必须是绝对路径。
	targetDir, err := validateAbsDir(downloadDirOr(req.TargetDir, a.cfg.Get().DefaultDownloadDir), "target_dir")
	if err != nil {
		a.writeError(w, err)
		return
	}

	// 会话与服务端客户端：无会话时明确返回 agent.no_session，绝不静默失败。
	client, err := a.serverClient()
	if err != nil {
		a.writeError(w, err)
		return
	}

	initial := DownloadState{
		DiskID:    diskID,
		State:     DownloadStateRunning,
		StartedAt: time.Now().UnixMilli(),
	}
	task, state, created := a.downloads.reserve(diskID, initial)
	if !created {
		// 同一 disk 已有进行中的任务：返回既有任务，不重复下载。
		a.writeJSON(w, http.StatusAccepted, map[string]any{"download": state})
		return
	}

	// 文件名：请求值优先；缺省时由服务端 Content-Disposition 推导，推导失败回退 disk-<id>.vhdx。
	fileName := strings.TrimSpace(req.FileName)
	if fileName != "" {
		fileName, err = sanitizeDownloadFileName(fileName)
		if err != nil {
			a.downloads.drop(diskID)
			a.writeError(w, err)
			return
		}
	} else {
		fileName = a.resolveRemoteFileName(client, diskID)
	}
	targetPath := filepath.Join(targetDir, fileName)
	partPath := targetPath + downloadPartSuffix

	ctx, cancel := context.WithCancel(a.bgContext())
	task.setCancel(cancel)
	state, _ = a.downloads.update(diskID, func(s *DownloadState) {
		s.FileName = fileName
		s.TargetPath = targetPath
	})

	a.logger.Info("开始下载母盘到本地", "disk_id", diskID, "target", targetPath)
	a.hub.Publish(Event{Type: "download", Data: state})

	safeGo(a.logger, "disk_download", func() { a.runDiskDownload(ctx, task, client, diskID, partPath, targetPath) })
	a.writeJSON(w, http.StatusAccepted, map[string]any{"download": state})
}

// handleListDownloads 列出全部下载任务。
func (a *Agent) handleListDownloads(w http.ResponseWriter, _ *http.Request) {
	a.writeJSON(w, http.StatusOK, map[string]any{"items": a.downloads.list()})
}

// handleCancelDownload 取消指定磁盘进行中的下载（幂等：不存在也返回 ok）。
func (a *Agent) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	diskID := strings.TrimSpace(r.PathValue("disk_id"))
	if diskID == "" {
		a.writeError(w, apperr.InvalidParam("disk_id"))
		return
	}
	if a.downloads.cancel(diskID) {
		a.logger.Info("已请求取消母盘下载", "disk_id", diskID)
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 下载执行 ----

// runDiskDownload 在后台执行下载：断点续传 → 完整性校验 → 原子改名 → 推送终态事件。
//
// 取消/失败**保留 .part**：母盘体积大，取消多为"暂停/稍后重试"，
// 保留前缀可在下次发起时从断点续传，避免整盘重下（与自更新下载一致）。
// 只有长度校验不通过（可能损坏）时才删除 .part。
func (a *Agent) runDiskDownload(ctx context.Context, task *downloadTask, client *serverClient, diskID, partPath, targetPath string) {
	if err := a.downloadDiskFile(ctx, task, client, diskID, partPath); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			a.finishDiskDownload(task, DownloadStateCanceled, "")
			a.logger.Info("母盘下载已取消", "disk_id", diskID, "target", targetPath)
			return
		}
		a.finishDiskDownload(task, DownloadStateFailed, apperr.CodeOf(err))
		a.logger.Warn("母盘下载失败", "disk_id", diskID, "target", targetPath, "error", err)
		return
	}

	// 原子落地：先删可能存在的同名正式文件，再同目录 rename。
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		a.finishDiskDownload(task, DownloadStateFailed, apperr.CodeInternal)
		a.logger.Warn("清理同名目标文件失败", "disk_id", diskID, "target", targetPath, "error", err)
		return
	}
	if err := os.Rename(partPath, targetPath); err != nil {
		a.finishDiskDownload(task, DownloadStateFailed, apperr.CodeInternal)
		a.logger.Warn("下载落地改名失败", "disk_id", diskID, "target", targetPath, "error", err)
		return
	}
	// 落地后清理断点元数据（尽力而为；残留不影响下次下载，会在预检时被清理）。
	if err := os.Remove(downloadMetaPath(partPath)); err != nil && !os.IsNotExist(err) {
		a.logger.Warn("清理下载元数据失败", "path", downloadMetaPath(partPath), "error", err)
	}
	a.finishDiskDownload(task, DownloadStateDone, "")
	a.logger.Info("母盘下载完成", "disk_id", diskID, "target", targetPath)
}

// downloadDiskFile 把母盘内容下载到 partPath：分段并发 + 断点续传 + 自动重试 + 陈旧前缀保护。
//
// 只做"字节数 == total_bytes"的完整性判断（服务端不提供摘要，全量 SHA256 成本过高）。
func (a *Agent) downloadDiskFile(ctx context.Context, task *downloadTask, client *serverClient, diskID, partPath string) error {
	metaPath := downloadMetaPath(partPath)
	conns := a.downloadConnections()
	prog := newDownloadProgress(a, task)

	var lastErr error
	for attempt := 1; attempt <= downloadMaxAttempts; attempt++ {
		if attempt > 1 {
			delay := downloadRetryDelay(attempt - 1)
			a.logger.Warn("母盘下载失败，准备重试",
				"disk_id", diskID, "attempt", attempt, "max", downloadMaxAttempts,
				"received_bytes", prog.received.Load(), "delay", delay.String(), "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := a.downloadAttempt(ctx, task, client, diskID, partPath, metaPath, conns, prog)
		if err == nil {
			return a.verifyDownloadedSize(partPath, metaPath)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errDownloadResetNeeded) {
			// 远端已替换或本地断点不可信：downloadAttempt 已重置 .part 与 meta，稍后退避重来。
			a.logger.Warn("本地断点已重置，重新开始下载", "disk_id", diskID, "attempt", attempt)
			lastErr = err
			continue
		}
		if !isRetryableDownloadError(err) {
			return err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errDownloadFailed("attempts_exhausted", errors.New("download attempts exhausted"))
	} else if errors.Is(lastErr, errDownloadResetNeeded) {
		lastErr = errDownloadFailed("stale_prefix", lastErr)
	}
	return lastErr
}

// downloadAttempt 执行一次下载尝试（一次可能含 N 个并发分段），并集中处理断点重置。
func (a *Agent) downloadAttempt(ctx context.Context, task *downloadTask, client *serverClient, diskID, partPath, metaPath string, conns int, prog *downloadProgress) error {
	partSize, err := partFileSize(partPath)
	if err != nil {
		return errDownloadFailed("stat_part", err)
	}
	meta, status := loadDownloadMeta(metaPath)

	var runErr error
	switch {
	case status == metaCorrupt && partSize > 0:
		// .part 存在但元数据损坏：无法区分单流/分段，为避免静默损坏，整体重置后从头下载。
		a.logger.Warn("下载元数据损坏，丢弃本地断点", "disk_id", diskID, "path", metaPath)
		a.resetDownloadState(partPath, metaPath, prog)
		runErr = a.freshDownload(ctx, task, client, diskID, partPath, metaPath, conns, prog)
	case partSize > 0 && meta != nil && len(meta.Segments) > 0:
		runErr = a.resumeSegmented(ctx, task, client, diskID, partPath, metaPath, meta, partSize, prog)
	case partSize > 0:
		// 单流续传：meta 有效则携带 If-Range；meta 缺失（旧版本残留）则降级为不带保护的续传。
		if meta == nil {
			a.logger.Info("下载元数据缺失，降级为单流续传", "disk_id", diskID)
			meta = &downloadPartMeta{Version: downloadPartMetaVersion, DiskID: diskID}
		}
		runErr = a.resumeSingleStream(ctx, task, client, diskID, partPath, metaPath, meta, partSize, prog)
	default:
		_ = os.Remove(metaPath) // 残留/损坏的元数据（对应 .part 不存在）：清理后按全新下载处理。
		runErr = a.freshDownload(ctx, task, client, diskID, partPath, metaPath, conns, prog)
	}

	switch {
	case runErr == nil:
		return nil
	case errors.Is(runErr, errDownloadResetNeeded):
		a.resetDownloadState(partPath, metaPath, prog)
		return errDownloadResetNeeded
	case errors.Is(runErr, errDownloadRangeNotSatisfiable):
		a.resetDownloadState(partPath, metaPath, prog)
		return errDownloadFailed("resume_range", runErr)
	case errors.Is(runErr, errDownloadSizeMismatch):
		a.removeDownloadState(partPath, metaPath)
		return apperr.New(CodeDownloadFailed, http.StatusInternalServerError).
			WithArg("stage", "verify_size").
			WithArg("expected_size", downloadMetaTotal(meta))
	case errors.Is(runErr, errDownloadDiskFull):
		return errInsufficientLocalSpace(downloadRequiredSpace(downloadMetaTotal(meta)), freeSpaceBytes(filepath.Dir(partPath)))
	}
	return runErr
}

// freshDownload 全新下载：先探测响应头，再决定分段并发或单流。
func (a *Agent) freshDownload(ctx context.Context, task *downloadTask, client *serverClient, diskID, partPath, metaPath string, conns int, prog *downloadProgress) error {
	resp, err := client.OpenRange(ctx, diskContentPath(diskID), 0)
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusPartialContent:
		// 未带 Range 却回了 206：不符合预期，按可重试的协议错误处理。
		_ = resp.Body.Close()
		return errDownloadFailed("probe", errors.New("unexpected 206 without range"))
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return parseServerError(resp.StatusCode, data)
	}

	total := parseContentLength(resp.Header.Get("Content-Length"))
	lastModified := strings.TrimSpace(resp.Header.Get("Last-Modified"))
	etag := strings.TrimSpace(resp.Header.Get("ETag"))
	acceptRanges := isBytesAcceptRanges(resp.Header.Get("Accept-Ranges"))

	dir := filepath.Dir(partPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		_ = resp.Body.Close()
		return errDownloadFailed("prepare_dir", err)
	}
	// 空间预检：不足则直接失败，绝不开始写盘。
	if err := a.ensureLocalSpace(dir, total); err != nil {
		_ = resp.Body.Close()
		return err
	}

	meta := &downloadPartMeta{
		Version:      downloadPartMetaVersion,
		DiskID:       diskID,
		Total:        total,
		LastModified: lastModified,
		ETag:         etag,
	}

	// 分段触发条件（全部满足）：服务端声明 Accept-Ranges: bytes、total 已知且 >= 64MiB、并发 > 1。
	if acceptRanges && total >= downloadSegmentedMinBytes && conns > 1 {
		segments := buildDownloadSegments(total, conns)
		if len(segments) >= 2 {
			_ = resp.Body.Close()
			meta.Connections = len(segments)
			meta.Segments = segments
			if err := writeDownloadMeta(metaPath, meta); err != nil {
				return errDownloadFailed("write_meta", err)
			}
			prog.reset(total, 0)
			return a.runSegments(ctx, client, diskID, partPath, metaPath, meta, newSegmentStates(segments), prog)
		}
	}

	// 单流：复用本次探测响应，避免多打一次请求。
	if err := writeDownloadMeta(metaPath, meta); err != nil {
		_ = resp.Body.Close()
		return errDownloadFailed("write_meta", err)
	}
	prog.reset(total, 0)
	return a.consumeSingleStream(ctx, partPath, metaPath, meta, resp, 0, prog)
}

// resumeSegmented 从 meta 记录的分段偏移继续下载。
func (a *Agent) resumeSegmented(ctx context.Context, task *downloadTask, client *serverClient, diskID, partPath, metaPath string, meta *downloadPartMeta, partSize int64, prog *downloadProgress) error {
	if partSize != meta.Total || !validDownloadSegments(meta.Segments, meta.Total) {
		// 分段模式要求 .part 已预分配到 total；大小/边界不符说明断点不可信。
		return errDownloadResetNeeded
	}
	states := newSegmentStates(meta.Segments)
	var done int64
	for i, st := range states {
		d := meta.Segments[i].Done
		if d < 0 || d > st.length() {
			d = 0
		}
		st.done.Store(d)
		done += d
	}
	prog.reset(meta.Total, done)
	if done >= meta.Total {
		return nil // 已完整（例如上次改名失败）：交由上层校验/落地。
	}
	return a.runSegments(ctx, client, diskID, partPath, metaPath, meta, states, prog)
}

// resumeSingleStream 单流断点续传（也用于"元数据缺失的旧残留"降级续传）。
//
// meta 可能来自磁盘（带校验信息），也可能是降级场景下临时构造的空元数据：
// 后者不带 If-Range，且会在读到响应头后用新信息补全 meta，从而升级为受保护续传。
func (a *Agent) resumeSingleStream(ctx context.Context, task *downloadTask, client *serverClient, diskID, partPath, metaPath string, meta *downloadPartMeta, offset int64, prog *downloadProgress) error {
	validator := meta.validator()
	resp, err := client.OpenRangeBytes(ctx, diskContentPath(diskID), offset, -1, validator)
	if err != nil {
		return err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		if validator != "" {
			// 带 If-Range 仍回 200：远端母盘已被替换，绝不能把新内容接到旧前缀后面。
			_ = resp.Body.Close()
			return errDownloadResetNeeded
		}
		if offset > 0 {
			// 无校验信息（旧残留）：服务端忽略 Range，丢弃前缀从头下载（保持既有行为）。
			a.logger.Info("服务端忽略 Range，改为从头下载", "disk_id", diskID)
			offset = 0
		}
		meta.Total = parseContentLength(resp.Header.Get("Content-Length"))
	case http.StatusPartialContent:
		if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
			if validator != "" && meta.Total > 0 && total != meta.Total {
				// 记录的总长与远端现状不符：远端已变，重置。
				_ = resp.Body.Close()
				return errDownloadResetNeeded
			}
			meta.Total = total
		}
		if meta.Total > 0 && offset >= meta.Total {
			_ = resp.Body.Close()
			if validator == "" {
				// 无校验信息：无法确认远端未变，谨慎起见重置而非直接判为完成。
				return errDownloadResetNeeded
			}
			prog.report(meta.Total, true)
			return nil
		}
	case http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		return errDownloadRangeNotSatisfiable
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return parseServerError(resp.StatusCode, data)
	}

	// 用最新响应头补全校验信息（降级场景借此升级为受保护续传），随后落盘 meta。
	meta.Version = downloadPartMetaVersion
	meta.Segments = nil // 单流模式
	if lm := strings.TrimSpace(resp.Header.Get("Last-Modified")); lm != "" {
		meta.LastModified = lm
	}
	if tag := strings.TrimSpace(resp.Header.Get("ETag")); tag != "" {
		meta.ETag = tag
	}
	if err := writeDownloadMeta(metaPath, meta); err != nil {
		a.logger.Warn("写入下载元数据失败", "path", metaPath, "error", err)
	}
	prog.reset(meta.Total, offset)
	return a.consumeSingleStream(ctx, partPath, metaPath, meta, resp, offset, prog)
}

// consumeSingleStream 把响应体顺序写入 .part（从 offset 开始），并同步维护 meta.single_done。
func (a *Agent) consumeSingleStream(ctx context.Context, partPath, metaPath string, meta *downloadPartMeta, resp *http.Response, offset int64, prog *downloadProgress) error {
	defer func() { _ = resp.Body.Close() }()

	received, err := a.writePartSequential(ctx, resp.Body, partPath, offset, meta.Total, prog)
	if err != nil {
		return err
	}
	if meta.Total > 0 && received != meta.Total {
		// 长度不符：确定性损坏，删除断点后失败（不重试；文件句柄已随 writePartSequential 关闭）。
		a.removeDownloadState(partPath, metaPath)
		return apperr.New(CodeDownloadFailed, http.StatusInternalServerError).
			WithArg("stage", "verify_size").
			WithArg("expected_size", meta.Total).
			WithArg("actual_size", received)
	}
	meta.SingleDone = received
	if err := writeDownloadMeta(metaPath, meta); err != nil {
		a.logger.Warn("写入下载元数据失败", "path", metaPath, "error", err)
	}
	prog.report(received, true)
	return nil
}

// writePartSequential 顺序写盘：从 offset 起把 body 写入 partPath，返回已落盘的绝对字节数。
//
// offset == 0 时会先把 .part 截断为 0（等价于从头写）。文件句柄在本函数内关闭，
// 以便调用方在返回后删除 .part（Windows 上无法删除仍被打开的文件）。
func (a *Agent) writePartSequential(ctx context.Context, body io.Reader, partPath string, offset, total int64, prog *downloadProgress) (int64, error) {
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, errDownloadFailed("open_part", err)
	}
	defer func() { _ = f.Close() }()

	if offset == 0 {
		if err := f.Truncate(0); err != nil {
			return 0, errDownloadFailed("truncate_part", err)
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, errDownloadFailed("seek_part", err)
	}

	var src io.Reader = body
	if total > 0 {
		// 已知总大小：最多只读 remaining+1 字节，杜绝镜像端无限流式响应写满磁盘。
		src = io.LimitReader(body, total-offset+1)
	}
	buf := downloadCopyPool.Get().([]byte)
	defer downloadCopyPool.Put(buf)

	n, err := io.CopyBuffer(&countWriter{w: f, p: prog}, src, buf)
	if err != nil {
		if ctx.Err() != nil {
			return offset + n, ctx.Err()
		}
		return offset + n, mapDownloadBodyError(err)
	}
	return offset + n, nil
}

// runSegments 并发执行 N 个分段：每段独立 OpenRange + WriteAt，互不影响。
//
// .part 预先 Truncate(total) 成稀疏文件，各段用 WriteAt 写到自己的绝对偏移，
// 因此不需要各段写独立文件再拼接（避免 2 倍磁盘占用与额外 IO）。
func (a *Agent) runSegments(ctx context.Context, client *serverClient, diskID, partPath, metaPath string, meta *downloadPartMeta, states []*segmentState, prog *downloadProgress) error {
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return errDownloadFailed("open_part", err)
	}
	defer func() { _ = f.Close() }()

	// 预分配：制造稀疏文件，使各段可直接 WriteAt 到绝对偏移。
	if err := f.Truncate(meta.Total); err != nil {
		return errDownloadFailed("truncate_part", err)
	}

	validator := meta.validator()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		if err == nil {
			return
		}
		errOnce.Do(func() {
			firstErr = err
			cancel() // 任一非取消类错误：立即取消其余分段
		})
	}

	for _, st := range states {
		if st.done.Load() >= st.length() {
			continue
		}
		wg.Add(1)
		safeGo(a.logger, "download_segment", func() {
			defer wg.Done()
			fail(a.copySegment(runCtx, client, diskID, f, st, validator, meta.Total, prog))
		})
	}
	wg.Wait()

	// 无论成功失败都持久化当前分段偏移，供下次续传（进程被杀只会落后、不会超前）。
	a.persistDownloadMeta(metaPath, meta, states)

	if firstErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return firstErr
	}
	for _, st := range states {
		if st.done.Load() != st.length() {
			return errDownloadSizeMismatch
		}
	}
	return nil
}

// copySegment 把一个分段的剩余字节全部拉取并写入（必要时可多次请求续传该段）。
func (a *Agent) copySegment(ctx context.Context, client *serverClient, diskID string, f *os.File, st *segmentState, validator string, total int64, prog *downloadProgress) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := st.length() - st.done.Load()
		if remaining <= 0 {
			return nil
		}
		start := st.start + st.done.Load()
		resp, err := client.OpenRangeBytes(ctx, diskContentPath(diskID), start, st.end, validator)
		if err != nil {
			return err
		}
		err = a.consumeSegmentBody(ctx, f, st, resp, start, remaining, total, validator, prog)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
}

// consumeSegmentBody 校验单个分段的响应并把它写入 .part 的对应偏移。
func (a *Agent) consumeSegmentBody(ctx context.Context, f *os.File, st *segmentState, resp *http.Response, start, remaining, total int64, validator string, prog *downloadProgress) error {
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		gotStart, gotTotal := parseContentRange(resp.Header.Get("Content-Range"))
		if gotStart >= 0 && gotStart != start {
			// 服务端给的起点与我们请求的不一致：无法安全拼接，重置。
			return errDownloadResetNeeded
		}
		if validator != "" && total > 0 && gotTotal > 0 && gotTotal != total {
			return errDownloadResetNeeded
		}
	case http.StatusOK:
		// 带 If-Range 却回 200：远端已变（或服务端不再支持分段），重置。
		return errDownloadResetNeeded
	case http.StatusRequestedRangeNotSatisfiable:
		return errDownloadRangeNotSatisfiable
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return parseServerError(resp.StatusCode, data)
	}

	buf := downloadCopyPool.Get().([]byte)
	defer downloadCopyPool.Put(buf)

	w := &segmentWriter{f: f, off: start, p: prog}
	n, err := io.CopyBuffer(w, io.LimitReader(resp.Body, remaining), buf)
	st.done.Add(n)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return mapDownloadBodyError(err)
	}
	if n < remaining {
		// 响应体提前结束：视为可重试的中断。
		return errDownloadFailed("download_body", io.ErrUnexpectedEOF)
	}
	return nil
}

// countWriter 顺序写的同时把字节数记入进度聚合器。
type countWriter struct {
	w io.Writer
	p *downloadProgress
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	if n > 0 {
		c.p.add(int64(n))
	}
	return n, err
}

// segmentWriter 以 WriteAt 写到分段的绝对偏移（不依赖文件游标，故可并发）。
type segmentWriter struct {
	f   *os.File
	off int64
	p   *downloadProgress
}

func (w *segmentWriter) Write(b []byte) (int, error) {
	n, err := w.f.WriteAt(b, w.off)
	w.off += int64(n)
	if n > 0 {
		w.p.add(int64(n))
	}
	return n, err
}

// segmentState 是一个分段的运行态：绝对闭区间 [start, end] 与已写入字节数。
type segmentState struct {
	start int64
	end   int64
	done  atomic.Int64
}

// length 返回该分段应写入的字节数。
func (s *segmentState) length() int64 { return s.end - s.start + 1 }

// newSegmentStates 由持久化分段构造运行态（done 初始为 0）。
func newSegmentStates(segments []downloadSegment) []*segmentState {
	states := make([]*segmentState, len(segments))
	for i, s := range segments {
		states[i] = &segmentState{start: s.Start, end: s.End}
	}
	return states
}

// buildDownloadSegments 把 [0, total) 均分为 n 段，边界固定可复现：
// start_i = total*i/n，end_i = total*(i+1)/n - 1。
func buildDownloadSegments(total int64, n int) []downloadSegment {
	if n < 2 || total <= 0 {
		return nil
	}
	segments := make([]downloadSegment, 0, n)
	for i := 0; i < n; i++ {
		start := total * int64(i) / int64(n)
		end := total*int64(i+1)/int64(n) - 1
		if end < start {
			continue
		}
		segments = append(segments, downloadSegment{Start: start, End: end})
	}
	return segments
}

// validDownloadSegments 校验分段是否自洽：从 0 连续覆盖到 total-1。
func validDownloadSegments(segments []downloadSegment, total int64) bool {
	if total <= 0 || len(segments) == 0 {
		return false
	}
	var expect int64
	for _, s := range segments {
		if s.Start != expect || s.End < s.Start {
			return false
		}
		expect = s.End + 1
	}
	return expect == total
}

// downloadProgress 汇总整盘下载进度：received 为各段已收字节之和（原子累加），
// 统一经 progressThrottle 节流后才推送 SSE，避免多 goroutine 并发推送与高频率刷屏。
type downloadProgress struct {
	a        *Agent
	task     *downloadTask
	total    atomic.Int64
	received atomic.Int64

	mu       sync.Mutex
	throttle progressThrottle
}

func newDownloadProgress(a *Agent, task *downloadTask) *downloadProgress {
	return &downloadProgress{a: a, task: task, throttle: newProgressThrottle(0)}
}

// reset 重置进度基线（总大小/已收字节），并立即推送一次。
func (p *downloadProgress) reset(total, received int64) {
	p.total.Store(total)
	p.received.Store(received)
	p.push(true)
}

// add 累加已收字节（各段写入后调用）。
func (p *downloadProgress) add(n int64) {
	if n <= 0 {
		return
	}
	p.received.Add(n)
	p.push(false)
}

// report 按需推送一次进度（force=true 时忽略节流）。
func (p *downloadProgress) report(total int64, force bool) {
	if total > 0 {
		p.total.Store(total)
	}
	p.push(force)
}

// push 在节流允许时推送进度。
func (p *downloadProgress) push(force bool) {
	got := p.received.Load()
	p.mu.Lock()
	ok := p.throttle.allow(force, got, time.Now())
	p.mu.Unlock()
	if !ok {
		return
	}
	p.a.reportDownloadProgress(p.task, got, p.total.Load())
}

// downloadPartMeta 是断点续传的旁路元数据（<target>.part.meta）。
//
// 记录远端的校验信息（用于 If-Range 陈旧前缀保护）与各分段的已完成偏移，
// 使取消 / 异常退出 / 进程重启后每个分段都能从各自的 done 继续。
type downloadPartMeta struct {
	// Version 元数据格式版本（不符则视为不可用，从头下载）。
	Version int `json:"version"`
	// DiskID 该断点属于哪块母盘（便于排查）。
	DiskID string `json:"disk_id,omitempty"`
	// Total 远端总字节数（未知为 0）。
	Total int64 `json:"total_bytes"`
	// LastModified 远端 Last-Modified（If-Range 首选校验值）。
	LastModified string `json:"last_modified,omitempty"`
	// ETag 远端 ETag（Last-Modified 缺失时的备选）。
	ETag string `json:"etag,omitempty"`
	// Connections 分段数（仅分段模式）。
	Connections int `json:"connections,omitempty"`
	// Segments 分段偏移（分段模式）；为空表示单流模式。
	Segments []downloadSegment `json:"segments,omitempty"`
	// SingleDone 单流模式已落盘字节数（信息性；续传以 .part 实际大小为准）。
	SingleDone int64 `json:"single_done,omitempty"`
}

// downloadSegment 是一个分段的持久化表示。
//
// Done 为该段已写入的字节数（相对量），续传偏移为 start+done。
type downloadSegment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

// downloadMetaStatus 表示读取断点元数据的结果。
type downloadMetaStatus int

const (
	// metaAbsent 元数据文件不存在（可能是旧版本残留的 .part）。
	metaAbsent downloadMetaStatus = iota
	// metaValid 元数据可用。
	metaValid
	// metaCorrupt 元数据存在但损坏/版本不符（不可信）。
	metaCorrupt
)

// validator 返回 If-Range 使用的校验值：优先 Last-Modified，其次 ETag；都无则空串（不启用保护）。
func (m *downloadPartMeta) validator() string {
	if m == nil {
		return ""
	}
	if v := strings.TrimSpace(m.LastModified); v != "" {
		return v
	}
	return strings.TrimSpace(m.ETag)
}

// downloadMetaPath 返回 part 文件对应的断点元数据路径（<target>.part.meta）。
func downloadMetaPath(partPath string) string {
	return partPath + downloadPartMetaSuffix
}

// downloadMetaTotal 返回元数据声明的总大小（meta 为 nil 时返回 0）。
func downloadMetaTotal(meta *downloadPartMeta) int64 {
	if meta == nil || meta.Total < 0 {
		return 0
	}
	return meta.Total
}

// loadDownloadMeta 读取断点元数据。
func loadDownloadMeta(path string) (*downloadPartMeta, downloadMetaStatus) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, metaAbsent
		}
		return nil, metaCorrupt
	}
	var meta downloadPartMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, metaCorrupt
	}
	if meta.Version != downloadPartMetaVersion {
		return nil, metaCorrupt
	}
	return &meta, metaValid
}

// writeDownloadMeta 原子替换式写入断点元数据（先写临时文件再改名）。
//
// Windows 上 os.Rename 不会覆盖已存在的目标，故这里用 x/sys/windows.MoveFileEx 的
// MOVEFILE_REPLACE_EXISTING 做原子替换：任何时刻磁盘上都存在一份完整可解析的 meta，
// 不会出现"写一半"或"短暂缺失"的中间态。
func writeDownloadMeta(path string, meta *downloadPartMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	to, err := windows.UTF16PtrFromString(path)
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

// persistDownloadMeta 把当前分段偏移落盘（尽力而为，失败只记日志）。
//
// done 只在对应字节完整写入后才累加，因此 meta 记录的偏移永远不会超过 .part 中已落盘的字节数：
// 进程被杀最多让进度"落后"（下次重下少量数据），绝不会让新数据接到空洞后面。
func (a *Agent) persistDownloadMeta(metaPath string, meta *downloadPartMeta, states []*segmentState) {
	if meta == nil {
		return
	}
	snapshot := *meta
	snapshot.Segments = make([]downloadSegment, len(states))
	for i, st := range states {
		snapshot.Segments[i] = downloadSegment{Start: st.start, End: st.end, Done: st.done.Load()}
	}
	if err := writeDownloadMeta(metaPath, &snapshot); err != nil {
		a.logger.Warn("写入下载元数据失败", "path", metaPath, "error", err)
	}
}

// resetDownloadState 丢弃 .part 与 meta 并把进度归零（陈旧前缀保护 / 416 / 状态不可信时使用）。
func (a *Agent) resetDownloadState(partPath, metaPath string, prog *downloadProgress) {
	a.removeDownloadState(partPath, metaPath)
	if prog != nil {
		prog.reset(0, 0)
	}
}

// removeDownloadState 删除 .part 与 meta（尽力而为）。
//
// 注意：Windows 上无法删除仍被打开的文件，因此调用点必须在目标文件句柄关闭之后。
func (a *Agent) removeDownloadState(partPath, metaPath string) {
	if err := os.Remove(partPath); err != nil && !os.IsNotExist(err) {
		a.logger.Warn("删除下载断点失败", "path", partPath, "error", err)
	}
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		a.logger.Warn("删除下载元数据失败", "path", metaPath, "error", err)
	}
}

// verifyDownloadedSize 落地前的最终字节数校验（元数据声明了总大小时才校验）。
func (a *Agent) verifyDownloadedSize(partPath, metaPath string) error {
	meta, status := loadDownloadMeta(metaPath)
	if status != metaValid || meta.Total <= 0 {
		return nil // 无校验信息（远端未声明长度）：跳过
	}
	size, err := partFileSize(partPath)
	if err != nil {
		return errDownloadFailed("stat_part", err)
	}
	if size != meta.Total {
		a.removeDownloadState(partPath, metaPath)
		return apperr.New(CodeDownloadFailed, http.StatusInternalServerError).
			WithArg("stage", "verify_size").
			WithArg("expected_size", meta.Total).
			WithArg("actual_size", size)
	}
	return nil
}

// downloadConnections 返回生效的分段并发数（配置已归一，这里再兜底一次）。
func (a *Agent) downloadConnections() int {
	return normalizeDownloadConnections(a.cfg.Get().DownloadConnections)
}

// downloadRetryDelay 返回第 retry 次重试（1-based）前的退避时长。
func downloadRetryDelay(retry int) time.Duration {
	if retry < 1 {
		retry = 1
	}
	if retry > len(downloadRetryDelays) {
		retry = len(downloadRetryDelays)
	}
	return downloadRetryDelays[retry-1]
}

// isRetryableDownloadError 判断下载错误是否值得重试。
//
// 可重试：5xx（含网络不可达 502）、网络中断 / io.ErrUnexpectedEOF / connection reset 等。
// 不重试：4xx（404/403/409 等确定性错误）、416、空间不足、本地准备/校验类失败。
func isRetryableDownloadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, errDownloadResetNeeded) || errors.Is(err, errDownloadRangeNotSatisfiable) ||
		errors.Is(err, errDownloadDiskFull) || errors.Is(err, errDownloadSizeMismatch) {
		return false
	}
	e, ok := apperr.As(err)
	if !ok {
		// 非结构化错误：网络层 / 本地 IO 中断，重试通常可恢复。
		return true
	}
	if e.Code == CodeInsufficientLocalSpace {
		return false
	}
	switch e.Args["stage"] {
	case "verify_size", "resume_range", "write_meta", "prepare_dir", "open_part",
		"truncate_part", "seek_part", "stat_part", "probe", "attempts_exhausted", "stale_prefix":
		return false
	}
	return e.HTTP >= 500
}

// mapDownloadBodyError 把读/写 body 的错误映射为稳定错误码。
//
// 磁盘写满（ENOSPC）单独识别为 errDownloadDiskFull（上层映射为空间不足）；
// 其余中断保留断点（可重试）。
func mapDownloadBodyError(err error) error {
	if isDiskFullError(err) {
		return errDownloadDiskFull
	}
	return errDownloadFailed("download_body", err)
}

// ensureLocalSpace 预检目标目录所在卷的可用空间；不足则直接失败（不开始写盘）。
//
// 探测失败（例如路径不是盘符卷）时不阻断下载，只记 WARN：可用性优先，
// 真正的兜底是写盘时 ENOSPC → agent.insufficient_local_space。
func (a *Agent) ensureLocalSpace(dir string, total int64) error {
	need := downloadRequiredSpace(total)
	free, err := volumeFreeBytes(dir)
	if err != nil {
		a.logger.Warn("无法探测目标卷可用空间，跳过空间预检", "dir", dir, "error", err)
		return nil
	}
	if free < need {
		return errInsufficientLocalSpace(need, free)
	}
	return nil
}

// downloadRequiredSpace 返回下载 total 字节所需的可用空间（含预留）。
//
// 预留取 max(1GiB, total*5%)，避免把目标卷写满而影响系统与其它进程；
// total 未知（<=0）时至少要求 1GiB。
func downloadRequiredSpace(total int64) int64 {
	if total <= 0 {
		return downloadSpaceReserveFloor
	}
	reserve := total / downloadSpaceReserveRatio
	if reserve < downloadSpaceReserveFloor {
		reserve = downloadSpaceReserveFloor
	}
	return total + reserve
}

// volumeFreeBytes 返回目录所在卷的可用字节数（原生 GetDiskFreeSpaceEx）。
//
// 选择原生 API 而非复用 winps 的卷空间脚本：下载前/写盘失败时需要**同步、快速**地拿到
// 结果（脚本要起一次 PowerShell，秒级且依赖 PowerShell 可用），
// 与 internal/platform/winvhd 的 ensureFreeSpace 采用同一套做法，保持一致。
func volumeFreeBytes(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var freeAvailable, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvailable, &totalBytes, &totalFree); err != nil {
		return 0, err
	}
	return int64(freeAvailable), nil
}

// freeSpaceBytes 尽力而为地读取可用空间（失败返回 0），用于写盘期 ENOSPC 的错误参数。
func freeSpaceBytes(dir string) int64 {
	free, err := volumeFreeBytes(dir)
	if err != nil {
		return 0
	}
	return free
}

// isDiskFullError 判断错误是否为磁盘写满（ENOSPC）。
func isDiskFullError(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

// isBytesAcceptRanges 判断 Accept-Ranges 是否声明支持字节区间。
func isBytesAcceptRanges(v string) bool {
	return strings.Contains(strings.ToLower(v), "bytes")
}

// parseContentRange 解析 Content-Range（bytes 0-99/12345）。
//
// 返回起点与总长；起点无法解析时返回 -1，总长未知（*）或无法解析时返回 -1。
func parseContentRange(v string) (start, total int64) {
	v = strings.TrimSpace(v)
	const prefix = "bytes "
	if !strings.HasPrefix(v, prefix) {
		return -1, 0
	}
	spec := strings.TrimSpace(v[len(prefix):])
	slash := strings.LastIndexByte(spec, '/')
	if slash < 0 {
		return -1, 0
	}
	rangePart := strings.TrimSpace(spec[:slash])
	totalPart := strings.TrimSpace(spec[slash+1:])
	dash := strings.IndexByte(rangePart, '-')
	if dash <= 0 {
		return -1, 0
	}
	s, err := strconv.ParseInt(strings.TrimSpace(rangePart[:dash]), 10, 64)
	if err != nil || s < 0 {
		return -1, 0
	}
	if totalPart == "" || totalPart == "*" {
		return s, -1
	}
	t, err := strconv.ParseInt(totalPart, 10, 64)
	if err != nil || t < 0 {
		return s, -1
	}
	return s, t
}

// reportDownloadProgress 更新任务进度并推送 download 事件（total<=0 时保持既有总值）。
func (a *Agent) reportDownloadProgress(task *downloadTask, received, total int64) {
	state := task.update(func(s *DownloadState) {
		s.ReceivedBytes = received
		if total > 0 {
			s.TotalBytes = total
		}
	})
	a.hub.Publish(Event{Type: "download", Data: state})
}

// finishDiskDownload 收尾任务：置终态、记录错误码、推送 download 事件。
func (a *Agent) finishDiskDownload(task *downloadTask, state, errCode string) {
	now := time.Now().UnixMilli()
	snap := task.update(func(s *DownloadState) {
		s.State = state
		s.Error = errCode
		s.FinishedAt = now
		if state == DownloadStateDone && s.TotalBytes > 0 {
			s.ReceivedBytes = s.TotalBytes
		}
	})
	a.hub.Publish(Event{Type: "download", Data: snap})
}

// resolveRemoteFileName 通过一次带 Range 的 GET 读取响应头，从 Content-Disposition 推导文件名。
//
// 任何失败（不可达 / 非 2xx / 无文件名）都回退为 disk-<id>.vhdx：
// 下载是否真正可行由后台任务决定，不在探测阶段阻断。
func (a *Agent) resolveRemoteFileName(client *serverClient, diskID string) string {
	fallback := downloadFallbackFileName(diskID)
	ctx, cancel := context.WithTimeout(a.bgContext(), downloadProbeTimeout)
	defer cancel()

	resp, err := client.OpenRange(ctx, diskContentPath(diskID), 0)
	if err != nil {
		return fallback
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fallback
	}
	name, err := sanitizeDownloadFileName(filenameFromContentDisposition(resp.Header.Get("Content-Disposition")))
	if err != nil {
		return fallback
	}
	return name
}

// ---- 校验与解析辅助 ----

// diskContentPath 返回服务端母盘内容下载路径。
func diskContentPath(diskID string) string {
	return downloadContentPathPrefix + url.PathEscape(diskID) + "/content"
}

// downloadDirOr 返回请求指定的目录；为空时回退到本地配置的默认目录。
func downloadDirOr(requested, fallback string) string {
	if dir := strings.TrimSpace(requested); dir != "" {
		return dir
	}
	return fallback
}

// validateAbsDir 校验收到的目录非空且为绝对路径（非法时返回 system.invalid_param）。
func validateAbsDir(dir, field string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return "", apperr.InvalidParam(field)
	}
	return dir, nil
}

// sanitizeDownloadFileName 清洗并校验下载文件名。
//
// 只允许单个纯文件名：拒绝路径穿越、绝对路径与路径分隔符（\ / :），
// 并移植上传侧的 Windows 保留名 / 非法字符规则。
func sanitizeDownloadFileName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", apperr.InvalidParam("file_name")
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		return "", apperr.InvalidParam("file_name")
	}
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return "", apperr.InvalidParam("file_name")
	}
	if isWindowsReservedName(name) {
		return "", apperr.InvalidParam("file_name")
	}
	return name, nil
}

// windowsDownloadReservedNames 是 Windows 保留设备名（不含扩展名比较）。
var windowsDownloadReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// isWindowsReservedName 判断文件名是否为 Windows 保留名（含带扩展名形式）。
func isWindowsReservedName(name string) bool {
	base := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		base = name[:i]
	}
	return windowsDownloadReservedNames[strings.ToUpper(base)]
}

// downloadFallbackFileName 由 disk_id 生成安全的回退文件名（只保留 [A-Za-z0-9._-]）。
//
// 之所以要清洗：disk_id 来自请求，若含路径分隔符直接拼进文件名会造成路径穿越。
func downloadFallbackFileName(diskID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
			return r
		}
		return -1
	}, diskID)
	if safe == "" {
		safe = "unknown"
	}
	return "disk-" + safe + ".vhdx"
}

// filenameFromContentDisposition 从 Content-Disposition 头中提取 filename。
func filenameFromContentDisposition(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	lower := strings.ToLower(v)
	// 优先 RFC 5987 的 filename*=。
	if i := strings.Index(lower, "filename*="); i >= 0 {
		raw := v[i+len("filename*="):]
		if j := strings.IndexByte(raw, ';'); j >= 0 {
			raw = raw[:j]
		}
		raw = strings.TrimSpace(raw)
		if k := strings.Index(raw, "''"); k >= 0 {
			raw = raw[k+2:]
		}
		if decoded, err := url.QueryUnescape(strings.Trim(raw, `"`)); err == nil {
			if name := strings.TrimSpace(decoded); name != "" {
				return name
			}
		}
	}
	if i := strings.Index(lower, "filename="); i >= 0 {
		raw := v[i+len("filename="):]
		if j := strings.IndexByte(raw, ';'); j >= 0 {
			raw = raw[:j]
		}
		return strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), `"`))
	}
	return ""
}

// partFileSize 返回 part 文件已有的字节数；文件不存在时返回 0。
func partFileSize(partPath string) (int64, error) {
	info, err := os.Stat(partPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if info.IsDir() {
		return 0, nil
	}
	return info.Size(), nil
}

// parseContentLength 解析 Content-Length；非法或缺失返回 0。
func parseContentLength(v string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseContentRangeTotal 从 Content-Range（bytes 0-99/12345）解析总大小；未知返回 0。
func parseContentRangeTotal(v string) int64 {
	i := strings.LastIndexByte(v, '/')
	if i < 0 {
		return 0
	}
	total := strings.TrimSpace(v[i+1:])
	if total == "" || total == "*" {
		return 0
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

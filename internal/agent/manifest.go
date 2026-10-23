package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"vault/internal/apperr"
)

// 上传核对的本地清单任务。
//
// 为什么做成"异步任务 + 轮询状态"而不是一个同步请求：核对动辄几十万文件、1000GB，
// 同步接口在算哈希期间只能让界面干转圈（连"正在读哪个文件"都看不到）。拆成
// start/status/result 三段后，界面能持续展示阶段、已处理文件数、字节数与当前文件。
const (
	manifestStateRunning   = "running"
	manifestStateSucceeded = "succeeded"
	manifestStateFailed    = "failed"

	manifestPhaseScan = "scan"
	manifestPhaseHash = "hash"
	manifestPhaseDone = "done"

	// manifestTaskTTL 是已完成任务在内存里保留的时长（够界面取走结果即可）。
	manifestTaskTTL = 15 * time.Minute

	// manifestHashBufferBytes 是算哈希时的读块大小。4MiB 让 1000GB 级大文件也能
	// 每秒多次上报"已读字节"，进度条不会卡在一个文件上纹丝不动。
	manifestHashBufferBytes = 4 << 20
)

// fsManifestRequest 是 POST /agent/fs/manifest/start 的请求体。
type fsManifestRequest struct {
	Path string `json:"path"`
	// Hash 为 true 时对每个文件计算 SHA-256（耗时与总字节数成正比）。
	Hash bool `json:"hash"`
}

// fsManifestEntry 是交给服务端做上传核对的一项。
type fsManifestEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Hash 为空表示没算/算失败：服务端退化为只比大小，避免误报“内容不同”。
	Hash string `json:"hash,omitempty"`
}

// ManifestStatus 是清单任务的进度快照。
type ManifestStatus struct {
	ManifestID string `json:"manifest_id"`
	State      string `json:"state"`
	// Phase 是 scan | hash | done。
	Phase string `json:"phase"`
	// Done/Total 是已处理/总文件数；扫描阶段 Total 未知（0）。
	Done int `json:"done"`
	// Total 是过滤掉卷根系统元数据后的文件总数；扫描阶段为 0。
	Total int `json:"total"`
	// Bytes/TotalBytes 是已处理/总字节；扫描阶段 TotalBytes 为 0。
	Bytes      int64 `json:"bytes"`
	TotalBytes int64 `json:"total_bytes"`
	// Current 是此刻正在处理的文件（相对源目录的斜杠路径）。
	Current string `json:"current,omitempty"`
	// ErrorCode/ErrorMessage 只在 state=failed 时有值。
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// ManifestResult 是 POST /agent/fs/manifest/result 的响应。
type ManifestResult struct {
	Path       string            `json:"path"`
	FileCount  int               `json:"file_count"`
	TotalBytes int64             `json:"total_bytes"`
	Entries    []fsManifestEntry `json:"entries"`
	Warnings   []string          `json:"warnings"`
}

type manifestTask struct {
	id   string
	raw  string
	hash bool
	ctx  context.Context

	mu   sync.Mutex
	snap ManifestStatus
	// entries/warnings/root 只在成功后可读（由 snap.State 决定）。
	entries  []fsManifestEntry
	warnings []string
	root     string
	// finishedAt 用于回收内存；零值表示还没结束。
	finishedAt time.Time
}

type manifestManager struct {
	mu    sync.Mutex
	tasks map[string]*manifestTask
}

func newManifestManager() *manifestManager {
	return &manifestManager{tasks: make(map[string]*manifestTask)}
}

// create 登记一个新任务，并顺手回收过期的已完成任务。
func (m *manifestManager) create(ctx context.Context, raw string, hash bool) *manifestTask {
	t := &manifestTask{id: uuid.NewString(), raw: raw, hash: hash, ctx: ctx}
	t.snap = ManifestStatus{ManifestID: t.id, State: manifestStateRunning, Phase: manifestPhaseScan}
	m.mu.Lock()
	m.gcLocked()
	m.tasks[t.id] = t
	m.mu.Unlock()
	return t
}

func (m *manifestManager) get(id string) *manifestTask {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

func (m *manifestManager) gcLocked() {
	cutoff := time.Now().Add(-manifestTaskTTL)
	for id, t := range m.tasks {
		t.mu.Lock()
		finished := !t.finishedAt.IsZero() && t.finishedAt.Before(cutoff)
		t.mu.Unlock()
		if finished {
			delete(m.tasks, id)
		}
	}
}

// update 更新进度快照。
func (t *manifestTask) update(phase string, done, total int, bytes, totalBytes int64, current string) {
	t.mu.Lock()
	t.snap.Phase = phase
	t.snap.Done = done
	t.snap.Total = total
	t.snap.Bytes = bytes
	if totalBytes > 0 {
		t.snap.TotalBytes = totalBytes
	}
	t.snap.Current = current
	t.mu.Unlock()
}

func (t *manifestTask) setTotals(total int, totalBytes int64) {
	t.mu.Lock()
	t.snap.Total = total
	t.snap.TotalBytes = totalBytes
	t.mu.Unlock()
}

func (t *manifestTask) succeed(res *scanResult, entries []fsManifestEntry) {
	t.mu.Lock()
	t.snap.State = manifestStateSucceeded
	t.snap.Phase = manifestPhaseDone
	t.snap.Done = len(entries)
	t.snap.Total = len(entries)
	t.snap.Bytes = t.snap.TotalBytes
	t.snap.Current = ""
	t.entries = entries
	t.warnings = res.Warnings
	t.root = res.Root
	t.finishedAt = time.Now()
	t.mu.Unlock()
}

func (t *manifestTask) fail(err error) {
	code := apperr.CodeOf(err)
	msg := err.Error()
	if code == apperr.CodeInternal {
		// internal 的 Error() 也带稳定前缀，保留原文便于排查。
		msg = err.Error()
	}
	t.mu.Lock()
	t.snap.State = manifestStateFailed
	t.snap.Current = ""
	t.snap.ErrorCode = code
	t.snap.ErrorMessage = msg
	t.finishedAt = time.Now()
	t.mu.Unlock()
}

// status 返回快照。
func (t *manifestTask) status() ManifestStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snap
}

// result 返回成功后的完整清单；未成功时 ok=false。
func (t *manifestTask) result() (ManifestResult, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.snap.State != manifestStateSucceeded {
		return ManifestResult{}, false
	}
	warnings := t.warnings
	if warnings == nil {
		warnings = []string{}
	}
	return ManifestResult{
		Path:       t.root,
		FileCount:  len(t.entries),
		TotalBytes: t.snap.TotalBytes,
		Entries:    t.entries,
		Warnings:   warnings,
	}, true
}

// handleStartManifest 启动一次本地清单生成，立即返回 manifest_id。
func (a *Agent) handleStartManifest(w http.ResponseWriter, r *http.Request) {
	var req fsManifestRequest
	if err := decodeJSON(r, &req); err != nil {
		a.writeError(w, err)
		return
	}
	// 先校验一次路径，把"路径不存在/不是目录"这类错误同步返回，不必让用户等轮询。
	if _, err := validateAbsDir(req.Path, "path"); err != nil {
		a.writeError(w, err)
		return
	}
	t := a.manifestMgr().create(context.Background(), req.Path, req.Hash)
	go a.runManifest(t)
	a.writeJSON(w, http.StatusAccepted, map[string]any{"manifest_id": t.id})
}

// manifestMgr 惰性初始化清单管理器（测试里手工构造的 Agent 可能没有它）。
func (a *Agent) manifestMgr() *manifestManager {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.manifests == nil {
		a.manifests = newManifestManager()
	}
	return a.manifests
}

// handleManifestStatus 返回进度快照（界面按 0.5~1s 轮询）。
func (a *Agent) handleManifestStatus(w http.ResponseWriter, r *http.Request) {
	t := a.manifestMgr().get(strings.TrimSpace(r.URL.Query().Get("id")))
	if t == nil {
		a.writeError(w, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
		return
	}
	a.writeJSON(w, http.StatusOK, t.status())
}

// handleManifestResult 返回完整清单（只在成功后可用）。
func (a *Agent) handleManifestResult(w http.ResponseWriter, r *http.Request) {
	t := a.manifestMgr().get(strings.TrimSpace(r.URL.Query().Get("id")))
	if t == nil {
		a.writeError(w, apperr.New(apperr.CodeNotFound, http.StatusNotFound))
		return
	}
	res, ok := t.result()
	if !ok {
		st := t.status()
		code := "agent.manifest_not_ready"
		if st.State == manifestStateFailed {
			code = st.ErrorCode
			if code == "" {
				code = "agent.manifest_failed"
			}
		}
		a.writeError(w, apperr.New(code, http.StatusConflict))
		return
	}
	a.writeJSON(w, http.StatusOK, res)
}

// runManifest 是清单任务的主体：扫描 → （可选）逐文件 SHA-256 → 完成。
func (a *Agent) runManifest(t *manifestTask) {
	root, err := validateAbsDir(t.raw, "path")
	if err != nil {
		t.fail(err)
		return
	}
	res, err := scanLocalDirWith(root, func(files int, bytes int64, rel string) {
		// 扫描阶段总字节未知，只报累计发现量。
		t.update(manifestPhaseScan, files, 0, bytes, 0, rel)
	})
	if err != nil {
		t.fail(err)
		return
	}

	entries := make([]fsManifestEntry, 0, len(res.Entries))
	var totalBytes int64
	for _, e := range res.Entries {
		// 与 service 端 walkVerifyRoot 一致：卷根系统元数据不算上传内容。
		if isVerifyRootMetadata(e.Rel) {
			continue
		}
		entries = append(entries, fsManifestEntry{Path: e.Rel, Size: e.Size})
		totalBytes += e.Size
	}
	if len(entries) == 0 {
		t.fail(errScanEmpty())
		return
	}
	t.setTotals(len(entries), totalBytes)
	if !t.hash {
		t.succeed(res, entries)
		return
	}

	var doneBytes int64
	for i := range entries {
		if t.ctx.Err() != nil {
			t.fail(t.ctx.Err())
			return
		}
		entry := &entries[i]
		t.update(manifestPhaseHash, i, len(entries), doneBytes, totalBytes, entry.Path)
		abs := filepath.Join(root, filepath.FromSlash(entry.Path))
		sum, hashErr := hashLocalFileProgress(abs, func(n int64) {
			doneBytes += n
			t.update(manifestPhaseHash, i, len(entries), doneBytes, totalBytes, entry.Path)
		})
		if hashErr != nil {
			res.Warnings = append(res.Warnings, "hash_failed:"+entry.Path)
		} else {
			entry.Hash = sum
		}
		t.update(manifestPhaseHash, i+1, len(entries), doneBytes, totalBytes, entry.Path)
	}
	t.succeed(res, entries)
}

// hashLocalFileProgress 计算文件的 SHA-256，并在每次读块后回调已读字节数。
func hashLocalFileProgress(path string, onRead func(int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buf := make([]byte, manifestHashBufferBytes)
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

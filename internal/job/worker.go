// Package job 实现异步任务执行。
//
// 设计依据（见 docs/implementation.md 5.12）：
//   - 所有分钟级以上的操作（建盘、从目录建盘、复制、发布、回收）都必须走任务，
//     避免 HTTP 请求超时、无法展示进度、无法重试；
//   - 幂等：相同 idem_key 的任务只执行一次；
//   - 串行化：相同 lock_key 的任务串行执行（例如同一磁盘的操作）。
package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"vault/internal/domain"
	"vault/internal/lock"
	"vault/internal/store"
)

// Reporter 供 handler 上报进度。
type Reporter interface {
	// Progress 上报 0-100 的进度。越界值会被钳制。
	Progress(percent int)
}

// ReporterFunc 把普通函数适配为 Reporter。
type ReporterFunc func(percent int)

// Progress 实现 Reporter。
func (f ReporterFunc) Progress(percent int) { f(percent) }

// Handler 是任务处理器。每种任务类型注册一个。
type Handler interface {
	// JobType 返回该 handler 处理的任务类型。
	JobType() domain.JobType
	// Execute 执行任务。返回 nil 视为成功。
	// ctx 会在服务端关闭或任务取消时被取消。
	Execute(ctx context.Context, j *domain.Job, rep Reporter) error
}

// HandlerFunc 把函数适配为 Handler。
type HandlerFunc struct {
	T domain.JobType
	F func(ctx context.Context, j *domain.Job, rep Reporter) error
}

// JobType 实现 Handler。
func (h HandlerFunc) JobType() domain.JobType { return h.T }

// Execute 实现 Handler。
func (h HandlerFunc) Execute(ctx context.Context, j *domain.Job, rep Reporter) error {
	return h.F(ctx, j, rep)
}

// Options 是 Worker 配置。
type Options struct {
	// Concurrency 并发执行的任务数上限，默认 4。
	Concurrency int
	// PollInterval 空闲时的轮询间隔，默认 1s。
	PollInterval time.Duration
	// MaxAttempt 最大尝试次数，超过则标记为失败，默认 3。
	MaxAttempt int
	// Logger 日志器。
	Logger *slog.Logger
}

// Worker 从数据库领取任务并执行。
type Worker struct {
	store  *store.Store
	locks  *lock.Keyed
	log    *slog.Logger
	opts   Options
	mu     sync.RWMutex
	byType map[domain.JobType]Handler

	// onFinished 任务结束回调（可选），用于推送 SSE 通知。
	onFinished func(j *domain.Job)

	// onPanic handler 发生 panic 时的回调（可选），用于把 panic 落进审计/告警。
	//
	// 之所以做成回调而不是在 job 包直接写库：internal/job 不应反向依赖 internal/app，
	// 由装配方（cmd/vault-server）把审计能力接进来。
	onPanic func(ctx context.Context, j *domain.Job, panicValue any, stack string)

	// onFinalFailure 任务**重试用尽、确定失败**时的回调（可选）。
	//
	// 与 onFinished 的区别：onFinished 在每个终态（成功/取消/失败）都会调用；
	// 本回调只在"不会再重试"时调用，供装配方把资源从中间态回滚。
	// 典型场景：建盘任务彻底失败后把磁盘从 creating 置为 error —— 否则磁盘永远停在
	// 中间态，而挂载/发布路径会一直等 ready，表现为"每次点挂载都报 disk.not_ready
	// 且永远不会变好"（对账也刻意跳过 creating，救不了它）。
	onFinalFailure func(ctx context.Context, j *domain.Job, cause error)
}

// NewWorker 构造 Worker。
func NewWorker(st *store.Store, locks *lock.Keyed, opts Options) *Worker {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 4
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.MaxAttempt <= 0 {
		opts.MaxAttempt = 3
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Worker{
		store:  st,
		locks:  locks,
		log:    opts.Logger,
		opts:   opts,
		byType: make(map[domain.JobType]Handler),
	}
}

// Register 注册任务处理器。
func (w *Worker) Register(h Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.byType[h.JobType()] = h
}

// OnFinished 设置任务结束回调。
func (w *Worker) OnFinished(fn func(j *domain.Job)) {
	w.onFinished = fn
}

// OnPanic 设置任务 handler panic 回调。
//
// 与 OnFinished 一样在 Start 之前调用（Worker 不持锁读该字段）。
func (w *Worker) OnPanic(fn func(ctx context.Context, j *domain.Job, panicValue any, stack string)) {
	w.onPanic = fn
}

// OnFinalFailure 设置"任务重试用尽、确定失败"的回调。
//
// 与 OnFinished / OnPanic 一样在 Start 之前调用（Worker 不持锁读该字段）。
func (w *Worker) OnFinalFailure(fn func(ctx context.Context, j *domain.Job, cause error)) {
	w.onFinalFailure = fn
}

// Enqueue 提交任务。
//
// 幂等：若 idemKey 非空且已存在同键任务，直接返回已有任务并置 created=false。
// 调用方应据此避免重复触发长任务。
func (w *Worker) Enqueue(ctx context.Context, j *domain.Job) (job *domain.Job, created bool, err error) {
	id, created, err := w.store.CreateJob(ctx, j)
	if err != nil {
		return nil, false, err
	}
	got, err := w.store.GetJob(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return got, created, nil
}

// EnqueueWith 是 Enqueue 的便捷封装：自动序列化 payload 并生成幂等键。
func (w *Worker) EnqueueWith(ctx context.Context, t domain.JobType, refID, idemKey, lockKey string, payload any) (*domain.Job, bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	return w.Enqueue(ctx, &domain.Job{
		Type:    t,
		RefID:   refID,
		IdemKey: idemKey,
		LockKey: lockKey,
		Payload: string(raw),
		State:   domain.JobStatePending,
	})
}

// Start 启动执行循环，直到 ctx 结束。
func (w *Worker) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.opts.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			w.loop(ctx, workerID)
		}(i)
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, workerID int) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		handled, err := w.tick(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("任务轮询失败", "error", err, "worker", workerID)
		}

		// 有活干则立刻继续轮询，空闲则退避。
		delay := w.opts.PollInterval
		if handled {
			delay = 0
		}
		timer.Reset(delay)
	}
}

// tick 领取并执行一批任务，返回是否处理过任务。
func (w *Worker) tick(ctx context.Context) (bool, error) {
	jobs, err := w.store.ClaimJobs(ctx, w.opts.Concurrency)
	if err != nil {
		return false, err
	}
	if len(jobs) == 0 {
		return false, nil
	}

	for i := range jobs {
		j := &jobs[i]
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		w.runOne(ctx, j)
	}
	return true, nil
}

func (w *Worker) runOne(ctx context.Context, j *domain.Job) {
	w.mu.RLock()
	handler, ok := w.byType[j.Type]
	w.mu.RUnlock()

	if !ok {
		w.log.Error("未注册的任务类型", "type", j.Type, "job_id", j.ID)
		w.finish(ctx, j, domain.JobStateFailed, "未注册的任务类型: "+string(j.Type))
		return
	}

	// 按 lock_key 串行：同一资源（如同一磁盘）的操作不能并发。
	var release func()
	if j.LockKey != "" {
		release = w.locks.Lock(j.LockKey)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()

	rep := ReporterFunc(func(percent int) {
		if err := w.store.UpdateJobProgress(ctx, j.ID, percent); err != nil {
			w.log.Warn("更新任务进度失败", "job_id", j.ID, "error", err)
		}
	})

	log := w.log.With("job_id", j.ID, "type", j.Type, "ref_id", j.RefID, "attempt", j.Attempt)
	log.Info("任务开始")
	start := time.Now()

	execErr := normalizeNilError(w.execute(ctx, handler, j, rep))
	cost := time.Since(start)

	switch {
	case execErr == nil:
		log.Info("任务成功", "cost_ms", cost.Milliseconds())
		w.finish(ctx, j, domain.JobStateSucceeded, "")
	case errors.Is(execErr, context.Canceled):
		log.Warn("任务被取消", "cost_ms", cost.Milliseconds())
		w.finish(ctx, j, domain.JobStateCancelled, "已取消")
	default:
		// 未达最大尝试次数时退回 pending，实现重试。
		if j.Attempt < w.opts.MaxAttempt {
			log.Warn("任务失败，将重试", "error", execErr, "cost_ms", cost.Milliseconds())
			if err := w.retry(ctx, j, execErr); err != nil {
				log.Error("任务重试入队失败", "error", err)
				w.finish(ctx, j, domain.JobStateFailed, execErr.Error())
			}
			return
		}
		log.Error("任务失败且已达最大尝试次数", "error", execErr, "cost_ms", cost.Milliseconds())
		if w.onFinalFailure != nil {
			// 回滚仍要能写库，因此用独立 context：本轮 ctx 可能已取消/超时。
			// 回调内部不得 panic（装配方负责兜底），这里也不再包一层 recover，
			// 以免把"任务失败"变成"进程被打挂"。
			failCtx, cancelFail := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			w.onFinalFailure(failCtx, j, execErr)
			cancelFail()
		}
		w.finish(ctx, j, domain.JobStateFailed, execErr.Error())
	}
}

// normalizeNilError 把"接口非 nil、内部却是 nil 指针"的错误（typed nil）归一为 nil。
//
// 为什么必须有这道防线：这类假错误会被 `if err != nil` 判成失败，更危险的是
// `errors.Is(err, X)` / `errors.As(err, &y)` 会对 nil 接收者解引用，
// 在 **worker 协程**里直接 panic → 整个服务端进程退出
// （真实事故：Windows 下创建磁盘必崩，根因见 platform/winvhd 的 runVHD）。
//
// 正常代码不应产生 typed nil（产生它的地方已修），这里只作为最后一道兜底：
// 宁可把这种错误当成"成功"，也绝不让一个任务打挂整个进程。
func normalizeNilError(err error) error {
	if err == nil {
		return nil
	}
	switch v := reflect.ValueOf(err); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return nil
		}
	}
	return err
}

// execute 执行 handler，并对 panic 做兜底，避免单个任务拖垮整个 worker。
//
// panic 必须留下**带栈**的 ERROR 与（可选）一条回调记录：否则只能看到
// "任务执行发生内部错误"，无法定位是哪一行崩的（建盘这类异步任务尤其如此）。
func (w *Worker) execute(ctx context.Context, h Handler, j *domain.Job, rep Reporter) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			w.log.Error("任务执行 panic",
				"job_id", j.ID, "type", j.Type, "panic", r, "stack", stack)
			if w.onPanic != nil {
				w.onPanic(ctx, j, r, stack)
			}
			err = errors.New("任务执行发生内部错误")
		}
	}()
	return h.Execute(ctx, j, rep)
}

// PanicSummary 把一次 panic 值与堆栈折叠为适合落库的一行摘要（完整堆栈仍在日志文件里）。
//
// 导出给装配方复用，避免审计里塞进几十 KB 的堆栈把审计页撑坏。
func PanicSummary(panicValue any, stack string) string {
	return "panic=" + headLines(fmt.Sprint(panicValue), 1) +
		" stack=" + headLines(stack, 3)
}

// headLines 取文本前 n 行并以 " | " 拼接。
func headLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.SplitN(strings.TrimRight(s, "\n"), "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}

func (w *Worker) finish(ctx context.Context, j *domain.Job, st domain.JobState, lastErr string) {
	// 任务结束后用独立 context，避免因上层 ctx 取消而写不进终态。
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := w.store.FinishJob(finishCtx, j.ID, st, lastErr); err != nil {
		w.log.Error("写入任务终态失败", "job_id", j.ID, "error", err)
	}
	if w.onFinished != nil {
		j.State = st
		j.LastError = lastErr
		w.onFinished(j)
	}
}

func (w *Worker) retry(ctx context.Context, j *domain.Job, cause error) error {
	retryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return w.store.ResetJobToPending(retryCtx, j.ID, cause.Error())
}

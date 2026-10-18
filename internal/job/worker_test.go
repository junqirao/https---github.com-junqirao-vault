package job

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// nilTypedError 返回"接口非 nil、内部指针为 nil"的假错误（typed nil）。
type nilTypedError struct{}

func (*nilTypedError) Error() string { return "typed nil" }

func typedNilError() error {
	var e *nilTypedError
	return e
}

// TestNormalizeNilError 覆盖 worker 的最后一道防线。
//
// 背景：Windows 下创建磁盘时，VirtDisk 调用成功却返回了 typed nil 错误
// （见 platform/winvhd 的 runVHD），worker 里的 errors.Is(execErr, context.Canceled)
// 会对 nil 接收者解引用，直接 panic 打挂整个服务端进程。
func TestNormalizeNilError(t *testing.T) {
	if err := normalizeNilError(nil); err != nil {
		t.Fatalf("nil 应保持 nil，得到 %v", err)
	}
	if err := normalizeNilError(typedNilError()); err != nil {
		t.Fatalf("typed nil 应被归一为 nil，得到 %T(%v)", err, err)
	}
	real := errors.New("真实错误")
	if err := normalizeNilError(real); !errors.Is(err, real) {
		t.Fatalf("真实错误不应被丢弃")
	}
	wrapped := fmt.Errorf("包装: %w", real)
	if err := normalizeNilError(wrapped); !errors.Is(err, real) {
		t.Fatalf("包装错误不应被丢弃")
	}
}

// TestTypedNilErrorDoesNotPanicOnErrorsIs 固化"崩溃场景"本身的期望行为：
// 归一之后再做 errors.Is 不得 panic。
func TestTypedNilErrorDoesNotPanicOnErrorsIs(t *testing.T) {
	err := normalizeNilError(typedNilError())
	if errors.Is(err, context.Canceled) {
		t.Fatalf("归一后的 nil 不应匹配 context.Canceled")
	}
}

// TestCancelRunningJobCancelsContext 锁定"取消正在执行的任务"的核心机制：
// Cancel 必须取消该任务的子 ctx —— 正在跑的 PowerShell 靠 exec.CommandContext 被杀掉，
// 任务随即以 cancelled 收场并释放它持有的锁（真实诉求："任务一直卡着，给个取消按钮"）。
func TestCancelRunningJobCancelsContext(t *testing.T) {
	w := NewWorker(nil, nil, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w.cancelMu.Lock()
	w.running["job-1"] = cancel
	w.cancelMu.Unlock()

	if err := w.Cancel(context.Background(), "job-1"); err != nil {
		t.Fatalf("取消正在执行的任务不应报错：%v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("Cancel 应取消该任务的子 ctx")
	}
}

// TestBeginJobHandlesCancelRequest 锁定"取消请求落在'被领取'与'登记'之间"的兜底：
// beginJob 必须原子地"查取消请求 + 登记 running"，被请求取消的任务在开跑前就被拦下。
func TestBeginJobHandlesCancelRequest(t *testing.T) {
	w := NewWorker(nil, nil, Options{})
	parent := context.Background()

	// 预登记取消请求：beginJob 应返回 cancelled=true，且不再登记 running。
	w.cancelRequested["job-2"] = true
	_, cancel, cancelled := w.beginJob(parent, "job-2")
	defer cancel()
	if !cancelled {
		t.Fatal("已请求取消的任务应在开跑前被拦下")
	}
	w.cancelMu.Lock()
	_, stillRequested := w.cancelRequested["job-2"]
	_, registered := w.running["job-2"]
	w.cancelMu.Unlock()
	if stillRequested {
		t.Fatal("取消请求被处理后应清掉标记")
	}
	if registered {
		t.Fatal("被取消的任务不应登记 running")
	}

	// 正常任务：登记 running，且 ctx 可被取消。
	jobCtx, cancel2, cancelled2 := w.beginJob(parent, "job-3")
	defer cancel2()
	if cancelled2 {
		t.Fatal("未请求取消的任务不应被拦下")
	}
	w.cancelMu.Lock()
	_, registered = w.running["job-3"]
	w.cancelMu.Unlock()
	if !registered {
		t.Fatal("正常任务应登记 running")
	}
	w.endJob("job-3")
	if jobCtx.Err() != nil {
		t.Fatal("正常任务的 ctx 不应被取消")
	}
}

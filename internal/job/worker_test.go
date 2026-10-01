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

package apperr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
)

// typedNil 返回一个"接口非 nil、内部指针为 nil"的错误（typed nil）。
//
// 这是真实事故的成因：某个 `func(...) *apperr.Error` 在成功分支直接 return nil，
// 调用方却把它当 error 返回，于是 `err != nil` 成立、errors.Is/As 解引用时 panic
// （曾导致 Windows 下每次创建磁盘都把服务端进程打挂）。
func typedNil() error {
	var e *Error
	return e
}

// TestTypedNilIsSafe 保证 apperr 的各个入口对 typed nil 都安全。
func TestTypedNilIsSafe(t *testing.T) {
	var nilErr error = typedNil()

	// errors.Is 会调用 Unwrap：nil 接收者必须不 panic（这正是当初的崩溃点）。
	if errors.Is(nilErr, context.Canceled) {
		t.Fatalf("typed nil 不应匹配任何目标错误")
	}

	// As 必须把 typed nil 视为"不是业务错误"，否则调用方读 e.Code 会 panic。
	if e, ok := As(nilErr); ok || e != nil {
		t.Fatalf("As(typed nil) = (%v, %v)，期望 (nil, false)", e, ok)
	}

	// CodeOf / HTTPStatus 依赖 As，同样必须安全。
	if got := CodeOf(nilErr); got != CodeInternal {
		t.Fatalf("CodeOf(typed nil) = %q，期望 %q", got, CodeInternal)
	}
	if got := HTTPStatus(nilErr); got != 500 {
		t.Fatalf("HTTPStatus(typed nil) = %d，期望 500", got)
	}
	if got := nilErr.Error(); got != "" {
		t.Fatalf("typed nil 的 Error() = %q，期望空串", got)
	}
}

// TestNilReceiverMethods 直接对 nil 接收者调用方法也不得 panic。
func TestNilReceiverMethods(t *testing.T) {
	var e *Error
	if e.Error() != "" {
		t.Fatalf("nil 接收者 Error() 期望空串")
	}
	if e.Unwrap() != nil {
		t.Fatalf("nil 接收者 Unwrap() 期望 nil")
	}
}

// TestNormalErrorChain 确保正常错误的 Is / As / HTTPStatus 语义未被削弱。
func TestNormalErrorChain(t *testing.T) {
	cause := errors.New("底层原因")
	err := New("repo.not_found", 404).WithArg("id", "r1").WithCause(cause)

	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is 应能沿 Cause 链匹配到底层错误")
	}
	// 顺带覆盖"不 panic 且不误匹配"：正常错误不该命中无关目标。
	if errors.Is(err, context.Canceled) {
		t.Fatalf("正常错误不应匹配 context.Canceled")
	}
	e, ok := As(err)
	if !ok || e == nil || e.Code != "repo.not_found" || e.Args["id"] != "r1" {
		t.Fatalf("As 未取到业务错误：%v %v", e, ok)
	}
	if HTTPStatus(err) != 404 {
		t.Fatalf("HTTPStatus = %d，期望 404", HTTPStatus(err))
	}
}

// TestLogValueExposesDiagnosis 钉住"日志里必须看得见失败原因"这条约定。
//
// 事故背景：Error() 按约定只返回 Code（Args 是留给前端 i18n 插值的），于是
// `slog.Error(..., "error", err)` 落盘只有一句 system.unavailable，component / hint
// 全部丢失。现场因此分不清是内核侧（iscsi_target_mod 未注册 iscsi fabric）还是用户态
// （targetcli 未安装）——这两者处置方式完全不同，只能靠猜，最后猜到了错误的方向。
//
// 用真实的 slog JSON handler 断言，而不是只测 LogValue 的返回值：要保证的其实是
// "slog 的 Any 会解析 LogValuer 并展开成对象"（见 log/slog Value.Kind 的 LogValuer 分支）。
func TestLogValueExposesDiagnosis(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	err := New(CodeUnavailable, 500).
		WithArg("component", "iscsi_fabric").
		WithArg("hint", "modprobe iscsi_target_mod").
		WithCause(errors.New("fabric 目录不存在"))

	logger.Error("就绪判定失败", "error", err)

	var rec map[string]any
	if uErr := json.Unmarshal(buf.Bytes(), &rec); uErr != nil {
		t.Fatalf("日志不是合法 JSON: %v（%s）", uErr, buf.String())
	}
	got, ok := rec["error"].(map[string]any)
	if !ok {
		t.Fatalf("error 字段应渲染成对象（带 Args），实际 %T：%s", rec["error"], buf.String())
	}
	for k, want := range map[string]string{
		"code":      CodeUnavailable,
		"component": "iscsi_fabric",
		"hint":      "modprobe iscsi_target_mod",
		"cause":     "fabric 目录不存在",
	} {
		if got[k] != want {
			t.Fatalf("日志字段 error.%s = %v，期望 %q；完整日志：%s", k, got[k], want, buf.String())
		}
	}
}

// TestLogValueNilReceiver LogValue 对 nil 接收者必须安全（与 Error/Unwrap 一致）。
func TestLogValueNilReceiver(t *testing.T) {
	var e *Error
	if got := e.LogValue(); got.Kind() != slog.KindString || got.String() != "" {
		t.Fatalf("nil 接收者 LogValue() = %v，期望空字符串值", got)
	}
}

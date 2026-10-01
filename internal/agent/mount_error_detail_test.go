package agent

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// TestMountErrorDetailOf 覆盖"挂载失败原始报错"的提取与策略边界。
//
// 两个方向都要焊住：
//   - 取得到：界面上只看得到稳定码时用户一头雾水（真实工单："客户端报错至少要知道为什么"）；
//   - 不外泄：原始报错**绝不能**出现在 Error() 文本里，否则它会随每个 API 错误响应泄漏出去
//     （这就是它当初被设计成"只进日志"的原因）。
func TestMountErrorDetailOf(t *testing.T) {
	// 模拟平台层的真实构造：errPSFailed(cause).WithArg(...)。
	scriptErr := &winps.ScriptError{Reason: "connect_failed", Step: "connect", Message: "Access is denied."}
	err := apperr.New(winps.CodePSFailed, http.StatusInternalServerError).
		WithCause(scriptErr).
		WithArg("reason", "connect_failed")

	t.Run("提取 reason@step 与原始报错", func(t *testing.T) {
		if got, want := mountErrorDetailOf(err), "connect_failed@connect: Access is denied."; got != want {
			t.Fatalf("detail = %q，期望 %q", got, want)
		}
	})

	t.Run("原始报错不得进入 Error() 文本", func(t *testing.T) {
		if strings.Contains(err.Error(), "Access is denied") {
			t.Fatalf("原始报错泄漏进了错误文本（会随 API 响应外泄）：%q", err.Error())
		}
	})

	t.Run("只有 reason 没有 message 时退化为 reason", func(t *testing.T) {
		only := &winps.ScriptError{Reason: "exec_failed"}
		got := mountErrorDetailOf(apperr.New(winps.CodePSFailed, 500).WithCause(only))
		if got != "exec_failed" {
			t.Fatalf("detail = %q，期望 exec_failed", got)
		}
	})

	t.Run("非 PowerShell 失败返回空（不硬凑细节）", func(t *testing.T) {
		if got := mountErrorDetailOf(errors.New("plain error")); got != "" {
			t.Fatalf("detail = %q，期望空串", got)
		}
		if got := mountErrorDetailOf(nil); got != "" {
			t.Fatalf("nil 应为空串，实际 %q", got)
		}
	})

	t.Run("超长报错必须截断（状态要经 SSE 推送并落盘）", func(t *testing.T) {
		huge := &winps.ScriptError{Reason: "x", Message: strings.Repeat("啊", 600)}
		got := mountErrorDetailOf(apperr.New(winps.CodePSFailed, 500).WithCause(huge))
		if runes := []rune(got); len(runes) > maxMountErrorDetailRunes+1 {
			t.Fatalf("截断后长度 = %d 字符，期望不超过 %d(+省略号)", len(runes), maxMountErrorDetailRunes)
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("截断后应以省略号结尾，实际 %q", got)
		}
	})
}

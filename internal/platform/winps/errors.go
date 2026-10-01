// Package winps 提供统一的 Windows PowerShell 执行器。
//
// 设计约束（见 docs/implementation.md 6.6）：
//   - 参数一律通过 `-File script.ps1 -Param value` 或 `-EncodedCommand` 传递，
//     绝不把调用方提供的值拼接进脚本文本（防命令注入）；
//   - 脚本统一输出压缩 JSON：成功时 stdout 最后一行形如 {"ok":true,...}，
//     失败时输出 {"ok":false,"message":"..."} 并以非 0 退出码结束；
//   - 原始 PowerShell 报错文本**不进错误码、不进 Error() 文本、不进 API 响应**，
//     只进日志；唯一例外是 ScriptError.Message（见其说明）——它仅供**本机诊断展示**
//     使用，调用方必须刻意取出，不会随错误自动外泄。
package winps

import (
	"net/http"

	"vault/internal/apperr"
)

// 平台层错误码。
const (
	// CodePSFailed PowerShell 脚本执行失败（原始报错文本只进日志）。
	CodePSFailed = "platform.ps_failed"
	// CodePSUnavailable 探测不到可用的 PowerShell。
	CodePSUnavailable = "platform.powershell_unavailable"
)

// errPSFailed 构造统一的脚本执行失败错误。
//
// cause 不得包含原始 PowerShell 报错文本（只允许携带退出状态、context 错误等自身信息），
// 因为 apperr.Error 会对外暴露 Cause 的文本。
func errPSFailed(cause error) *apperr.Error {
	e := apperr.New(CodePSFailed, http.StatusInternalServerError)
	if cause != nil {
		e.Cause = cause
	}
	return e
}

// ScriptError 表示脚本通过 {"ok":false,"reason":"..."} 结构化上报的失败。
//
// Error() 只暴露稳定的 reason / step，**不含原始 PowerShell 文本**，因此可作为 Cause
// 安全地传给上层（不会随 API 错误响应外泄）；上层可用 errors.As 取出并映射为更精确的业务错误码。
type ScriptError struct {
	Reason string
	// Step 脚本内"当前进行到哪一步"（可空）。同一 reason 覆盖多步时靠它定位。
	Step string
	// Message 是脚本自报的原始报错文本（$_.Exception.Message）。
	//
	// ⚠️ **刻意不参与 Error() 文本**：它只供上层**主动**取用做本机诊断展示
	// （见 internal/agent 的 mountErrorDetailOf → 挂载状态的 last_error_detail）。
	// 这样"原始报错只进日志"的对外约定依然成立：常规错误响应里看到的仍只有稳定码。
	//
	// 脚本自身已约定不回显 CHAP 密钥（各 catch 分支均只取 Exception.Message）。
	Message string
}

func (e *ScriptError) Error() string {
	if e.Reason == "" {
		return "powershell script reported failure"
	}
	if e.Step != "" {
		return "powershell script reported failure: " + e.Reason + "@" + e.Step
	}
	return "powershell script reported failure: " + e.Reason
}

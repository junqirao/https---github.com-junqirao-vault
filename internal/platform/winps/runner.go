package winps

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"

	"vault/internal/apperr"
)

const (
	// DefaultPwshPath 默认的 PowerShell 可执行文件。
	DefaultPwshPath = "powershell.exe"
	// DefaultTimeout 默认的脚本执行超时。
	DefaultTimeout = 5 * time.Minute
	// waitDelay 进程被取消后等待其退出的额外时间，避免因管道未关闭而挂起。
	waitDelay = 10 * time.Second
	// logTextLimit 日志中保留的 stdout/stderr 最大字节数。
	logTextLimit = 8192
)

// baseArgs 是固定的 PowerShell 启动参数。
var baseArgs = []string{
	"-NoProfile",
	"-NonInteractive",
	"-ExecutionPolicy", "Bypass",
	"-OutputFormat", "Text",
}

// Options 是 Runner 的构造参数。
type Options struct {
	// PwshPath PowerShell 可执行文件路径，默认 "powershell.exe"。
	PwshPath string
	// Timeout 单次执行超时，默认 5 分钟。超时后子进程会被杀掉。
	Timeout time.Duration
	// Logger 日志器，可为 nil（使用 slog.Default）。
	Logger *slog.Logger
}

// Runner 是 PowerShell 执行器。零值不可用，请使用 NewRunner 构造。
type Runner struct {
	pwshPath string
	timeout  time.Duration
	logger   *slog.Logger
}

// NewRunner 构造执行器。
func NewRunner(opt Options) *Runner {
	r := &Runner{
		pwshPath: opt.PwshPath,
		timeout:  opt.Timeout,
		logger:   opt.Logger,
	}
	if r.pwshPath == "" {
		r.pwshPath = DefaultPwshPath
	}
	if r.timeout <= 0 {
		r.timeout = DefaultTimeout
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	return r
}

// Available 探测 PowerShell 是否可用（存在且能执行一条最简单的语句）。
func (r *Runner) Available(ctx context.Context) error {
	if _, err := exec.LookPath(r.pwshPath); err != nil {
		r.logger.Error("未找到 PowerShell 可执行文件", "path", r.pwshPath, "err", err)
		return apperr.New(CodePSUnavailable, 500)
	}
	if _, err := r.RunInline(ctx, "Write-Output 'ok'"); err != nil {
		r.logger.Error("PowerShell 不可执行", "path", r.pwshPath, "err", err)
		return apperr.New(CodePSUnavailable, 500)
	}
	return nil
}

// RunInline 执行一段内联脚本主体。
//
// body 只写业务主体，不需要自带 $ErrorActionPreference / try-catch —— 统一包装由本方法注入。
// 注意：body 内不得拼接调用方提供的值（防注入）；需要传值请改用 RunScript + 脚本参数。
func (r *Runner) RunInline(ctx context.Context, script string) ([]byte, error) {
	wrapped := wrapInline(script)
	args := append(append([]string{}, baseArgs...), "-EncodedCommand", encodeCommand(wrapped))
	r.logger.Debug("执行 PowerShell 内联脚本", "chars", len(wrapped))
	return r.exec(ctx, args)
}

// RunScript 执行磁盘上的 .ps1 脚本。
//
// args 以「参数名 + 值」成对给出（例如 "-Path", `D:\vault\x.vhdx`），
// 最终拼成 `-File <scriptPath> -Path D:\vault\x.vhdx`。
// 由于通过 exec 直接传参而非经过 shell，值不会被解释执行，因此是注入安全的。
func (r *Runner) RunScript(ctx context.Context, scriptPath string, args ...string) ([]byte, error) {
	full := make([]string, 0, len(baseArgs)+2+len(args))
	full = append(full, baseArgs...)
	full = append(full, "-File", scriptPath)
	full = append(full, args...)
	// 不记录 args 原文：其中可能包含 CHAP 密钥等敏感值。
	r.logger.Debug("执行 PowerShell 脚本", "script", scriptPath, "args_count", len(args)/2)
	return r.exec(ctx, full)
}

// RunJSON 执行内联脚本并解析其 JSON 输出。
func (r *Runner) RunJSON(ctx context.Context, script string, out any) error {
	stdout, err := r.RunInline(ctx, script)
	if err != nil {
		return err
	}
	return r.decode(stdout, out)
}

// RunScriptJSON 执行内嵌脚本（见 ScriptPath）并解析其 JSON 输出。
//
// Value 为空字符串的参数会被跳过，脚本使用 param() 的默认值；
// 因此需要表达「显式空值」时必须用非空表示（数组用 "[]"）。
func (r *Runner) RunScriptJSON(ctx context.Context, scriptName string, params []Param, out any) error {
	scriptPath, err := ScriptPath(scriptName)
	if err != nil {
		r.logger.Error("内嵌脚本物化失败", "script", scriptName, "err", err)
		return errPSFailed(err)
	}
	args := make([]string, 0, len(params)*2)
	for _, p := range params {
		if p.Name == "" || p.Value == "" {
			continue
		}
		name := p.Name
		if !strings.HasPrefix(name, "-") {
			name = "-" + name
		}
		args = append(args, name, p.Value)
	}
	stdout, err := r.RunScript(ctx, scriptPath, args...)
	if err != nil {
		return err
	}
	return r.decode(stdout, out)
}

// Param 是一个脚本参数。
type Param struct {
	Name  string
	Value string
}

// String 构造字符串参数。值为空时该参数不会被传递。
func String(name, value string) Param { return Param{Name: name, Value: value} }

// Bool 构造布尔参数，值为 "true" / "false"。
func Bool(name string, value bool) Param {
	if value {
		return Param{Name: name, Value: "true"}
	}
	return Param{Name: name, Value: "false"}
}

// JSONParam 构造 JSON 参数（数组/对象）。空切片会序列化为 "[]" 这种非空字符串，
// 从而能与「不传该参数」区分开。
func JSONParam(name string, v any) (Param, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Param{}, err
	}
	return Param{Name: name, Value: string(b)}, nil
}

// exec 执行 powershell 并返回 stdout。失败时返回平台的 AppError，原始报错只进日志。
func (r *Runner) exec(ctx context.Context, args []string) ([]byte, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, r.pwshPath, args...)
	// 统一消除控制台黑框：见 PrepareHiddenCommand 的说明。
	PrepareHiddenCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = waitDelay

	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		r.logger.Warn("PowerShell 执行超时或被取消",
			"err", ctx.Err().Error(),
			"stderr", truncate(stderr.String()),
			"stdout", truncate(stdout.String()))
		return stdout.Bytes(), errPSFailed(ctx.Err())
	case err != nil:
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		// 脚本约定失败时输出 {"ok":false,"reason":"..."} 并以非 0 退出。
		// 这里仍要提取结构化 reason，否则上层无法把失败映射为更精确的业务错误码。
		_, env := lastEnvelope(stdout.Bytes())
		if structured := r.envelopeFailure(env); structured != nil {
			r.logger.Debug("PowerShell 脚本以非 0 退出码结束（已提取结构化失败原因）",
				"exit_code", exitCode,
				"stdout", truncate(stdout.String()))
			return stdout.Bytes(), structured
		}
		r.logger.Error("PowerShell 执行失败",
			"exit_code", exitCode,
			"stderr", truncate(stderr.String()),
			"stdout", truncate(stdout.String()))
		// 脚本崩了、没吐出结构化 JSON：把 stderr（退化时用 stdout）作为原始细节带上去，
		// 否则本机诊断只能看到一句 platform.ps_failed，等于没线索。
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return stdout.Bytes(), errPSFailed(errors.Join(&ScriptError{
			Reason:  "exec_failed",
			Message: truncate(detail),
		}, err))
	}

	if s := strings.TrimSpace(stderr.String()); s != "" {
		r.logger.Debug("PowerShell 输出到 stderr", "stderr", truncate(s))
	}
	return stdout.Bytes(), nil
}

// envelope 是脚本 JSON 输出的控制字段。
type envelope struct {
	OK     *bool  `json:"ok"`
	Reason string `json:"reason"`
	// Step 是脚本内部"当前进行到哪一步"（如 set_enabled / set_chap / create_target）。
	//
	// 同一脚本里的多个步骤往往共用一个 reason，只有 reason 时无法定位；
	// step 是脚本自定义的稳定标识（不含原始报错文本），可安全进日志与对外错误参数。
	Step string `json:"step"`
	// ErrorType / Inner 是异常的 .NET 类型名与内层异常文本，**只进日志、不对外**。
	// 它们能把"CimException（服务/名称校验）"与"参数绑定失败"这类问题区分开。
	ErrorType string `json:"error_type"`
	Inner     string `json:"inner"`
	Message   string `json:"message"`
}

// lastEnvelope 解析 stdout 中最后一行合法 JSON。
//
// 从末尾向上扫描，跳过 PowerShell 的其他输出（verbose / 进度等），找到第一行可解析的 JSON。
func lastEnvelope(stdout []byte) (string, *envelope) {
	lines := strings.Split(strings.ReplaceAll(string(stdout), "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var env envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		return line, &env
	}
	return "", nil
}

// envelopeFailure 把脚本的结构化失败（ok=false）转换为平台错误。
//
// 稳定的 reason / step 进错误码与 args；原始 message 进日志，并**额外**挂在
// ScriptError.Message 上（不参与 Error() 文本），供上层主动取用做本机诊断展示。
func (r *Runner) envelopeFailure(env *envelope) error {
	if env == nil || env.OK == nil || *env.OK {
		return nil
	}
	// 原始报错文本、异常类型与内层异常：进日志；对外只经 ScriptError.Message（显式取用）。
	r.logger.Error("PowerShell 脚本报告失败",
		"reason", env.Reason,
		"step", env.Step,
		"error_type", env.ErrorType,
		"inner", truncate(env.Inner),
		"message", truncate(env.Message))
	var cause error
	if env.Reason != "" {
		cause = &ScriptError{Reason: env.Reason, Step: env.Step, Message: truncate(env.Message)}
	}
	if cause == nil {
		return errPSFailed(nil)
	}
	// reason / step 都是脚本自己定义的**稳定标识**（如 set_target_failed / set_enabled），
	// 不含 PowerShell 原始报错文本，因此可以安全地放进对外错误参数里：
	// 否则客户端只能看到 platform.ps_failed，"创建 iSCSI 目标失败"无从定位是哪一步。
	e := errPSFailed(cause).WithArg("reason", env.Reason)
	if env.Step != "" {
		e = e.WithArg("step", env.Step)
	}
	return e
}

// decode 解析脚本次成功执行时的 JSON 输出并反序列化到 out（out 可为 nil）。
func (r *Runner) decode(stdout []byte, out any) error {
	line, env := lastEnvelope(stdout)
	if env == nil {
		r.logger.Error("PowerShell 脚本没有输出合法 JSON", "stdout", truncate(string(stdout)))
		return errPSFailed(fmt.Errorf("no json output"))
	}
	if failure := r.envelopeFailure(env); failure != nil {
		return failure
	}
	if out != nil {
		if err := json.Unmarshal([]byte(line), out); err != nil {
			r.logger.Error("PowerShell 脚本输出 JSON 解析失败", "err", err.Error(), "line", truncate(line))
			return errPSFailed(err)
		}
	}
	return nil
}

// wrapInline 注入统一的错误处理包装，业务脚本只写主体。
func wrapInline(body string) string {
	const prelude = `$ErrorActionPreference='Stop';$ProgressPreference='SilentlyContinue';` +
		`try{[Console]::OutputEncoding=[System.Text.Encoding]::UTF8}catch{};`

	var b strings.Builder
	b.Grow(len(body) + 256)
	b.WriteString(prelude)
	b.WriteString("try {\n")
	b.WriteString(body)
	b.WriteString("\n} catch {\n")
	b.WriteString("[pscustomobject]@{ ok = $false; message = $_.Exception.Message } | ConvertTo-Json -Compress -Depth 5\n")
	b.WriteString("exit 1\n}\n")
	return b.String()
}

// encodeCommand 把脚本编码为 UTF-16LE Base64，供 -EncodedCommand 使用。
// 这样可彻底避免命令行引号/换行带来的转义问题。
func encodeCommand(script string) string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		buf[i*2] = byte(u)
		buf[i*2+1] = byte(u >> 8)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// truncate 截断日志文本，避免日志被 PowerShell 输出淹没。
func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= logTextLimit {
		return s
	}
	return s[:logTextLimit] + "...(truncated)"
}

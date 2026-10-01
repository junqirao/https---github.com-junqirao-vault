//go:build linux

package linuxlvm

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"vault/internal/apperr"
)

// logTextLimit 单条命令输出写入日志的最大字节数，避免日志被 LVM 的冗长输出淹没。
const logTextLimit = 4096

// Run 执行一条外部命令，成功时返回 TrimSpace 后的 CombinedOutput。
//
// 参数一律以切片传给 exec.CommandContext，**绝不拼接 shell 字符串**：
// LV 名、设备路径都来自配置或前端，拼 shell 会引入命令注入。
// 失败时只把「精简后的输出」写进日志，返回给上层的是平台错误码
// （原始报错文本不进 API 响应，符合 apperr 的设计约定）。
func Run(ctx context.Context, logger *slog.Logger, name string, args ...string) (string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if ctx.Err() != nil {
			logger.Error("命令超时或被取消",
				"cmd", name, "err", ctx.Err().Error(), "output", limitText(text))
			return text, apperr.New(CodeCommandFailed, http.StatusInternalServerError).
				WithCause(ctx.Err()).WithArg("cmd", name)
		}
		logger.Error("命令执行失败",
			"cmd", name, "err", err.Error(), "output", limitText(text))
		return text, apperr.New(CodeCommandFailed, http.StatusInternalServerError).
			WithCause(err).WithArg("cmd", name)
	}
	logger.Debug("命令执行成功", "cmd", name, "args", args, "duration", time.Since(start).String())
	return text, nil
}

// LookPath 是 exec.LookPath 的薄封装，供能力探测使用（只查可执行文件，不执行它）。
func LookPath(name string) (string, error) { return exec.LookPath(name) }

// limitText 截断进入日志的命令输出。
func limitText(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= logTextLimit {
		return s
	}
	return s[:logTextLimit] + "...(truncated)"
}

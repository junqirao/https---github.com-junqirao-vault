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
	return execCmd(ctx, logger, false, name, args...)
}

// RunQuiet 与 Run 同源，区别只在**非 0 退出不写 ERROR 日志**，由调用方解释这个退出码。
//
// 之所以需要它：有一类调用把非 0 退出当作**正常答案**，而不是故障——
//
//	lvs  <vg>/<lv>  → "Failed to find logical volume"：判定"这个卷还不存在"
//	blkid <dev>     → 退出码 2：判定"这个设备上还没有文件系统"
//	lvchange -an    → 本就未激活，幂等语义下无需处理
//
// 这些分支每次建盘、每次删除都会走到（建盘的第一步就是探一次"不存在"），打成 ERROR
// 会让日志里满是红字，真正的故障反而被淹没（真机反馈：建盘成功了，前后却各跟着一条 ERROR）。
// 命令输出仍以 Debug 留痕，需要时照样能查。
//
// 只在"非 0 退出属于正常分支"的地方用；有副作用的操作（mkfs / mount / lvcreate 等）
// 失败仍走 Run 记 ERROR，即使调用方只是告警。
// 超时/被取消也不在此列——那说明 LVM 卡住了，仍然按 ERROR 记录。
func RunQuiet(ctx context.Context, logger *slog.Logger, name string, args ...string) (string, error) {
	return execCmd(ctx, logger, true, name, args...)
}

// execCmd 是 Run / RunQuiet 的共同实现：quiet 只影响"非 0 退出"时的日志级别。
func execCmd(ctx context.Context, logger *slog.Logger, quiet bool, name string, args ...string) (string, error) {
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
		if quiet {
			logger.Debug("命令返回非 0（按正常分支处理）",
				"cmd", name, "err", err.Error(), "output", limitText(text))
		} else {
			logger.Error("命令执行失败",
				"cmd", name, "err", err.Error(), "output", limitText(text))
		}
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

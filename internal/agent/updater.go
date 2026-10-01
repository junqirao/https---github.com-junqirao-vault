package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// 替换器（原 vault-updater）相关常量。
const (
	// DefaultUpdateTimeout 是等待父进程退出的默认超时。
	DefaultUpdateTimeout = 120 * time.Second
	// updatePollInterval 是等待进程退出的轮询间隔。
	updatePollInterval = 300 * time.Millisecond
	// replaceAttempts 是覆盖目标文件的最大尝试次数（文件可能短暂被占用）。
	replaceAttempts = 10
	// replaceRetryDelay 是覆盖失败后的重试间隔。
	replaceRetryDelay = 500 * time.Millisecond
	// updaterCopyPrefix 是替换器临时副本的文件名前缀。
	updaterCopyPrefix = "updater-"
	// updaterLogName 是替换器独立排障日志的文件名。
	updaterLogName = "updater.log"
)

// UpdateApplyOptions 是 apply-update 子命令的入参。
type UpdateApplyOptions struct {
	// ParentPID 是正在运行的旧版 agent 的 PID（替换器等待其退出后才动文件）。
	ParentPID int
	// StagedPath 是已下载并校验通过的新文件（绝对路径）。
	StagedPath string
	// TargetPath 是需要被覆盖的正式文件（绝对路径，通常是 Vault-Agent.exe）。
	TargetPath string
	// BackupPath 是替换前的备份路径（绝对路径，可为空表示不备份）。
	BackupPath string
	// Restart 表示替换成功后是否启动 TargetPath。
	Restart bool
	// Timeout 是等待父进程退出的超时。
	Timeout time.Duration
}

// RunApplyUpdate 以"替换器"身份运行：等待父进程退出 → 备份 → 替换 →（可选）重启。
//
// 只有通过 apply-update 启动的临时副本进程才会调用本函数：运行中的 exe 无法覆盖自身，
// 因此"替换 + 重启"必须由**另一个文件**（SelfCopyForUpdate 复制出来的副本）来执行，
// 副本从另一个路径运行，所以可以覆盖正式路径上的 Vault-Agent.exe。
//
// 返回值非 nil 表示本次更新没有成功（调用方据此返回非 0 退出码）。
// 成功与失败都会追加写入 updater.log：替换器运行在 agent 已退出之后，
// 需要一份**独立可读**的现场日志，因此不接入按天切分的统一日志。
func RunApplyUpdate(ctx context.Context, opt UpdateApplyOptions, logger *slog.Logger) error {
	opt.StagedPath = strings.TrimSpace(opt.StagedPath)
	opt.TargetPath = strings.TrimSpace(opt.TargetPath)
	opt.BackupPath = strings.TrimSpace(opt.BackupPath)

	log := newApplyLogger(updaterLogPath(opt.TargetPath), logger)

	// 参数校验：所有失败路径都要留下明确原因（替换器可能是唯一现场）。
	if err := validateApplyOptions(opt); err != nil {
		log.error("参数校验失败", "error", err)
		return err
	}

	log.info("替换器启动", "pid", opt.ParentPID, "staged", opt.StagedPath, "target", opt.TargetPath,
		"backup", opt.BackupPath, "restart", opt.Restart, "timeout", opt.Timeout.String())

	// ---- ① 等待父进程退出（只等待，绝不强杀）----
	if err := waitForProcessExit(ctx, opt.ParentPID, opt.Timeout); err != nil {
		log.error("等待父进程退出失败，已放弃本次更新（未做任何修改）",
			"pid", opt.ParentPID, "timeout", opt.Timeout.String(), "error", err)
		return err
	}
	log.info("父进程已退出", "pid", opt.ParentPID)

	// ---- ② 备份 target ----
	hadTarget := false
	if _, err := os.Stat(opt.TargetPath); err == nil {
		hadTarget = true
		if opt.BackupPath != "" {
			if err := copyFile(opt.TargetPath, opt.BackupPath); err != nil {
				log.error("备份失败，已放弃本次更新（未做任何修改）", "error", err)
				return err
			}
			log.info("已备份原文件", "backup", opt.BackupPath)
		} else {
			log.warn("未指定备份路径：替换失败将无法还原")
		}
	} else {
		log.info("目标文件不存在，将直接写入", "target", opt.TargetPath)
	}

	// ---- ③ staged → target ----
	if err := replaceFile(opt.StagedPath, opt.TargetPath); err != nil {
		log.error("替换目标文件失败，尝试用备份还原", "error", err)
		rollbackApply(opt, hadTarget, log)
		return err
	}
	log.info("替换完成", "target", opt.TargetPath)

	// ---- ④ 重启 ----
	if opt.Restart {
		if err := startDetached(opt.TargetPath); err != nil {
			log.error("启动新版本失败，尝试用备份还原", "error", err)
			rollbackApply(opt, hadTarget, log)
			return err
		}
		log.info("已启动新版本", "target", opt.TargetPath)
	}

	log.info("更新流程成功结束")
	return nil
}

// SelfCopyForUpdate 把当前进程的 exe 复制到临时路径，返回副本路径。
//
// 副本放在 %ProgramData%\Vault\update\ 下（而不是 %TEMP%），便于集中清理与排障；
// 命名为 updater-<pid>-<8位随机>.exe，避免并发/历史副本互相覆盖。
//
// Windows 允许复制**正在运行**的 exe：镜像文件以共享读方式打开，读取其字节不受限制
// （只禁止改写正在运行的镜像），因此本函数在进程自身运行时也能成功。
func SelfCopyForUpdate() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("agent: 解析当前可执行文件路径失败: %w", err)
	}
	dir := filepath.Join(DataDir(), updateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("agent: 创建替换器副本目录失败: %w", err)
	}
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, fmt.Sprintf("%s%d-%s.exe", updaterCopyPrefix, os.Getpid(), suffix))
	if err := copyFile(exe, dst); err != nil {
		return "", fmt.Errorf("agent: 复制自身为替换器副本失败: %w", err)
	}
	return dst, nil
}

// CleanupStaleUpdaterCopies 清理历史遗留的替换器副本（best-effort，供启动时调用）。
//
// 正在运行的副本无法被删除（Windows 禁止删除运行中的镜像），此时 os.Remove 会失败，
// 直接忽略即可——它会在退出后由下一次启动清理。清理失败不影响任何功能。
func CleanupStaleUpdaterCopies(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	dir := filepath.Join(DataDir(), updateDirName)
	matches, err := filepath.Glob(filepath.Join(dir, updaterCopyPrefix+"*.exe"))
	if err != nil || len(matches) == 0 {
		return
	}
	removed := 0
	for _, p := range matches {
		if err := os.Remove(p); err != nil {
			continue
		}
		removed++
	}
	if removed > 0 {
		logger.Info("已清理历史替换器副本", "count", removed, "dir", dir)
	}
}

// startApplyUpdateCopy 复制自身为临时副本，并以 `apply-update` 子命令启动该副本。
//
// 副本进程是**分离**的（不继承控制台、不随父进程退出而终止），因为它要在父进程退出后
// 才继续执行替换与重启。
func startApplyUpdateCopy(args []string, logger *slog.Logger) (string, error) {
	exe, err := SelfCopyForUpdate()
	if err != nil {
		return "", err
	}
	cmd := detachedCommand(exe, append([]string{applyUpdateSubcommand}, args...)...)
	if err := cmd.Start(); err != nil {
		return exe, err
	}
	if logger != nil {
		logger.Info("已启动替换器临时副本（apply-update），等待其完成替换与重启", "copy", exe)
	}
	return exe, nil
}

// validateApplyOptions 校验参数（路径必须是绝对路径、staged 必须存在、target 目录必须存在）。
func validateApplyOptions(opt UpdateApplyOptions) error {
	if opt.ParentPID <= 0 {
		return errors.New("-pid 必须为正整数")
	}
	if opt.StagedPath == "" || opt.TargetPath == "" {
		return errors.New("-staged 与 -target 均为必填")
	}
	for name, p := range map[string]string{"-staged": opt.StagedPath, "-target": opt.TargetPath} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%s 必须是绝对路径: %q", name, p)
		}
	}
	info, err := os.Stat(opt.StagedPath)
	if err != nil {
		return fmt.Errorf("-staged 不可用: %w", err)
	}
	if info.IsDir() {
		return errors.New("-staged 指向目录")
	}
	dir := filepath.Dir(opt.TargetPath)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("-target 所在目录不存在: %q", dir)
	}
	if opt.BackupPath != "" {
		if !filepath.IsAbs(opt.BackupPath) {
			return fmt.Errorf("-backup 必须是绝对路径: %q", opt.BackupPath)
		}
		if err := os.MkdirAll(filepath.Dir(opt.BackupPath), 0o755); err != nil {
			return fmt.Errorf("-backup 目录不可创建: %w", err)
		}
	}
	if opt.Timeout <= 0 {
		return errors.New("-timeout 必须为正")
	}
	return nil
}

// waitForProcessExit 轮询等待 PID 退出；返回错误表示超时、被取消或无法判定。
//
// 使用 SYNCHRONIZE 句柄等待而不是"查询进程名"：句柄方式既不会误判同名进程，
// 也不需要任何额外工具依赖。
func waitForProcessExit(ctx context.Context, pid int, timeout time.Duration) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// 打不开句柄通常意味着进程已不存在（或权限不足）：
		// 前者视为"已退出"，后者由后续的文件操作自然暴露。
		return nil
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	if timeout <= 0 {
		timeout = DefaultUpdateTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("等待已取消: %w", err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("等待进程退出超时")
		}
		waitMs := uint32(updatePollInterval.Milliseconds())
		if remaining < updatePollInterval {
			waitMs = uint32(remaining.Milliseconds())
		}
		event, err := windows.WaitForSingleObject(handle, waitMs)
		if err != nil {
			return err
		}
		if event == windows.WAIT_OBJECT_0 {
			return nil
		}
		// WAIT_TIMEOUT：继续轮询。
	}
}

// replaceFile 把 staged 覆盖到 target。
//
// 原子性：先复制到 target 同目录的临时文件，再改名覆盖；
// 这样即使复制中途失败，target 仍是完整的旧文件（由调用方决定是否还原）。
func replaceFile(staged, target string) error {
	tmp := target + ".new"
	var lastErr error
	for attempt := 1; attempt <= replaceAttempts; attempt++ {
		if err := copyFile(staged, tmp); err != nil {
			lastErr = err
		} else if err := os.Rename(tmp, target); err != nil {
			lastErr = err
		} else {
			return nil
		}
		// 目标可能仍被短暂占用（句柄释放有延迟）：重试而不是强杀进程。
		_ = os.Remove(tmp)
		time.Sleep(replaceRetryDelay)
	}
	return fmt.Errorf("重试 %d 次后仍无法替换 %s: %w", replaceAttempts, target, lastErr)
}

// rollbackApply 用备份还原 target（还原失败只记日志，交由人工处理）。
func rollbackApply(opt UpdateApplyOptions, hadTarget bool, log *applyLogger) {
	if opt.BackupPath == "" {
		log.warn("无法还原：本次未指定备份路径")
		return
	}
	if _, err := os.Stat(opt.BackupPath); err != nil {
		log.warn("无法还原：备份不存在", "backup", opt.BackupPath, "error", err)
		return
	}
	if !hadTarget {
		log.warn("原文件本来不存在，删除替换失败的目标文件", "target", opt.TargetPath)
		_ = os.Remove(opt.TargetPath)
		return
	}
	if err := copyFile(opt.BackupPath, opt.TargetPath); err != nil {
		log.error("还原失败，请人工恢复", "backup", opt.BackupPath, "target", opt.TargetPath, "error", err)
		return
	}
	log.info("已用备份还原", "target", opt.TargetPath)
}

// startDetached 以分离会话启动目标程序（不继承替换器的控制台）。
func startDetached(path string) error {
	cmd := detachedCommand(path)
	// 不 Wait：新进程应独立于替换器存活。
	return cmd.Start()
}

// detachedCommand 构造"分离会话"的启动命令：不继承控制台，独立于父进程存活。
func detachedCommand(path string, args ...string) *exec.Cmd {
	cmd := exec.Command(path, args...)
	cmd.Dir = filepath.Dir(path)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	return cmd
}

// copyFile 复制文件（保留内容；覆盖已存在的目标）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// randomHex 返回 n 字节（2n 个十六进制字符）的随机串。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("agent: 生成随机串失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// applyLogger 是替换器的日志出口。
//
// 它把"时间戳 + 级别 + 消息 + 键值对"追加写入 updater.log，并（在传入时）转发到
// 结构化日志；替换器运行在 agent 已退出之后，这份文件是"替换失败/超时"事故的唯一现场，
// 因此成功与失败都必须写。
type applyLogger struct {
	path   string
	logger *slog.Logger
	mu     sync.Mutex
}

// newApplyLogger 构造日志器；文件不可写时退化为仅标准错误输出。
func newApplyLogger(path string, logger *slog.Logger) *applyLogger {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "Vault-Agent(apply-update): 创建日志目录失败:", err)
			return &applyLogger{logger: logger}
		}
	}
	return &applyLogger{path: path, logger: logger}
}

// info 记录普通事件。
func (l *applyLogger) info(msg string, kv ...any) { l.write("INFO", msg, kv...) }

// warn 记录需要人工关注但不致命的事件。
func (l *applyLogger) warn(msg string, kv ...any) { l.write("WARN", msg, kv...) }

// error 记录失败事件。
func (l *applyLogger) error(msg string, kv ...any) { l.write("ERROR", msg, kv...) }

// write 追加一行日志并转发到结构化日志。
func (l *applyLogger) write(level, msg string, kv ...any) {
	if l.logger != nil {
		switch level {
		case "ERROR":
			l.logger.Error(msg, kv...)
		case "WARN":
			l.logger.Warn(msg, kv...)
		default:
			l.logger.Info(msg, kv...)
		}
	}

	var b strings.Builder
	b.WriteString(time.Now().Format("2006-01-02T15:04:05.000Z07:00"))
	b.WriteString(" [")
	b.WriteString(level)
	b.WriteString("] ")
	b.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(fmt.Sprintf(" %v=%v", kv[i], kv[i+1]))
	}
	b.WriteString("\n")
	line := b.String()

	if l.path == "" {
		fmt.Fprintln(os.Stderr, strings.TrimRight(line, "\n"))
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Vault-Agent(apply-update): 写入日志失败:", err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line); err != nil {
		fmt.Fprintln(os.Stderr, "Vault-Agent(apply-update): 写入日志失败:", err)
	}
}

// updaterLogPath 返回替换器日志路径：%ProgramData%\Vault\update\updater.log。
//
// 与代理约定的更新工作目录一致；ProgramData 不可用时退化为"target 同目录/updater.log"，
// 保证任何环境都留下现场记录。
func updaterLogPath(target string) string {
	if pd := strings.TrimSpace(os.Getenv("ProgramData")); pd != "" {
		return filepath.Join(pd, "Vault", updateDirName, updaterLogName)
	}
	dir := filepath.Dir(target)
	if dir == "" || dir == "." {
		return updaterLogName
	}
	return filepath.Join(dir, updaterLogName)
}

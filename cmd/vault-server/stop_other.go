//go:build !windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vault/internal/config"
)

// pidFileName 是服务端在 Linux 上写下的 PID 文件名（用于 `Vault-Server stop`）。
//
// 与 Windows 的"全局命名事件"对应：Windows 靠内核对象跨进程触发停机，
// Linux 没有等价物，改为"服务端写 PID 文件 + stop 子命令发 SIGTERM"。
// 语义上二者最终都收敛到 serve 的**同一个**优雅停机流程
// （关闭 HTTP、停止 job worker、释放资源）。
const pidFileName = "vault-server.pid"

// 停机来源标识，用于日志区分是信号还是 stop 子命令。
const (
	// stopSourceSignal 表示由 Ctrl+C / SIGTERM 触发。
	stopSourceSignal = "signal"
	// stopSourceCommand 表示由 `Vault-Server stop` 触发。
	stopSourceCommand = "stop-command"
)

// stopRequest 描述一次停机请求的来源。
type stopRequest struct {
	// source 取 stopSourceSignal 或 stopSourceCommand。
	source string
	// signal 仅 source == stopSourceSignal 时有效，形如 "terminated"。
	signal string
}

// stopEvent 在 Linux 上是"PID 文件"的句柄（对应 Windows 的命名事件句柄）。
type stopEvent struct {
	path string
}

// createStopEvent 写下当前进程的 PID 文件，供 `Vault-Server stop` 定位本进程。
//
// 写失败只让 stop 子命令不可用，不影响启动与 Ctrl+C 停机（与 Windows 版一致）。
func createStopEvent() (stopEvent, error) {
	var lastErr error
	for _, dir := range pidDirCandidates() {
		path := filepath.Join(dir, pidFileName)
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			lastErr = err
			continue
		}
		return stopEvent{path: path}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可写的目录")
	}
	return stopEvent{}, lastErr
}

// closeStopEvent 删除 PID 文件（句柄为空时静默返回）。
func closeStopEvent(ev stopEvent) {
	if ev.path == "" {
		return
	}
	_ = os.Remove(ev.path)
}

// waitStopEvent 在 Linux 上返回一个**永不关闭**的通道。
//
// 原因：跨进程停机由 SIGTERM 承载，而 SIGTERM 已由 serve 的 signal.Notify 处理
// （见 main.go 的 stop_waiter），不需要第二条触发路径。返回永不关闭的通道
// 等价于在 select 中禁用该分支，行为与"stop 事件不可用"时的 Windows 版完全一致。
func waitStopEvent(_ stopEvent) <-chan struct{} {
	return make(chan struct{})
}

// runStop 实现 `Vault-Server stop`：向 PID 文件记录的进程发送 SIGTERM。
//
// 退出码：0 = 已成功发出停止请求；1 = 未检测到运行中的服务端或发送失败。
func runStop(args []string) int {
	fs := flag.NewFlagSet("Vault-Server "+stopSubcommand, flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "配置文件路径（用于确认服务端监听地址）")
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	pid, path, err := readPIDFile()
	if err != nil {
		fmt.Println("没有检测到正在运行的服务端。")
		fmt.Println("  提示：服务端可能未启动，或由另一个用户/容器命名空间运行（PID 文件不可见）。")
		return 1
	}

	// PID 可能已被复用：先核对 /proc/<pid>/comm 再发信号，避免误杀无关进程。
	if name, rerr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm"); rerr == nil {
		if !sameProgram(strings.TrimSpace(string(name))) {
			fmt.Printf("PID 文件（%s）中的进程 %d 不是 Vault 服务端（comm=%s），已忽略。\n",
				path, pid, strings.TrimSpace(string(name)))
			fmt.Println("  提示：服务端可能已异常退出，该 PID 文件已过期。")
			return 1
		}
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		fmt.Fprintln(os.Stderr, "发送停止信号失败:", err)
		fmt.Println("  提示：服务端可能已退出；如以其他用户身份运行，请用相同账号执行本命令。")
		return 1
	}
	fmt.Printf("已向运行中的服务端（PID %d）发送停止请求（优雅停机中）...\n", pid)

	// 尽力确认已停止：轮询监听端口直到连不上（最多 10 秒）。
	// 配置读不到（例如在别的目录执行）只跳过确认，不影响退出码。
	if loaded, err := config.Load(*configPath); err == nil {
		listen := loaded.Raw.HTTP.Listen
		if listen != "" {
			if waitUntilClosed(listen, 10*time.Second) {
				fmt.Println("服务端已停止。")
			} else {
				fmt.Println("停止请求已发送，但服务端在 10 秒内仍可连接（可能仍在停机中）。")
			}
		}
	} else {
		fmt.Println("（未能读取配置文件，跳过停止确认；可稍后用 ps 核对进程）")
	}
	return 0
}

// readPIDFile 读取 PID 文件，返回 PID 与实际使用的路径。
func readPIDFile() (int, string, error) {
	var lastErr error
	for _, dir := range pidDirCandidates() {
		path := filepath.Join(dir, pidFileName)
		b, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 {
			lastErr = fmt.Errorf("PID 文件内容非法: %s", path)
			continue
		}
		return pid, path, nil
	}
	if lastErr == nil {
		lastErr = errors.New("未找到 PID 文件")
	}
	return 0, "", lastErr
}

// sameProgram 判断 comm 是否是本程序（内核把 comm 截断到 15 字符）。
func sameProgram(comm string) bool {
	exe, err := os.Executable()
	if err != nil {
		return true // 无法判定时不阻断停止请求
	}
	want := filepath.Base(exe)
	if len(want) > 15 {
		want = want[:15]
	}
	return strings.EqualFold(comm, want)
}

// pidDirCandidates 返回 PID 文件的候选目录（顺序与 createStopEvent 一致）。
func pidDirCandidates() []string {
	return []string{"/run", os.TempDir()}
}

// waitUntilClosed 轮询监听地址，直到无法连接（即服务端已停止）或超时。
func waitUntilClosed(listen string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !probeListen(listen, launcherProbeTimeout) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(launcherPollInterval)
	}
}

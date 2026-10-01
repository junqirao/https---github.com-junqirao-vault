//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"

	"vault/internal/config"
)

// stopEventName 是用于跨进程优雅停机的全局命名事件名。
//
// 语义：服务端 serve 启动时创建它（手动重置、初始未触发）；
// `Vault-Server stop` 用另一个进程打开并 SetEvent 触发它，服务端据此走
// **与 Ctrl+C 完全相同**的优雅停机流程（关闭 HTTP、停止 job worker、释放资源）。
//
// 之所以用"命名事件"而不是"结束进程"（旧的 VBScript 方案就是直接 Terminate）：
// 强制结束进程会跳过数据库收尾、iSCSI 目标释放与 VHDX 句柄关闭，
// 可能留下孤儿 target 或未刷盘的差异盘。
const stopEventName = `Global\VaultServerStop`

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
	// signal 仅 source == stopSourceSignal 时有效，形如 "interrupt"。
	signal string
}

// createStopEvent 创建跨进程停止事件（手动重置、初始未触发）。
//
// 手动重置：一旦被触发就保持触发态，避免多个等待者之间产生竞态。
// name 已存在时 CreateEvent 会直接打开它（不会失败）。
func createStopEvent() (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(stopEventName)
	if err != nil {
		return 0, err
	}
	return windows.CreateEvent(nil, 1 /*manualReset*/, 0 /*initialState*/, name)
}

// closeStopEvent 关闭停止事件句柄（句柄无效时静默返回）。
func closeStopEvent(handle windows.Handle) {
	if handle == 0 {
		return
	}
	_ = windows.CloseHandle(handle)
}

// waitStopEvent 返回一个通道，停止事件被触发时该通道关闭。
//
// handle 无效（创建失败）时返回一个**永不关闭**的通道：在 select 中等价于
// 禁用该分支，从而保证"stop 子命令不可用"不会影响正常启动与 Ctrl+C 停机。
func waitStopEvent(handle windows.Handle) <-chan struct{} {
	done := make(chan struct{})
	if handle == 0 {
		return done
	}
	go func() {
		_, _ = windows.WaitForSingleObject(handle, windows.INFINITE)
		close(done)
	}()
	return done
}

// runStop 实现 `Vault-Server stop`：请求运行中的服务端优雅停机。
//
// 退出码：0 = 已成功发出停止请求（含确已停止）；1 = 未检测到运行中的服务端或发送失败。
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

	name, err := windows.UTF16PtrFromString(stopEventName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "停止请求失败:", err)
		return 1
	}

	// EVENT_MODIFY_STATE：SetEvent 所需；SYNCHRONIZE：等待/观察所需。
	handle, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE|windows.SYNCHRONIZE, false, name)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			fmt.Println("没有检测到正在运行的服务端。")
			fmt.Println("  提示：服务端可能未启动，或由非管理员身份启动（全局命名事件不可见）。")
			return 1
		}
		fmt.Fprintln(os.Stderr, "打开停止事件失败:", err)
		fmt.Fprintln(os.Stderr, "  提示：请以与启动服务端相同的账号（通常是管理员）执行本命令。")
		return 1
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	if err := windows.SetEvent(handle); err != nil {
		fmt.Fprintln(os.Stderr, "发送停止请求失败:", err)
		return 1
	}
	fmt.Println("已向运行中的服务端发送停止请求（优雅停机中）...")

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
		fmt.Println("（未能读取配置文件，跳过停止确认；可稍后用 Get-Process Vault-Server 核对）")
	}
	return 0
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

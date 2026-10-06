//go:build !windows

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"vault/internal/config"
)

// detachedFlagName 是"内部标记"命令行开关名。
//
// Linux 侧不派生后台子进程（见 ownsOwnConsole 的说明），该标记只是为了让
// main.go 的命令行定义与 Windows 版保持一致——解析得到后会被忽略。
const detachedFlagName = "detached"

// 启动器参数常量：Linux 侧不使用启动器，但 stop 子命令的"确认已停止"轮询
// 复用 launcherProbeTimeout / launcherPollInterval，故在此一并定义。
const (
	// launcherReadyTimeout 是等待服务端就绪的上限（仅 Windows 启动器使用）。
	launcherReadyTimeout = 30 * time.Second
	// launcherPollInterval 是就绪/停止确认的轮询间隔。
	launcherPollInterval = 300 * time.Millisecond
	// launcherProbeTimeout 是单次 TCP 探测的超时。
	launcherProbeTimeout = 800 * time.Millisecond
)

// ownsOwnConsole 恒返回 false：Linux 上没有"双击 exe 独占控制台"这种场景，
// 因此永远不会因"独占控制台"而自动切后台——是否后台只由 --background 决定
// （见 run 中的说明）。默认一律前台运行，由 systemd 等托管，或用 --background 后台化。
func ownsOwnConsole() bool { return false }

// runLauncher 是 --background 启动器路径（Linux）。
//
// 与 serve 路径的关键差异：**不获取单实例锁**，只做三件事：
//  1. 准备并加载配置；
//  2. 探测服务端是否已在运行（TCP 连得上即认为在运行）；
//  3. 派生一个脱离本进程会话（Setsid）的自身副本作为后台服务，等待其就绪。
func runLauncher(configPath string) int {
	if created, err := config.EnsureFromExample(configPath); err != nil {
		fmt.Fprintln(os.Stderr, "准备配置文件失败:", err)
		return 1
	} else if created {
		fmt.Print(configCreatedHint(configPath))
	}

	loaded, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载配置失败:", err)
		return 1
	}
	raw := loaded.Raw

	if probeListen(raw.HTTP.Listen, launcherProbeTimeout) {
		fmt.Printf("\nVault 服务端已经在运行中（地址 %s）。\n", raw.HTTP.Listen)
		fmt.Println("  无需重复启动。若要停止，请执行：")
		fmt.Printf("    Vault-Server %s -config %s\n", stopSubcommand, configPath)
		return 0
	}

	pid, err := spawnDetached(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动后台服务失败:", err)
		fmt.Fprintf(os.Stderr, "  提示：可前台启动以查看详细日志：Vault-Server -config %s\n", configPath)
		return 1
	}

	if !waitListen(raw.HTTP.Listen, launcherReadyTimeout) {
		fmt.Fprintf(os.Stderr, "\n启动超时：%d 秒内未能连接 %s。\n",
			int(launcherReadyTimeout.Seconds()), raw.HTTP.Listen)
		fmt.Fprintf(os.Stderr, "  后台进程已派生，PID %d；详细输出见 %s\n", pid, backgroundLogPathFor(configPath))
		return 1
	}

	printLauncherBanner(raw, configPath, pid)
	return 0
}

// spawnDetached 派生当前可执行文件作为后台服务（新会话 + 脱离本进程的 stdio）。
//
// 与 Windows 版同一套语义：
//   - SysProcAttr.Setsid：子进程进入新会话、脱离控制终端，父进程退出不影响它；
//   - Stdin 接到 /dev/null，Stdout/Stderr 落到后台日志文件（保留崩溃现场）；
//   - 参数去掉 --background 并追加 -detached，避免子进程二次派生；
//   - 不调用 Wait，立即 Release，让后台服务独立于启动器生命周期。
func spawnDetached(configPath string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("获取自身路径失败: %w", err)
	}

	logPath := backgroundLogPathFor(configPath)
	if dir := filepath.Dir(logPath); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			fmt.Fprintf(os.Stderr, "警告：无法创建后台日志目录 %s（%v）\n", dir, mkErr)
		}
	}
	console, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告：无法打开后台日志文件 %s（%v），后台崩溃现场将无法追溯\n", logPath, err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		devNull = nil
	}

	cmd := exec.Command(exe, backgroundChildArgs(os.Args[1:])...)
	cmd.Stdin = devNull
	cmd.Stdout = console
	cmd.Stderr = console
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// probeListen 探测监听地址是否已可连接。
func probeListen(listen string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", dialAddress(listen), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// waitListen 轮询监听地址直到可连接（返回 true）或超时（返回 false）。
func waitListen(listen string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if probeListen(listen, launcherProbeTimeout) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(launcherPollInterval)
	}
}

// dialAddress 把配置里的监听地址规范化为可用于拨号的地址。
//
// 监听地址常写成通配地址（0.0.0.0 / [::] / 空主机），直接拿去拨号是没有意义的，
// 这里统一替换为回环地址 127.0.0.1，从而兼容
// "0.0.0.0:8443"、"[::]:8443"、"127.0.0.1:8443" 三种写法。
// 解析失败（例如只写了端口）时原样返回，由拨号本身报错。
func dialAddress(listen string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

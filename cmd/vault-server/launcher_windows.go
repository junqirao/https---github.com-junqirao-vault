//go:build windows

package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"vault/internal/config"
)

// detachedFlagName 是"内部标记"命令行开关名。
//
// 作用：让启动器派生的子进程显式声明"我是后台进程，不要再走启动器"。
// 子进程以 DETACHED_PROCESS 创建、完全没有控制台，本来就不会触发启动器判断
// （GetConsoleWindow 为 0），该标记只作为显式兜底。
const detachedFlagName = "detached"

// 启动器的就绪等待与探测参数。
const (
	// launcherReadyTimeout 是等待后台服务端就绪的上限。
	launcherReadyTimeout = 30 * time.Second
	// launcherPollInterval 是就绪轮询间隔。
	launcherPollInterval = 300 * time.Millisecond
	// launcherProbeTimeout 是单次 TCP 探测的超时。
	launcherProbeTimeout = 800 * time.Millisecond
)

// ownsOwnConsole 判断当前进程是否**独占一个控制台**（典型的"双击启动"场景）。
//
// ⚠️ 该函数只用于给前台运行补一句提示（双击启动时崩溃现场会随窗口消失），
// **不再**用于"自动切后台"——是否后台只由 --background 决定（见 run 中的说明）。
//
// 判据与 internal/agent 的 HideOwnConsoleWindow 一致：
//   - GetConsoleWindow 返回 0 表示根本没有控制台（计划任务、SYSTEM 无人值守、
//     或被以管道方式启动），此时不视为双击；
//   - GetConsoleProcessList 返回 1 表示该控制台只服务于当前进程，即系统为"双击"
//     专门创建的控制台；从 cmd/PowerShell 手工启动时，控制台里必然还有 shell
//     进程（>= 2），因此不会误触发。
//
// 任何一步失败都返回 false：启动器路径是"体验优化"，宁可退化为前台启动。
func ownsOwnConsole() bool {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	getConsoleProcessList := kernel32.NewProc("GetConsoleProcessList")

	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		return false
	}

	// 缓冲区不足时该 API 返回所需大小，此时必然 != 1，同样不会误判。
	var pids [8]uint32
	count, _, _ := getConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	return count == 1
}

// runLauncher 是 --background 启动器路径。
//
// 与 serve 路径的关键差异：**不获取单实例锁**（锁属于真正提供服务的后台进程，
// 启动器若持有会与之冲突），只做三件事：
//  1. 准备并加载配置（与 serve 完全一致，保证后续探测与子进程读的是同一份配置）；
//  2. 探测服务端是否已在运行（TCP 连得上即认为在运行）；
//  3. 以 DETACHED_PROCESS 派生一个无控制台的自身副本作为后台服务，等待其就绪。
//
// 返回 0 表示"服务已就绪（或本来就在运行）"，返回 1 表示启动失败。
// 两条路径都会在结束前等待用户按一次回车——窗口是用户双击得到的，不该一闪而过。
func runLauncher(configPath string) int {
	// ---- 1) 配置：与 serve 路径同样的准备方式 ----
	if created, err := config.EnsureFromExample(configPath); err != nil {
		fmt.Fprintln(os.Stderr, "准备配置文件失败:", err)
		fmt.Fprintln(os.Stderr, "  提示：请把发布包中的 config.example.yaml 与本程序放在同一目录。")
		pauseForUser()
		return 1
	} else if created {
		fmt.Printf("未找到配置文件，已从模板生成: %s\n", configPath)
		fmt.Println("  提示：请确认其中的 storage.whitelist_root 指向你的数据目录（模板默认 D:\\VaultData）。")
	}

	loaded, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载配置失败:", err)
		fmt.Fprintln(os.Stderr, "  提示：请检查配置文件语法；也可在命令行执行 Vault-Server -config 配置路径 查看详细错误。")
		pauseForUser()
		return 1
	}
	raw := loaded.Raw

	// ---- 2) 已在运行？ ----
	if probeListen(raw.HTTP.Listen, launcherProbeTimeout) {
		fmt.Printf("\nVault 服务端已经在运行中（地址 %s）。\n", raw.HTTP.Listen)
		fmt.Println("  无需重复启动。若要停止，请在本程序所在目录执行：")
		fmt.Printf("    Vault-Server %s\n", stopSubcommand)
		pauseForUser()
		return 0
	}

	// ---- 3) 派生无控制台的后台服务 ----
	pid, err := spawnDetached(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动后台服务失败:", err)
		fmt.Fprintln(os.Stderr, "  提示：可改用命令行前台启动以查看详细日志：")
		fmt.Fprintln(os.Stderr, "    Vault-Server -config "+configPath)
		pauseForUser()
		return 1
	}

	// ---- 4) 等待就绪（TCP 探测，最多 30 秒）----
	if !waitListen(raw.HTTP.Listen, launcherReadyTimeout) {
		fmt.Fprintf(os.Stderr, "\n启动超时：%d 秒内未能连接 %s。\n",
			int(launcherReadyTimeout.Seconds()), raw.HTTP.Listen)
		fmt.Fprintf(os.Stderr, "  后台进程已派生，PID %d。\n", pid)
		fmt.Fprintln(os.Stderr, "请检查：")
		fmt.Fprintln(os.Stderr, "  1) 是否被安全软件拦截")
		if raw.Log.Dir != "" {
			fmt.Fprintf(os.Stderr, "  2) 日志目录 %s 下的日志文件（默认只写文件，不显示在界面上）\n", raw.Log.Dir)
		} else {
			fmt.Fprintln(os.Stderr, "  2) 当前未配置 log.dir，日志只在服务端控制台输出")
		}
		fmt.Fprintf(os.Stderr, "  5) 后台进程的控制台输出（含崩溃栈）见 %s\n", backgroundLogPathFor(configPath))
		fmt.Fprintln(os.Stderr, "  3) 是否已安装 iSCSI 目标服务器角色（缺失不影响启动，但相关功能不可用）")
		fmt.Fprintln(os.Stderr, "  4) 监听端口是否被占用")
		fmt.Fprintln(os.Stderr, "如需直接看启动错误，可在本目录打开命令行执行：")
		fmt.Fprintf(os.Stderr, "  Vault-Server -config %s\n", configPath)
		pauseForUser()
		return 1
	}

	// ---- 5) 就绪 ----
	printLauncherBanner(raw, configPath, pid)
	pauseForUser()
	return 0
}

// spawnDetached 以"完全没有控制台"的方式派生当前可执行文件作为后台服务。
//
// 关键点（任一遗漏都会让黑框重新出现或让服务随父进程一起结束）：
//   - CreationFlags 含 DETACHED_PROCESS：子进程**没有**控制台；
//     再带上 CREATE_NEW_PROCESS_GROUP，避免父子共享控制台事件（Ctrl+C 之类）。
//   - Stdin 留空（接到空设备），绝不继承启动器所在控制台的句柄。
//   - Stdout/Stderr **重定向到日志文件**：后台进程没有控制台，Go 运行时的
//     panic / fatal error 只写 fd 1/2；接到空设备就等于把崩溃现场直接丢掉
//     （表现正是"点一下就宕机、没有任何提示"）。落盘后运维可事后查证。
//   - 命令行参数 = 原始参数（去掉 --background）+ -detached。
//     去掉 --background 是双保险：即便 -detached 因故未被解析，子进程也不会二次派生。
//   - 工作目录**继承启动器**（不设置 cmd.Dir）：这样子进程读到的就是启动器
//     刚刚校验过的那份配置，避免相对路径被解析到别处。
//   - 不调用 Wait：父进程提示完就退出，后台服务必须继续运行；Release 进程句柄，
//     避免父进程退出前一直持有子进程句柄。
//
// 返回后台进程 PID。
func spawnDetached(configPath string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("获取自身路径失败: %w", err)
	}
	args := backgroundChildArgs(os.Args[1:])

	// 控制台输出落盘：失败只降级为"接到空设备"，绝不阻断启动。
	logPath := backgroundLogPathFor(configPath)
	var console *os.File
	if mkErr := os.MkdirAll(filepath.Dir(logPath), 0o755); mkErr != nil {
		fmt.Fprintf(os.Stderr, "警告：无法创建后台日志目录 %s（%v）\n", filepath.Dir(logPath), mkErr)
	}
	if f, oErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); oErr == nil {
		console = f
		defer func() { _ = console.Close() }()
	} else {
		fmt.Fprintf(os.Stderr, "警告：无法打开后台日志文件 %s（%v），后台崩溃现场将无法追溯\n", logPath, oErr)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = console
	cmd.Stderr = console
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// 不再等待该进程：立即释放句柄，让后台服务独立于启动器生命周期。
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

// pauseForUser 等待用户按一次回车，避免双击得到窗口一闪而过。
//
// 刻意使用最简单的行读取（而非 ReadConsoleInput）：读取失败（例如 stdin 不是
// 控制台、或已被关闭）就直接返回，不阻塞后续退出。
func pauseForUser() {
	fmt.Print("\n按回车键关闭本窗口（服务端在后台继续运行）...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Println()
}

//go:build windows

package winps

import (
	"os/exec"
	"syscall"
)

// createNoWindow 是 Win32 CreateProcess 的 CREATE_NO_WINDOW 标志位。
//
// 0x08000000 与 golang.org/x/sys/windows.CREATE_NO_WINDOW 同值；这里刻意用字面量
// 以避免 winps 包为此引入对 x/sys/windows 的依赖。
const createNoWindow = 0x08000000

// PrepareHiddenCommand 让子进程以“没有可见控制台窗口”的方式运行。
//
// 为什么必须这么做：服务端/代理常以 DETACHED_PROCESS（无控制台）启动，
// 此时若直接 exec 派生子进程，Windows 会为每个**控制台子系统**子程序新建一个
// 控制台窗口。PowerShell 被调用得极其频繁（服务端每次 GET /v1/system/info 都会
// 探测 iSCSI 能力、代理心跳/挂载/卸载/发起端探测等），用户因此会看到黑框反复弹出。
//
//   - CreationFlags = CREATE_NO_WINDOW：子进程使用一个**不可见**的控制台，彻底消除黑框；
//   - HideWindow = true：GUI 子系统子程序的兜底（不显示窗口）。
//
// 该函数是包内**唯一**的子进程准备入口，任何创建子进程的地方都必须调用它。
func PrepareHiddenCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

//go:build windows

package agent

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// HideOwnConsoleWindow 在"当前控制台只属于本进程"时隐藏控制台窗口。
//
// 目的：Vault-Agent 是控制台子系统程序，被**双击**启动时会弹出一个黑色控制台窗口，
// 但它本质是后台常驻服务，不该有窗口（客户端由 Electron 负责展示界面）。
//
// 为什么不用构建期 -H windowsgui 一劳永逸：那样会彻底失去控制台语义，
// `Vault-Agent -h` / `-version` / `apply-update` 从终端调用时输出会无处可去，
// 排障会很痛苦。这里改为运行时判断，两种用法都保住。
//
// 安全边界：先确认控制台里**只有自己**。若控制台里还挂着别的进程
// （典型情况：从 cmd / PowerShell 里手工启动），说明那是用户自己的终端窗口，
// 隐藏它会把用户的终端一起藏掉，因此绝不动手。
//
// 任何一步失败都静默返回——隐藏窗口只是体验优化，不影响代理功能。
func HideOwnConsoleWindow() {
	// 注意：这里使用 kernel32/user32 的裸调用（不引入 cgo）。
	getConsoleWindow := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	getConsoleProcessList := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	showWindow := windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow")

	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		// 没有控制台：例如被 Electron 以管道方式启动（windowsHide: true）。
		return
	}

	// 控制台进程数：为 1 说明这个控制台是系统为"双击"专门创建的。
	// 缓冲区不足时该 API 返回所需大小，此时必然 != 1，同样不会误隐藏。
	var pids [8]uint32
	count, _, _ := getConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	if count != 1 {
		return
	}

	const swHide = 0
	_, _, _ = showWindow.Call(hwnd, swHide)
}

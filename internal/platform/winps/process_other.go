//go:build !windows

package winps

import "os/exec"

// PrepareHiddenCommand 在非 Windows 平台为空实现。
//
// 控制台窗口是 Windows 独有的现象（CREATE_NO_WINDOW / HideWindow 也是 Windows 专属的
// syscall.SysProcAttr 字段），其他平台无需也无法设置，保持空实现以便跨平台编译。
func PrepareHiddenCommand(_ *exec.Cmd) {}

//go:build !linux

package main

import (
	"fmt"
	"os"
)

// doctorUsage 在非 Linux 平台为空：依赖自检针对 LIO/LVM，只在 Linux 上有意义。
const doctorUsage = ""

// runDoctor 在非 Linux 平台不可用。
//
// Windows 侧的等价信息由 GET /v1/system/info 的能力探测提供
// （Hyper-V / IscsiTarget 角色是否就绪），无需命令行自检。
func runDoctor(_ []string) int {
	fmt.Fprintln(os.Stderr, "doctor 子命令仅 Linux 支持：Windows 侧的依赖状态请看 GET /v1/system/info 的能力探测。")
	return 2
}

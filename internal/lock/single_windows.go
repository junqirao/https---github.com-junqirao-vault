//go:build windows

package lock

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// SingleInstance 通过命名互斥体保证同一台机器上只运行一个服务端进程。
//
// 为什么必须单实例（见 docs/implementation.md 3.2）：
//   - 同一 VHDX 被两个进程操作会损坏磁盘；
//   - iSCSI 目标名在单机上全局唯一，双实例会产生竞态。
type SingleInstance struct {
	handle windows.Handle
}

// AcquireSingleInstance 尝试获取全局单实例锁。
//
// name 建议形如 `Global\VaultServer`。已被占用时返回错误。
func AcquireSingleInstance(name string) (*SingleInstance, error) {
	if name == "" {
		name = `Global\VaultServer`
	}
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("lock: 互斥体名称非法: %w", err)
	}

	handle, err := windows.CreateMutex(nil, false, ptr)
	if err != nil {
		return nil, fmt.Errorf("lock: 创建互斥体失败: %w", err)
	}

	// WAIT_ABANDONED 表示上一个持有者异常退出，此时所有权归本进程，视为获取成功。
	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("lock: 等待互斥体失败: %w", err)
	}
	if status != windows.WAIT_OBJECT_0 && status != windows.WAIT_ABANDONED {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("lock: 服务端已在运行（互斥体 %s 被占用）", name)
	}

	return &SingleInstance{handle: handle}, nil
}

// Release 释放单实例锁。
func (s *SingleInstance) Release() error {
	if s == nil || s.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(s.handle)
	s.handle = 0
	return err
}

//go:build !windows

package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// SingleInstance 通过**文件锁（flock）**保证同一台机器上只运行一个服务端进程。
//
// 为什么必须单实例（见 docs/implementation.md 3.2）：
//   - 同一个 thin pool 上的 LV 被两个进程操作会损坏数据；
//   - iSCSI 目标名在单机上全局唯一，双实例会产生竞态。
//
// 与 Windows 版的差异：Windows 用命名互斥体（跨会话全局可见），
// Linux 没有等价物，这里用 flock 持有的锁文件——内核在进程退出（含被 kill -9）时
// 自动释放，不会留下死锁。
type SingleInstance struct {
	file *os.File
	path string
}

// AcquireSingleInstance 尝试获取全局单实例锁。
//
// name 是调用方给的逻辑名（Windows 形如 `Global\VaultServer`），Linux 侧只把它
// 规范化为锁文件名；已被占用时返回错误。
func AcquireSingleInstance(name string) (*SingleInstance, error) {
	base := lockFileName(name)
	var lastErr error
	for _, dir := range lockDirCandidates() {
		path := filepath.Join(dir, base)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			// 目录不存在或无权限：换下一个候选目录（不视为"已在运行"）。
			lastErr = err
			continue
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock: 服务端已在运行（锁文件 %s 被占用）", path)
		}
		// 写入 PID 仅便于运维排查，不参与锁语义。
		_ = f.Truncate(0)
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		return &SingleInstance{file: f, path: path}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("没有可用的锁目录")
	}
	return nil, fmt.Errorf("lock: 创建锁文件失败: %w", lastErr)
}

// Release 释放单实例锁（关闭文件即释放 flock，幂等）。
func (s *SingleInstance) Release() error {
	if s == nil || s.file == nil {
		return nil
	}
	f := s.file
	s.file = nil
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// lockDirCandidates 返回锁文件的候选目录，按优先顺序排列。
//
// /run/lock（多数发行版存在，且 /var/lock 是它的符号链接）优先；
// 容器等 /run 不可写的环境下退化为临时目录（此时单实例仅在同一用户内生效）。
func lockDirCandidates() []string {
	return []string{"/run/lock", "/var/lock", os.TempDir()}
}

// lockFileName 把逻辑锁名规范化为安全的文件名。
func lockFileName(name string) string {
	n := strings.TrimSpace(name)
	n = strings.ReplaceAll(n, `\`, "-")
	n = strings.ReplaceAll(n, "/", "-")
	if n == "" {
		n = "VaultServer"
	}
	return strings.ToLower(n) + ".lock"
}

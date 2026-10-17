//go:build windows

package winvhd

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCountTreeSkipsVolumeReservedDirs 是"本地目录上传建库必定失败"的回归测试。
//
// 事故现场：robocopy 已经把内容复制进新挂载的 VHDX（`G:\`），随后的校验调用
// countTree(`G:\`) 统计目标侧文件数，遍历到系统自建的 `G:\System Volume Information`
// 时被 ACL 拒绝（`open G:\System Volume Information: Access is denied.`），于是
// CopyTree 报 platform.access_denied(op=copytree_dst)；3 次重试都是同一原因，
// 任务最终 failed —— 而磁盘上的内容其实是完整的。
//
// 这里用普通目录模拟这两个系统目录（非卷根路径下可以自由创建），断言它们**整棵**
// 不参与统计。
func TestCountTreeSkipsVolumeReservedDirs(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), 3)
	writeTestFile(t, filepath.Join(root, "sub", "b.bin"), 5)

	// 系统保留目录：系统自动创建、内容不可读，也不属于复制内容。
	// 刻意用大小写混写，锁住 NTFS 的大小写不敏感比较。
	writeTestFile(t, filepath.Join(root, "System Volume Information", "tracking.log"), 7)
	writeTestFile(t, filepath.Join(root, "$RECYCLE.BIN", "S-1-5-21", "$R123.txt"), 11)

	files, bytes, err := countTree(root)
	if err != nil {
		t.Fatalf("countTree 失败: %v", err)
	}
	if files != 2 || bytes != 8 {
		t.Fatalf("系统保留目录必须整棵跳过，期望 2 个文件 / 8 字节，实际 %d / %d", files, bytes)
	}
}

// TestCountTreeKeepsReservedNameFiles 锁定"只跳目录"：同名**文件**属于用户数据，必须统计。
func TestCountTreeKeepsReservedNameFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "sub", "System Volume Information"), 4)
	writeTestFile(t, filepath.Join(root, "$recycle.bin"), 6)

	files, bytes, err := countTree(root)
	if err != nil {
		t.Fatalf("countTree 失败: %v", err)
	}
	if files != 2 || bytes != 10 {
		t.Fatalf("同名文件不能被跳过，期望 2 个文件 / 10 字节，实际 %d / %d", files, bytes)
	}
}

// writeTestFile 写一个指定字节数的文件，父目录不存在时自动创建。
func writeTestFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
}

//go:build windows

package winvhd

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/Microsoft/go-winio/vhd"
)

// TestRunVHDReturnsTrueNilOnSuccess 是真实事故的回归测试。
//
// runVHD 曾经写成 `return mapVHDError("vhd", err)`，而 mapVHDError 的返回类型是
// *apperr.Error：底层调用**成功**（err == nil）时，得到的是"接口非 nil、内部指针为 nil"
// 的假错误。后果有二：
//  1. 上层 `if err != nil` 把成功当失败 —— Windows 下每次 VHDX 创建/挂载/卸载都"失败"；
//  2. 该错误传到 job worker 后 errors.Is 会解引用 nil → panic，整个服务端进程退出。
//
// 因此这里断言：成功的调用必须返回**真正的 nil 接口**（err == nil，而不是 typed nil）。
func TestRunVHDReturnsTrueNilOnSuccess(t *testing.T) {
	m := NewManager(Options{})

	err := m.runVHD(context.Background(), func() error { return nil })
	if err != nil {
		t.Fatalf("成功的 VHDX 操作必须返回 nil，实际得到 %T(%v)", err, err)
	}
}

// TestOpenForAttachUsesValidAccessMask 是"从目录创建存储必定失败"的回归测试。
//
// 背景：go-winio 固定以 **version 2** 的 OPEN_VIRTUAL_DISK_PARAMETERS 调用 OpenVirtualDisk，
// 该版本要求 access mask 为 NONE；此前 Attach 传的是 VIRTUAL_DISK_ACCESS_ALL，
// 于是 OpenVirtualDisk 稳定返回 ERROR_INVALID_PARAMETER(87)
// （日志：platform.vhd_failed: failed to open virtual disk: The parameter is incorrect.），
// 建盘流水线在"挂载"这一步就断了。
//
// 本测试建一个空 VHDX（创建不需要提权），再按 attach 的姿势打开它：
// 只要有人把掩码改回 ALL / ATTACH_RW / GET_INFO，这里立刻失败。
func TestOpenForAttachUsesValidAccessMask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.vhdx")
	params := &vhd.CreateVirtualDiskParameters{
		Version: 2,
		Version2: vhd.CreateVersion2{
			MaximumSize:      minSizeBytes, // 4MiB 之上的最小合法值即可
			BlockSizeInBytes: blockSizeInBytes,
		},
	}
	h, err := vhd.CreateVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagNone, params)
	if err != nil {
		t.Fatalf("创建测试用 VHDX 失败: %v", err)
	}
	_ = syscall.CloseHandle(h)

	handle, err := openForAttach(path)
	if err != nil {
		t.Fatalf("按 attach 姿势打开 VHDX 失败（access mask 很可能又用错了）: %v", err)
	}
	_ = syscall.CloseHandle(handle)
}

// TestRunVHDPropagatesFailure 确保失败路径没有被上面的修复削弱。
func TestRunVHDPropagatesFailure(t *testing.T) {
	m := NewManager(Options{})

	sentinel := errors.New("boom")
	err := m.runVHD(context.Background(), func() error { return sentinel })
	if err == nil {
		t.Fatal("底层失败必须向上返回错误")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误链应保留底层原因，实际：%v", err)
	}
}

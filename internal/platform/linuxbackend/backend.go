//go:build linux

// Package linuxbackend 把 Linux 侧的两块实现聚合为一个 platform 后端：
//
//	虚拟磁盘 / 卷 / 存储池 = linuxlvm（LVM thin LV + thin snapshot + dm-cache）
//	iSCSI 目标            = liotarget（configfs 直控 LIO）
//
// 为什么需要这一层（而不是让 app 直接持有两个 Manager）：
//
//  1. LIO 的 iblock backstore 是**按路径打开块设备**的（udev_path），
//     而 LVM thin LV 默认带 skip-activation 标记（lv_attr 第 10 位 'k'），
//     服务器重启或 LV 新建后 /dev/mapper 下**没有设备节点**，直接登记 backstore 必然失败。
//     因此每次"把这个盘交给 iSCSI"之前都必须先 `lvchange -ay -K`——这个"先激活再发布"
//     的编排属于平台内部知识，放在这里就不会泄漏到 app 层。
//  2. app 层只认 platform 的四个接口，装配时给一个对象即可，与 Windows 侧对称。
package linuxbackend

import (
	"context"
	"log/slog"

	"vault/internal/platform"
	"vault/internal/platform/linuxlvm"
	"vault/internal/platform/liotarget"
)

// Backend 是 Linux 平台后端的聚合入口。
//
// 嵌入 *linuxlvm.Manager 以获得 DiskBackend / VolumeBackend / StorageAdmin 与 Kind；
// IscsiBackend 由本类型实现（在转发给 LIO 之前先激活 LV，见包注释）。
type Backend struct {
	*linuxlvm.Manager

	lio *liotarget.Manager
	log *slog.Logger
}

// 编译期断言：Backend 必须同时满足各后端接口
// （StorageVolumeBackend 由嵌入的 linuxlvm.Manager 提供，见 storagevolume.go）。
var (
	_ platform.DiskBackend          = (*Backend)(nil)
	_ platform.VolumeBackend        = (*Backend)(nil)
	_ platform.IscsiBackend         = (*Backend)(nil)
	_ platform.StorageAdmin         = (*Backend)(nil)
	_ platform.StorageVolumeBackend = (*Backend)(nil)
)

// New 构造 Backend。任一参数为 nil 时返回 nil（调用方据此判定"该平台后端未装配"）。
func New(lvm *linuxlvm.Manager, lio *liotarget.Manager, logger *slog.Logger) *Backend {
	if lvm == nil || lio == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Backend{Manager: lvm, lio: lio, log: logger}
}

// ---- IscsiBackend ----

// Available 探测 LIO configfs 是否可用（Kind 由嵌入的 linuxlvm.Manager 提供）。
func (b *Backend) Available(ctx context.Context) error { return b.lio.Available(ctx) }

// EnsureTarget 先把待发布的 LV 激活，再交给 LIO 收敛目标完整期望状态。
func (b *Backend) EnsureTarget(ctx context.Context, spec platform.TargetSpec) error {
	if spec.BackingRef != "" {
		if err := b.activate(ctx, spec.BackingRef, spec.ReadOnly); err != nil {
			return err
		}
	}
	return b.lio.EnsureTarget(ctx, spec)
}

// RemoveTarget 删除目标（含其名下全部映射与授权）。幂等。
func (b *Backend) RemoveTarget(ctx context.Context, name string) error {
	return b.lio.RemoveTarget(ctx, name)
}

// GetTarget 查询单个目标。
func (b *Backend) GetTarget(ctx context.Context, name string) (*platform.TargetInfo, error) {
	return b.lio.GetTarget(ctx, name)
}

// ListTargets 列出全部目标。
func (b *Backend) ListTargets(ctx context.Context) ([]platform.TargetInfo, error) {
	return b.lio.ListTargets(ctx)
}

// ListSessions 枚举在线会话（LIO 可真实枚举）。
func (b *Backend) ListSessions(ctx context.Context, name string) ([]platform.Session, error) {
	return b.lio.ListSessions(ctx, name)
}

// ForceLogout 强制登出指定会话。
func (b *Backend) ForceLogout(ctx context.Context, name, sessionID string) error {
	return b.lio.ForceLogout(ctx, name, sessionID)
}

// EnsureVirtualDisk 把虚拟磁盘纳入 LIO 登记（backstore 需要设备节点，故先激活）。
func (b *Backend) EnsureVirtualDisk(ctx context.Context, ref, description string) error {
	if err := b.activate(ctx, ref, false); err != nil {
		return err
	}
	return b.lio.EnsureVirtualDisk(ctx, ref, description)
}

// RemoveVirtualDisk 移除虚拟盘登记（不删除底层数据）。幂等。
func (b *Backend) RemoveVirtualDisk(ctx context.Context, ref string) error {
	return b.lio.RemoveVirtualDisk(ctx, ref)
}

// AttachLun 建立「虚拟磁盘 ↔ 目标」映射（先激活以保证设备节点存在）。
func (b *Backend) AttachLun(ctx context.Context, targetName, ref string, readOnly bool) error {
	if err := b.activate(ctx, ref, readOnly); err != nil {
		return err
	}
	return b.lio.AttachLun(ctx, targetName, ref, readOnly)
}

// DetachLun 解除映射。幂等（未映射视为成功）。
//
// 刻意**不**在此停用 LV：映射拆除后目标可能很快再次发布（客户端重连），
// 保持激活可省掉一次 lvchange；真正的停用由 linuxlvm.Delete 在删除前完成。
func (b *Backend) DetachLun(ctx context.Context, targetName, ref string) error {
	return b.lio.DetachLun(ctx, targetName, ref)
}

// activate 激活引用对应的 LV；ref 不是本后端能识别的 LV 引用时返回错误（交由上层暴露）。
func (b *Backend) activate(ctx context.Context, ref string, readOnly bool) error {
	return b.Manager.Activate(ctx, ref, readOnly)
}

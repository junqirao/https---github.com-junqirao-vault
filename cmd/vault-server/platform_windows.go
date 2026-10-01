//go:build windows

package main

import (
	"context"
	"log/slog"

	"vault/internal/config"
	"vault/internal/platform/iscsitarget"
	"vault/internal/platform/volume"
	"vault/internal/platform/winbackend"
	"vault/internal/platform/winps"
	"vault/internal/platform/winvhd"
)

// buildPlatform 装配 Windows 侧的平台后端。
//
// 抽象映射：虚拟磁盘 = VHDX 文件；iSCSI = IscsiTarget 角色模块；
// 存储池管理（LVM/dm-cache）在 Windows 上不存在，winbackend 会整体返回 ErrUnsupported。
// 存储底层卷（StorageVol）**刻意留 nil**：Windows 上"存储"就是 NTFS 卷上的普通目录，
// 没有卷概念，app 层据此直接走目录模式（若装配为非 nil，会让全部存储被 fail-closed 剔除）。
func buildPlatform(ctx context.Context, raw config.Config, log *slog.Logger) (platformDeps, error) {
	if err := checkPlatformKind(raw.Platform.Kind, "windows"); err != nil {
		return platformDeps{}, err
	}

	ps := winps.NewRunner(winps.Options{Logger: log})
	hyperV := winbackend.ProbeHyperV(ctx, ps)
	if !hyperV {
		log.Warn("未探测到 Hyper-V 模块：空间回收与磁盘标识重置可能不可用")
	}
	vhd := winvhd.NewManager(winvhd.Options{Logger: log, HyperVModuleAvailable: hyperV})
	vol := volume.NewManager(ps, log)
	iscsi := iscsitarget.NewManager(ps, log)

	// 启动即探测 iSCSI 目标服务器角色：缺失时"发布磁盘 / 挂载"必然失败
	// （表现为"无法创建 iSCSI 目标"），与其等到用户点按钮才报错，不如启动就说清楚。
	//
	// 探测失败**不阻断启动**：服务端其余能力（存储库管理、上传、用户等）仍然可用，
	// 只是磁盘发布与挂载不可用（相关接口会返回 system.unavailable）。
	if err := iscsi.Available(ctx); err != nil {
		log.Error("iSCSI 目标服务器不可用：磁盘发布与挂载将失败", "error", err,
			"hint", "请安装 iSCSI 目标服务器角色：Install-WindowsFeature FS-iSCSITarget-Server，并确认 WinTarget 服务已启动")
	} else {
		log.Info("iSCSI 目标服务器可用")
	}

	// 四个接口由同一个聚合对象提供（见 winbackend 包注释）。
	backend := winbackend.New(vhd, vol, iscsi, log)
	return platformDeps{Disk: backend, Vol: backend, Iscsi: backend, Platform: backend}, nil
}

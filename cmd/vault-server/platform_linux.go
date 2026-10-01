//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"vault/internal/config"
	"vault/internal/platform/linuxbackend"
	"vault/internal/platform/linuxlvm"
	"vault/internal/platform/liotarget"
)

// buildPlatform 装配 Linux 侧的平台后端。
//
// 抽象映射：虚拟磁盘 = LVM thin LV（差异盘 = thin snapshot）；
// iSCSI 目标 = LIO（直接读写 configfs，无需 targetcli/Python）；
// 存储池管理（建 VG/thin pool、挂 dm-cache）由 linuxlvm 的 StorageAdmin 实现承担；
// 每个"存储"各自的底层卷（thin LV + 文件系统 + 挂载点）由 StorageVolumeBackend 承担。
func buildPlatform(ctx context.Context, raw config.Config, log *slog.Logger) (platformDeps, error) {
	if err := checkPlatformKind(raw.Platform.Kind, "linux"); err != nil {
		return platformDeps{}, err
	}
	p := raw.Platform
	if err := checkLVMNames(p.LVM.VG, p.LVM.ThinPool); err != nil {
		return platformDeps{}, err
	}
	if b := strings.ToLower(strings.TrimSpace(p.Iscsi.Backend)); b != "" && b != "lio" {
		return platformDeps{}, fmt.Errorf("配置错误：platform.iscsi.backend=%q，Linux 目前只支持 lio", p.Iscsi.Backend)
	}

	lvm := linuxlvm.New(linuxlvm.Options{
		Logger:           log,
		VG:               p.LVM.VG,
		ThinPool:         p.LVM.ThinPool,
		ChunkSize:        p.LVM.ChunkSize,
		MetadataSize:     p.LVM.MetadataSize,
		WatermarkPercent: p.LVM.WatermarkPercent,
	})
	lio := liotarget.New(liotarget.Options{
		Logger:       log,
		ConfigFSRoot: p.Iscsi.ConfigFSRoot,
		IQNPrefix:    p.Iscsi.IQNPrefix,
	})

	// 启动期只探测、不阻断：缺少 LVM/LIO 时仍允许服务端起来（管理端可据此提示安装），
	// 真正用到相关能力时再报错——与 Windows 侧 Hyper-V 缺失的处理一致。
	if err := lvm.Available(ctx); err != nil {
		log.Warn("LVM 工具链不可用，虚拟磁盘功能将不可用", "error", err)
	}
	if err := lio.Available(ctx); err != nil {
		log.Warn("LIO configfs 不可用，iSCSI 目标功能将不可用（需 root 且已加载 target_core_mod/iscsi_target_mod）", "error", err)
	}

	backend := linuxbackend.New(lvm, lio, log)
	return platformDeps{Disk: backend, Vol: backend, Iscsi: backend, Platform: backend, StorageVol: backend}, nil
}

// checkLVMNames 校验 VG / thin pool 名。
//
// 约束来自 device-mapper 的转义规则：名字里的 '-' 会被映射为 "--"，
// 使 /dev/mapper/<vg>-<lv> 这种"第一个 '-' 即分隔符"的解析不可逆，
// 因此磁盘引用（ref）必须建立在"VG/LV 名不含 '-'"的前提上。
func checkLVMNames(vg, thinPool string) error {
	for _, it := range []struct{ key, val string }{
		{"platform.lvm.vg", vg},
		{"platform.lvm.thin_pool", thinPool},
	} {
		v := strings.TrimSpace(it.val)
		if v == "" {
			return fmt.Errorf("配置错误：%s 不能为空", it.key)
		}
		if strings.ContainsAny(v, "- \t/\\") {
			return fmt.Errorf("配置错误：%s=%q 含非法字符（不允许 '-'、空白与路径分隔符）", it.key, v)
		}
	}
	return nil
}

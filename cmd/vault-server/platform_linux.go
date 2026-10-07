//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"vault/internal/app"
	"vault/internal/config"
	"vault/internal/platform/linuxbackend"
	"vault/internal/platform/linuxlvm"
	"vault/internal/platform/liotarget"
	"vault/internal/platform/sysdeps"
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

	// ---- 系统依赖自检 + 自愈（装配后端之前做） ----
	//
	// 三类依赖缺任一项，都表现为"服务能起来、但某个功能在业务路径上才炸"：
	// configfs 未挂载 / LIO 模块未加载（或加载了却没在 configfs 里注册成功）/ 命令行工具缺失。
	// 这里先探测一轮：能自动修的（挂载 configfs、modprobe 与重载模块、写 modules-load.d）
	// 当场修掉；能自动装的（lvm2、e2fsprogs 等）按开关装掉；剩下的在日志里给出处置命令，
	// 并由前端横幅（GET /v1/system/deps）告诉使用者"缺了什么、怎么补"。
	//
	// 与下面的"探测但不阻断"一致：自检不通过也不阻止启动——运维还得连上管理端才能看到提示。
	sysdepsOpts := sysdeps.Options{
		Logger:       log,
		ConfigFSRoot: p.Iscsi.ConfigFSRoot,
		Repair:       p.AutoRepairEnabled(),
		Install:      p.AutoInstallEnabled(),
	}
	startup := sysdeps.Ensure(ctx, sysdepsOpts)
	if startup.OK() {
		if fixed := startup.FixedKeys(); len(fixed) > 0 {
			log.Info("系统依赖自检通过（启动时已自动修复缺项）",
				"fixed", strings.Join(fixed, ","), "elapsed_ms", startup.ElapsedMS)
		} else {
			log.Debug("系统依赖自检通过", "elapsed_ms", startup.ElapsedMS)
		}
	} else {
		// 缺项既写日志（给运维）也随 GET /v1/system/deps 返回（给使用者，管理端弹横幅）。
		log.Warn("系统依赖自检未通过：相关功能将不可用（管理端会显示横幅；执行 Vault-Server doctor 可自动修复并查看处置命令）",
			"missing", strings.Join(startup.MissingKeys(), ","))
	}

	// 接口侧改成**实时只读探测**：运维把缺项补齐后，前端横幅刷新即消失，不必重启服务端。
	// 修复仍然只在启动期与 doctor 里做——HTTP 请求不该悄悄改宿主机。
	sysdepsProbe := func(ctx context.Context) *app.SysDepsReport {
		return sysDepsReport(ctx, sysdepsOpts, startup.FixedKeys())
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
	return platformDeps{
		Disk: backend, Vol: backend, Iscsi: backend, Platform: backend, StorageVol: backend,
		SysDepsProbe: sysdepsProbe,
	}, nil
}

// sysDepsReport 把 sysdeps.Report 转成 app 层的平台中立 DTO。
//
// 转换放在这里而不是 app 层：sysdeps 是 `//go:build linux` 的包，
// app/api 必须保持跨平台可编译（同 platform_* 文件的取舍）。
//
// fixedKeys 是启动期自动修复留下的记录：实时探测对已修好的项只会报 ok，不会再报"已修复"，
// 合并进来前端才能显示"开机时已自动修复了哪些"。
func sysDepsReport(ctx context.Context, o sysdeps.Options, fixedKeys []string) *app.SysDepsReport {
	r := sysdeps.Detect(ctx, o)
	items := make([]app.SysDepsItem, 0, len(r.Items))
	missing := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		items = append(items, app.SysDepsItem{
			Key:      it.Key,
			Title:    it.Title,
			Required: it.Required,
			Status:   string(it.Status),
			Detail:   it.Detail,
			Fixed:    it.Fixed,
			Hint:     it.Hint,
		})
		if it.Required && it.Status == sysdeps.StatusMissing {
			missing = append(missing, it.Key)
		}
	}

	fixed := make([]string, 0, len(fixedKeys))
	seen := make(map[string]bool, len(fixedKeys))
	for _, k := range fixedKeys {
		if !seen[k] {
			seen[k] = true
			fixed = append(fixed, k)
		}
	}
	for _, it := range r.Items {
		if it.Status == sysdeps.StatusFixed && !seen[it.Key] {
			seen[it.Key] = true
			fixed = append(fixed, it.Key)
		}
	}

	// RepairEnabled/InstallEnabled 取**配置值**：Detect 的报告里恒为 false（它不修复），
	// 直接用会让前端误判成"自动修复被关掉了"。
	return &app.SysDepsReport{
		Supported:      true,
		OK:             len(missing) == 0,
		CheckedAt:      r.CheckedAt,
		ElapsedMS:      r.ElapsedMS,
		Root:           r.Root,
		RepairEnabled:  o.Repair,
		InstallEnabled: o.Install,
		ConfigFSRoot:   r.ConfigFSRoot,
		Items:          items,
		Missing:        missing,
		Fixed:          fixed,
	}
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

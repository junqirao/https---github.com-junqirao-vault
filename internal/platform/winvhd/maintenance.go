//go:build windows

package winvhd

import (
	"context"
	"net/http"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// ResetDiskIdentifier 重置 VHDX 的磁盘标识（DiskIdentifier）。
//
// 复制得到的 VHDX 会继承源盘相同的磁盘标识，Windows 会报 Event ID 158（KB2983588），
// 导致 VSS/备份失败，因此 Copy 之后必须调用本方法。
//
// 实现路径：
//   - 优先走 Hyper-V 模块的 `Set-VHD -ResetDiskIdentifier`（官方确认可用）；
//   - 目标环境无 Hyper-V 模块（Options.HyperVModuleAvailable 为 false）时，返回
//     platform.reset_disk_id_unavailable，由上层决定降级策略
//     （例如提示"需安装 Hyper-V 管理工具"或放弃复制）。
//
// 备选路径（当前未实现，需实测确认）：走 VirtDisk API 的
// OpenVirtualDisk + SetVirtualDiskInformation(SET_VIRTUAL_DISK_INFO_IDENTIFIER)。
// go-winio/vhd 尚未绑定 SetVirtualDiskInformation，因此这里只保留 Hyper-V 路径。
func (m *Manager) ResetDiskIdentifier(ctx context.Context, path string) error {
	if !m.hyperVAvailable {
		m.logger.Warn("缺少 Hyper-V 模块，无法重置磁盘标识；请由上层决定降级策略", "path", path)
		return apperr.New(CodeResetDiskIDUnavailable, http.StatusInternalServerError)
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	params := []winps.Param{winps.String("Path", path)}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVHDResetDiskIdentifier, params, nil); err != nil {
		return err
	}
	m.logger.Info("已重置 VHDX 磁盘标识", "path", path)
	return nil
}

// Optimize 回收 VHDX 空间（Retrim / Compact）。
//
// 只应对"空闲"的动态扩展盘执行（未挂载或只读挂载），且同一卷同一时刻只跑一个任务
// （磁盘 IO 密集）。调度约束由上层负责。
func (m *Manager) Optimize(ctx context.Context, path string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	params := []winps.Param{
		winps.String("Path", path),
		winps.String("Mode", "Full"),
	}
	var out struct {
		Method string `json:"method"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVHDOptimize, params, &out); err != nil {
		return err
	}
	m.logger.Info("已回收 VHDX 空间", "path", path, "method", out.Method)
	return nil
}

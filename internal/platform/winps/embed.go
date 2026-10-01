package winps

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// 注意：scripts/*.ps1 必须保存为 **UTF-8 with BOM**。
// Windows PowerShell 5.1 对没有 BOM 的 .ps1 会按系统 ANSI 代码页（中文环境为 GBK）解析，
// 导致脚本中的中文字符串/注释乱码甚至语法错误。
//
//go:embed scripts/*.ps1
var scriptsFS embed.FS

// 内嵌脚本名。调用方请使用这些常量，避免拼错文件名。
const (
	// ScriptIscsiAvailable 探测 iSCSI 目标服务器能力。
	ScriptIscsiAvailable = "iscsi_available.ps1"
	// ScriptIscsiEnsureVirtualDisk 幂等地把 VHDX 纳入 iSCSI 虚拟盘登记。
	ScriptIscsiEnsureVirtualDisk = "iscsi_ensure_virtual_disk.ps1"
	// ScriptIscsiRemoveVirtualDisk 移除 iSCSI 虚拟盘登记（不删除 .vhdx 文件）。
	ScriptIscsiRemoveVirtualDisk = "iscsi_remove_virtual_disk.ps1"
	// ScriptIscsiGetVirtualDisks 查询 iSCSI 虚拟盘。
	ScriptIscsiGetVirtualDisks = "iscsi_get_virtual_disks.ps1"
	// ScriptIscsiSetTarget 创建或更新 iSCSI 目标（含授权、CHAP、启用状态）。
	ScriptIscsiSetTarget = "iscsi_set_target.ps1"
	// ScriptIscsiRemoveTarget 删除 iSCSI 目标（先解除其全部映射）。
	ScriptIscsiRemoveTarget = "iscsi_remove_target.ps1"
	// ScriptIscsiAddMapping 建立「虚拟盘 ↔ 目标」映射（幂等）。
	ScriptIscsiAddMapping = "iscsi_add_mapping.ps1"
	// ScriptIscsiRemoveMapping 解除「虚拟盘 ↔ 目标」映射（幂等）。
	ScriptIscsiRemoveMapping = "iscsi_remove_mapping.ps1"
	// ScriptIscsiGetTargets 查询 iSCSI 目标（JSON 数组）。
	ScriptIscsiGetTargets = "iscsi_get_targets.ps1"
	// ScriptVolumeFindDisk 按 Get-Disk 的 Location 反查 VHDX 对应的磁盘号。
	ScriptVolumeFindDisk = "volume_find_disk.ps1"
	// ScriptVolumeEnsureFormatted 幂等地初始化/分区/格式化 VHDX 并返回盘符。
	ScriptVolumeEnsureFormatted = "volume_ensure_formatted.ps1"
	// ScriptVolumeSetDiskState 设置磁盘上下线 / 只读。
	ScriptVolumeSetDiskState = "volume_set_disk_state.ps1"
	// ScriptVolumeFreeSpace 查询路径所在卷的可用/总空间。
	ScriptVolumeFreeSpace = "volume_freespace.ps1"
	// ScriptVolumeFileSystem 查询路径所在卷的文件系统。
	ScriptVolumeFileSystem = "volume_filesystem.ps1"
	// ScriptVolumeUpdateCache 刷新存储缓存（Update-HostStorageCache）。
	ScriptVolumeUpdateCache = "volume_update_cache.ps1"
	// ScriptVolumeFindIscsiDisk 按容量与已占用磁盘列表定位客户端侧 iSCSI 磁盘。
	ScriptVolumeFindIscsiDisk = "volume_find_iscsi_disk.ps1"
	// ScriptVolumeMountDrive 为磁盘分配盘符（已有盘符则复用）。
	ScriptVolumeMountDrive = "volume_mount_drive.ps1"
	// ScriptVolumeMountDir 把分区挂载到已存在的空目录。
	ScriptVolumeMountDir = "volume_mount_dir.ps1"
	// ScriptVolumeUnmount 移除挂载点（盘符或目录）。
	ScriptVolumeUnmount = "volume_unmount.ps1"
	// ScriptVolumeCurrentMount 查询磁盘当前挂载点（盘符优先，其次目录）。
	ScriptVolumeCurrentMount = "volume_current_mount.ps1"
	// ScriptVolumeSetLabel 设置磁盘所在卷的卷标（盘符模式下用存储库名称命名该盘）。
	ScriptVolumeSetLabel = "volume_set_label.ps1"
	// ScriptInitiatorAvailable 探测 iSCSI 发起端（Initiator）能力。
	ScriptInitiatorAvailable = "initiator_available.ps1"
	// ScriptInitiatorEnsureService 确保 iSCSI 发起端服务（MSiSCSI）处于运行状态。
	//
	// 挂载前必须调用：连接类 cmdlet 依赖该服务的 WMI 提供程序，服务未运行时
	// Connect-IscsiTarget 直接抛 CIM 异常，真因会被压成 connect_failed（见 5.5 步骤 ③a）。
	ScriptInitiatorEnsureService = "initiator_ensure_service.ps1"
	// ScriptInitiatorEnsurePortal 幂等添加 iSCSI 目标门户。
	ScriptInitiatorEnsurePortal = "initiator_ensure_portal.ps1"
	// ScriptInitiatorConnect 连接 iSCSI 目标（支持单向 CHAP）。
	ScriptInitiatorConnect = "initiator_connect.ps1"
	// ScriptInitiatorWaitConnected 轮询等待 iSCSI 会话建立。
	ScriptInitiatorWaitConnected = "initiator_wait_connected.ps1"
	// ScriptInitiatorIsConnected 查询 iSCSI 目标是否已连接。
	ScriptInitiatorIsConnected = "initiator_is_connected.ps1"
	// ScriptInitiatorDisconnect 断开 iSCSI 目标的全部会话。
	ScriptInitiatorDisconnect = "initiator_disconnect.ps1"
	// ScriptInitiatorUnregister 取消 iSCSI 会话持久化。
	ScriptInitiatorUnregister = "initiator_unregister.ps1"
	// ScriptInitiatorRemoveTarget 清理已发现的 iSCSI 目标缓存。
	ScriptInitiatorRemoveTarget = "initiator_remove_target.ps1"
	// ScriptVHDResetDiskIdentifier 重置 VHDX 磁盘标识（Set-VHD -ResetDiskIdentifier）。
	ScriptVHDResetDiskIdentifier = "vhd_reset_disk_identifier.ps1"
	// ScriptVHDOptimize 回收 VHDX 空间（Retrim / Compact）。
	ScriptVHDOptimize = "vhd_optimize.ps1"
	// ScriptTLSCertFingerprint 查询本机证书指纹（可选能力）。
	ScriptTLSCertFingerprint = "tls_cert_fingerprint.ps1"
)

var (
	materializeOnce sync.Once
	materializeDir  string
	materializeErr  error
)

// ScriptPath 返回内嵌脚本物化到本地后的绝对路径。
//
// PowerShell 的 `-File` 需要一个真实的文件路径，因此内嵌脚本会在首次调用时
// 一次性释放到系统临时目录下的独立子目录（进程退出后由系统清理）。
func ScriptPath(name string) (string, error) {
	dir, err := materialize()
	if err != nil {
		return "", err
	}
	base := filepath.Base(name)
	p := filepath.Join(dir, base)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("winps: 未找到内嵌脚本 %q: %w", base, err)
	}
	return p, nil
}

// materialize 把全部内嵌脚本释放到临时目录，只做一次。
func materialize() (string, error) {
	materializeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "vault-ps-scripts-")
		if err != nil {
			materializeErr = err
			return
		}
		entries, err := fs.ReadDir(scriptsFS, "scripts")
		if err != nil {
			materializeErr = err
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, err := scriptsFS.ReadFile("scripts/" + e.Name())
			if err != nil {
				materializeErr = err
				return
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
				materializeErr = err
				return
			}
		}
		materializeDir = dir
	})
	return materializeDir, materializeErr
}

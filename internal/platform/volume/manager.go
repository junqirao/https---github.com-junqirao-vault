// Package volume 封装 Windows Storage 模块（Get-Disk / Initialize-Disk / New-Partition /
// Format-Volume / Set-Disk / Get-Volume），提供磁盘-卷层面的操作能力。
//
// 所有命令都通过 internal/platform/winps 以 `-File <脚本> -Name value` 方式执行，
// 参数不做字符串拼接；脚本的原始报错文本只进日志，返回给上层的是平台错误码。
package volume

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"vault/internal/apperr"
	"vault/internal/domain"
	"vault/internal/platform/winps"
)

// Manager 是磁盘/卷操作入口。
type Manager struct {
	ps     *winps.Runner
	logger *slog.Logger
}

// NewManager 构造 Manager。runner 为 nil 时使用默认配置的执行器。
func NewManager(runner *winps.Runner, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if runner == nil {
		runner = winps.NewRunner(winps.Options{Logger: logger})
	}
	return &Manager{ps: runner, logger: logger}
}

// DiskInfo 描述一块由 VHDX 映射而来的磁盘。
type DiskInfo struct {
	// Number 磁盘号（Get-Disk 的 Number）。
	Number int `json:"number"`
	// PartitionStyle RAW / MBR / GPT。
	PartitionStyle string `json:"partition_style"`
	// Size 磁盘容量（字节）。
	Size int64 `json:"size"`
	// Location 磁盘来源位置；对 VHDX 而言就是 VHDX 文件全路径。
	Location string `json:"location"`
}

// VolumeInfo 描述分区/卷的格式化结果。
type VolumeInfo struct {
	// DriveLetter 分配到的盘符（如 "E"）；无可用盘符时为空串。
	DriveLetter string `json:"drive_letter"`
	// PartitionNumber 分区号。
	PartitionNumber int `json:"partition_number"`
	// FileSystem 卷的文件系统名。
	FileSystem string `json:"file_system"`
	// SizeBytes 卷容量（字节）。
	SizeBytes int64 `json:"size_bytes"`
}

// findDiskResult 是 volume_find_disk.ps1 的 JSON 输出。
type findDiskResult struct {
	Number         int    `json:"number"`
	PartitionStyle string `json:"partition_style"`
	Size           int64  `json:"size"`
	Location       string `json:"location"`
}

// ensureFormattedResult 是 volume_ensure_formatted.ps1 的 JSON 输出。
type ensureFormattedResult struct {
	DriveLetter     string `json:"drive_letter"`
	PartitionNumber int    `json:"partition_number"`
	FileSystem      string `json:"file_system"`
	SizeBytes       int64  `json:"size_bytes"`
	DiskNumber      int    `json:"disk_number"`
	PartitionStyle  string `json:"partition_style"`
}

// FindDiskByVHDXPath 通过 Get-Disk 的 Location 属性匹配 VHDX 全路径拿到磁盘号。
func (m *Manager) FindDiskByVHDXPath(ctx context.Context, vhdxPath string) (*DiskInfo, error) {
	if strings.TrimSpace(vhdxPath) == "" {
		return nil, apperr.InvalidParam("vhdx_path")
	}
	params := []winps.Param{winps.String("VhdxPath", vhdxPath)}
	var result findDiskResult
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeFindDisk, params, &result); err != nil {
		// 脚本用 reason=disk_not_found 明确表达"VHDX 未挂载/不可见"，映射为领域错误码。
		var scriptErr *winps.ScriptError
		if errors.As(err, &scriptErr) && scriptErr.Reason == "disk_not_found" {
			return nil, apperr.DiskNotFound()
		}
		return nil, err
	}
	return &DiskInfo{
		Number:         result.Number,
		PartitionStyle: result.PartitionStyle,
		Size:           result.Size,
		Location:       result.Location,
	}, nil
}

// EnsureFormatted 幂等地初始化 + 分区 + 格式化 VHDX，返回分配的卷信息。
//
// 幂等保证：
//   - 磁盘已是 GPT/MBR 时不再 Initialize-Disk；
//   - 已有分区时复用；
//   - 已有文件系统且与 fileSystem 一致时跳过格式化；
//   - 已有其它文件系统时报错（避免覆盖已有数据）。
//
// label 为空时脚本使用默认标签 "VAULT"。
func (m *Manager) EnsureFormatted(ctx context.Context, vhdxPath, fileSystem, label string) (*VolumeInfo, error) {
	if strings.TrimSpace(vhdxPath) == "" {
		return nil, apperr.InvalidParam("vhdx_path")
	}
	if strings.TrimSpace(fileSystem) == "" {
		return nil, apperr.InvalidParam("file_system")
	}
	params := []winps.Param{
		winps.String("VhdxPath", vhdxPath),
		winps.String("FileSystem", fileSystem),
		winps.String("Label", label),
	}
	var result ensureFormattedResult
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeEnsureFormatted, params, &result); err != nil {
		var scriptErr *winps.ScriptError
		if errors.As(err, &scriptErr) && scriptErr.Reason == "disk_not_found" {
			return nil, apperr.DiskNotFound()
		}
		return nil, err
	}
	return &VolumeInfo{
		DriveLetter:     result.DriveLetter,
		PartitionNumber: result.PartitionNumber,
		FileSystem:      result.FileSystem,
		SizeBytes:       result.SizeBytes,
	}, nil
}

// SetOffline 设置磁盘上/下线。
//
// 客户端卸载流程要求：先把盘 Offline 再 Disconnect-IscsiTarget，
// 否则断开时会返回 HRESULT 0xefff0040（会话上有在线设备无法登出）。
func (m *Manager) SetOffline(ctx context.Context, diskNumber int, offline bool) error {
	return m.setDiskState(ctx, diskNumber, winps.Bool("Offline", offline))
}

// SetReadOnly 设置磁盘只读。
func (m *Manager) SetReadOnly(ctx context.Context, diskNumber int, readOnly bool) error {
	return m.setDiskState(ctx, diskNumber, winps.Bool("ReadOnly", readOnly))
}

func (m *Manager) setDiskState(ctx context.Context, diskNumber int, state winps.Param) error {
	if diskNumber < 0 {
		return apperr.InvalidParam("disk_number")
	}
	params := []winps.Param{
		winps.String("DiskNumber", strconv.Itoa(diskNumber)),
		state,
	}
	return m.ps.RunScriptJSON(ctx, winps.ScriptVolumeSetDiskState, params, nil)
}

// SpaceUsageOf 返回指定路径所在卷的标识、文件系统与可用/总空间。
//
// 卷标识优先取脚本输出的盘符（drive_letter），缺失时回退 filepath.VolumeName；
// 结果已归一为小写，便于按卷去重（见 domain.PathGuardSet.Volumes）。
func (m *Manager) SpaceUsageOf(ctx context.Context, path string) (*domain.VolumeSpace, error) {
	if strings.TrimSpace(path) == "" {
		return nil, apperr.InvalidParam("path")
	}
	params := []winps.Param{winps.String("Path", path)}
	var result struct {
		Free        int64  `json:"free"`
		Total       int64  `json:"total"`
		FileSystem  string `json:"file_system"`
		DriveLetter string `json:"drive_letter"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeFreeSpace, params, &result); err != nil {
		return nil, err
	}
	return &domain.VolumeSpace{
		Name:       domain.NormalizeVolumeName(result.DriveLetter, path),
		FileSystem: result.FileSystem,
		FreeBytes:  result.Free,
		TotalBytes: result.Total,
	}, nil
}

// FreeSpace 返回指定路径所在卷的可用/总空间（字节）。
//
// 保留该便捷签名以兼容既有调用；需要卷标识/文件系统时用 SpaceUsageOf。
func (m *Manager) FreeSpace(ctx context.Context, path string) (free, total int64, err error) {
	space, err := m.SpaceUsageOf(ctx, path)
	if err != nil {
		return 0, 0, err
	}
	return space.FreeBytes, space.TotalBytes, nil
}

// FileSystemOf 返回路径所在卷的文件系统名（如 NTFS / ReFS）。
func (m *Manager) FileSystemOf(ctx context.Context, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", apperr.InvalidParam("path")
	}
	params := []winps.Param{winps.String("Path", path)}
	var result struct {
		FileSystem string `json:"file_system"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeFileSystem, params, &result); err != nil {
		return "", err
	}
	if result.FileSystem == "" {
		return "", apperr.New(apperr.CodeUnavailable, http.StatusInternalServerError).WithArg("path", path)
	}
	return result.FileSystem, nil
}

// IsNTFS 便捷判断：路径所在卷是否为 NTFS。
//
// 本项目只支持 NTFS（ReFS 才是块克隆的唯一可用文件系统，但 VHDX 白名单要求 NTFS）。
func (m *Manager) IsNTFS(ctx context.Context, path string) (bool, error) {
	fileSystem, err := m.FileSystemOf(ctx, path)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(fileSystem, "NTFS"), nil
}

// UpdateHostStorageCache 刷新主机存储缓存。
//
// 挂载 VHDX 后立即 Get-Disk 可能查不到对应磁盘，需要先调用本方法。
func (m *Manager) UpdateHostStorageCache(ctx context.Context) error {
	return m.ps.RunScriptJSON(ctx, winps.ScriptVolumeUpdateCache, nil, nil)
}

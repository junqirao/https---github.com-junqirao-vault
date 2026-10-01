package volume

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// MountInfo 描述磁盘的分区挂载信息。
type MountInfo struct {
	// MountPath letter 模式为 "E:"；directory 模式为目录绝对路径；未挂载为空串。
	MountPath string `json:"mount_path"`
	// MountMode letter | directory；未挂载为空串。
	MountMode string `json:"mount_mode"`
	// DiskNumber 磁盘号。
	DiskNumber int `json:"disk_number"`
	// PartitionNumber 分区号。
	PartitionNumber int `json:"partition_number"`
}

// FindIscsiDisk 在 BusType=iSCSI 的磁盘中按容量定位客户端侧磁盘，返回磁盘号。
//
// 已占用磁盘号（usedDiskNumbers）会被排除，避免同一容量下多块盘互相抢占。
//
// ⚠️ 多块同尺寸 iSCSI 盘时仅凭容量无法精确定位，真实环境需要用 unique id
// （Get-Disk 的 UniqueId / SerialNumber / Path）配合服务端下发的 disk_unique_id；
// 本方法目前取编号最小的候选并记录 WARN —— 需实测确认。
func (m *Manager) FindIscsiDisk(ctx context.Context, sizeBytes int64, usedDiskNumbers []int) (int, error) {
	if sizeBytes <= 0 {
		return 0, apperr.InvalidParam("size_bytes")
	}
	if usedDiskNumbers == nil {
		usedDiskNumbers = []int{}
	}
	usedParam, err := winps.JSONParam("UsedDiskNumbers", usedDiskNumbers)
	if err != nil {
		return 0, apperr.InvalidParam("used_disk_numbers")
	}
	params := []winps.Param{
		winps.String("SizeBytes", strconv.FormatInt(sizeBytes, 10)),
		usedParam,
	}
	var result struct {
		Number         int `json:"number"`
		CandidateCount int `json:"candidate_count"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeFindIscsiDisk, params, &result); err != nil {
		if mapped := mappedReasonError(err, map[string]*apperr.Error{
			"disk_not_found": ErrIscsiDiskNotFound(),
		}); mapped != nil {
			return 0, mapped
		}
		return 0, err
	}
	if result.CandidateCount > 1 {
		m.logger.Warn("存在多块同容量 iSCSI 磁盘，已取编号最小的候选（精确匹配需靠 unique id，需实测确认）",
			"size_bytes", sizeBytes,
			"candidate_count", result.CandidateCount,
			"disk_number", result.Number)
	}
	return result.Number, nil
}

// MountToDriveLetter 为磁盘分配盘符，返回盘符（如 "E:"）。
//
// 若磁盘已有盘符则直接复用（幂等）。
func (m *Manager) MountToDriveLetter(ctx context.Context, diskNumber int) (string, error) {
	if diskNumber < 0 {
		return "", apperr.InvalidParam("disk_number")
	}
	params := []winps.Param{winps.String("DiskNumber", strconv.Itoa(diskNumber))}
	var result struct {
		DriveLetter string `json:"drive_letter"`
		Action      string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeMountDrive, params, &result); err != nil {
		if mapped := mappedReasonError(err, map[string]*apperr.Error{
			"no_partition": ErrNoPartition(),
		}); mapped != nil {
			return "", mapped
		}
		return "", err
	}
	if strings.TrimSpace(result.DriveLetter) == "" {
		return "", apperr.New(CodeMountFailed, http.StatusInternalServerError).
			WithArg("disk_number", diskNumber)
	}
	m.logger.Info("已分配盘符", "disk_number", diskNumber, "drive_letter", result.DriveLetter, "action", result.Action)
	return result.DriveLetter, nil
}

// MountToDirectory 把分区挂载到指定目录。
//
// 目录必须已存在且为空，且其所在卷为 NTFS；不满足时返回明确错误
// （platform.mount_dir_not_found / platform.mount_dir_not_empty / platform.mount_dir_not_ntfs）。
func (m *Manager) MountToDirectory(ctx context.Context, diskNumber int, dir string) error {
	if diskNumber < 0 {
		return apperr.InvalidParam("disk_number")
	}
	if strings.TrimSpace(dir) == "" {
		return apperr.InvalidParam("mount_path")
	}
	params := []winps.Param{
		winps.String("DiskNumber", strconv.Itoa(diskNumber)),
		winps.String("AccessPath", dir),
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeMountDir, params, nil); err != nil {
		if mapped := mappedReasonError(err, map[string]*apperr.Error{
			"dir_not_found":   ErrMountDirNotFound(dir),
			"not_a_directory": ErrMountDirNotFound(dir),
			"dir_not_empty":   ErrMountDirNotEmpty(dir),
			"parent_not_ntfs": ErrMountDirNotNTFS(dir),
			"no_partition":    ErrNoPartition(),
		}); mapped != nil {
			return mapped
		}
		return err
	}
	m.logger.Info("已挂载到目录", "disk_number", diskNumber, "path", dir)
	return nil
}

// Unmount 移除挂载点（盘符或目录），不改变磁盘数据。幂等。
func (m *Manager) Unmount(ctx context.Context, mountPath string) error {
	if strings.TrimSpace(mountPath) == "" {
		return apperr.InvalidParam("mount_path")
	}
	params := []winps.Param{winps.String("AccessPath", mountPath)}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeUnmount, params, nil); err != nil {
		return err
	}
	m.logger.Info("已移除挂载点", "mount_path", mountPath)
	return nil
}

// CurrentMountPath 返回磁盘当前的挂载点（盘符优先，其次目录）；未挂载返回 ""。
func (m *Manager) CurrentMountPath(ctx context.Context, diskNumber int) (string, error) {
	if diskNumber < 0 {
		return "", apperr.InvalidParam("disk_number")
	}
	params := []winps.Param{winps.String("DiskNumber", strconv.Itoa(diskNumber))}
	var result struct {
		MountPath string `json:"mount_path"`
		MountMode string `json:"mount_mode"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeCurrentMount, params, &result); err != nil {
		return "", err
	}
	return result.MountPath, nil
}

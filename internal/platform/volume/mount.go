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

// SetLabel 设置磁盘所在卷的卷标（如把"存储库名称"写到盘符上）。
//
// 前置条件：该卷已有盘符（Windows 只能用盘符或挂载路径定位卷）。
// label 建议由调用方先做字符清洗与长度截断（NTFS 上限 32 字符），见 agent 的 volumeLabelOf。
func (m *Manager) SetLabel(ctx context.Context, diskNumber int, label string) error {
	if diskNumber < 0 {
		return apperr.InvalidParam("disk_number")
	}
	if strings.TrimSpace(label) == "" {
		return apperr.InvalidParam("label")
	}
	params := []winps.Param{
		winps.String("DiskNumber", strconv.Itoa(diskNumber)),
		winps.String("Label", label),
	}
	var result struct {
		DriveLetter string `json:"drive_letter"`
		Label       string `json:"file_system_label"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeSetLabel, params, &result); err != nil {
		if mapped := mappedReasonError(err, map[string]*apperr.Error{
			"no_partition": ErrNoPartition(),
		}); mapped != nil {
			return mapped
		}
		return err
	}
	m.logger.Info("已设置卷标",
		"disk_number", diskNumber, "drive_letter", result.DriveLetter, "label", result.Label)
	return nil
}

// MountToDirectory 把分区挂载到指定目录。
//
// 目录不存在时由脚本创建（等价 mkdir -p），存在则必须为空，且其所在卷为 NTFS；不满足时返回
// 明确错误（platform.mount_dir_not_found / platform.mount_dir_not_empty /
// platform.mount_dir_not_ntfs）。
//
// ⚠️ "目录由脚本创建"不是分工问题：父目录可能是**本磁盘自己的残留挂载点**（老版本把卷直接挂到
// 父目录上，卸载后 junction 还在原地），那样建目录会把目录建进本卷内部，卷随即被挂到自己的
// 卷内路径上 —— 挂载点自指，资源管理器无限嵌套（真实事故）。识别要读 junction 的 Target 比卷
// GUID，只有脚本做得到；脚本会先清掉残留再在真实目录里建挂载点（见 volume_mount_dir.ps1 的
// .NOTES）。action=healed 即表示这一步真的修复过。
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
	var result struct {
		Action  string `json:"action"`
		Created bool   `json:"created"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeMountDir, params, &result); err != nil {
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
	if result.Action == "healed" {
		// 修复过就告警一次：这台机器此前残留过卷挂载点，出问题时这是第一线索。
		m.logger.Warn("挂载点下方是本磁盘残留的挂载点，已清理后重新挂载",
			"disk_number", diskNumber, "path", dir)
	}
	m.logger.Info("已挂载到目录",
		"disk_number", diskNumber, "path", dir, "action", result.Action, "created", result.Created)
	return nil
}

// Unmount 移除挂载点（盘符或目录），不改变磁盘数据。幂等。
//
// action=cleaned 表示分区已经不认领这个路径，但路径本身仍是残留的卷挂载点（junction），本次
// 顺手摘掉了它 —— 这是清理僵尸挂载点的第二条路（第一条在 MountToDirectory）。
func (m *Manager) Unmount(ctx context.Context, mountPath string) error {
	if strings.TrimSpace(mountPath) == "" {
		return apperr.InvalidParam("mount_path")
	}
	params := []winps.Param{winps.String("AccessPath", mountPath)}
	var result struct {
		Action string `json:"action"`
	}
	if err := m.ps.RunScriptJSON(ctx, winps.ScriptVolumeUnmount, params, &result); err != nil {
		return err
	}
	if result.Action == "cleaned" {
		m.logger.Warn("分区上已无该挂载点记录，但路径仍是残留的卷挂载点，已清理",
			"mount_path", mountPath)
	}
	m.logger.Info("已移除挂载点", "mount_path", mountPath, "action", result.Action)
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

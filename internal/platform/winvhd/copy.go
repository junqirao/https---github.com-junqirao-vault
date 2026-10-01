//go:build windows

package winvhd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// copySpaceFactor 空间预检系数：目标卷可用空间需 >= 源文件大小 * 1.1。
const copySpaceFactor = 1.1

// Copy 全量复制 VHDX。
//
// NTFS 不支持块克隆（FSCTL_DUPLICATE_EXTENTS_TO_FILE 仅 ReFS 可用），因此复制必然是
// 全量物理拷贝：复制 N GB 就需要额外 N GB 空闲空间。
//
// 流程：空间预检 → robocopy /J → 校验尺寸 → **重置 DiskIdentifier**。
// 最后一步不可省略：直接文件复制会继承源盘相同的磁盘标识，Windows 会报
// Event ID 158「Disk N has the same disk identifiers...」(KB2983588)，导致 VSS/备份失败。
func (m *Manager) Copy(ctx context.Context, src, dst string) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if src == "" || dst == "" {
		return apperr.InvalidParam("path")
	}
	srcClean := filepath.Clean(src)
	dstClean := filepath.Clean(dst)
	if strings.EqualFold(srcClean, dstClean) {
		return apperr.InvalidParam("dst")
	}

	sourceInfo, err := os.Stat(srcClean)
	if err != nil {
		return mapVHDError("copy_src", err)
	}
	if sourceInfo.IsDir() {
		return apperr.InvalidParam("src")
	}

	if err := ensureFreeSpace(dstClean, uint64(float64(sourceInfo.Size())*copySpaceFactor)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstClean), 0o755); err != nil {
		return mapVHDError("mkdir", err)
	}

	srcDir, srcName := filepath.Split(srcClean)
	dstDir := filepath.Dir(dstClean)
	if err := m.runRobocopy(ctx, srcDir, dstDir, srcName); err != nil {
		return err
	}

	// robocopy 以源文件名落盘；目标文件名不同时在同一卷内重命名（原子操作）。
	produced := filepath.Join(dstDir, srcName)
	if !strings.EqualFold(produced, dstClean) {
		if err := os.Rename(produced, dstClean); err != nil {
			return mapVHDError("copy_rename", err)
		}
	}

	copiedInfo, err := os.Stat(dstClean)
	if err != nil {
		return mapVHDError("copy_verify", err)
	}
	if copiedInfo.Size() != sourceInfo.Size() {
		return apperr.New(CodeCopyFailed, http.StatusInternalServerError).
			WithArg("src_size", sourceInfo.Size()).
			WithArg("dst_size", copiedInfo.Size())
	}

	if err := m.ResetDiskIdentifier(ctx, dstClean); err != nil {
		return err
	}

	m.logger.Info("已复制 VHDX", "src", srcClean, "dst", dstClean, "size_bytes", copiedInfo.Size())
	return nil
}

// runRobocopy 用 robocopy /J 做无缓冲顺序拷贝。
//
// robocopy 退出码约定：0~7 表示成功或部分成功（含"文件已是最新，未复制"），>= 8 才是真失败。
func (m *Manager) runRobocopy(ctx context.Context, srcDir, dstDir, fileName string) error {
	args := []string{
		srcDir, dstDir, fileName,
		"/J", "/COPY:DAT", "/R:1", "/W:1",
		"/NFL", "/NDL", "/NP", "/NJH", "/NJS",
	}
	cmd := exec.CommandContext(ctx, "robocopy", args...)
	// robocopy 是控制台程序：不隐藏窗口时会在无控制台的父进程下弹出黑框。
	winps.PrepareHiddenCommand(cmd)
	output, err := cmd.CombinedOutput()

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			m.logger.Error("robocopy 启动失败", "err", err.Error())
			return apperr.New(CodeCopyFailed, http.StatusInternalServerError).WithCause(err)
		}
		exitCode = exitErr.ExitCode()
	}

	if ctx.Err() != nil {
		return apperr.New(CodeCopyFailed, http.StatusInternalServerError).WithCause(ctx.Err())
	}
	if exitCode >= 8 {
		m.logger.Error("robocopy 复制失败",
			"exit_code", exitCode,
			"output", limitText(string(output)))
		return apperr.New(CodeCopyFailed, http.StatusInternalServerError).WithArg("exit_code", exitCode)
	}
	if exitCode != 0 {
		m.logger.Debug("robocopy 部分成功", "exit_code", exitCode)
	}
	return nil
}

// ensureFreeSpace 检查 dst 所在卷的可用空间是否满足 need 字节。
//
// 注意：这是复制前的**单点预检**，用原生 GetDiskFreeSpaceEx 读取目标卷可用空间，
// 与对外统计（domain.PathGuardSet.Volumes 的按卷去重合计）是两回事，不要在此重复造统计。
// 上层已按"目标根所在卷"做过选根与水位校验，这里只是兜底，避免复制把目标卷打满。
func ensureFreeSpace(dst string, need uint64) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return mapVHDError("mkdir", err)
	}
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return apperr.InvalidParam("path")
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(dirPtr, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return mapVHDError("freespace", err)
	}
	if freeBytesAvailable < need {
		return apperr.New(CodeInsufficientSpace, http.StatusInsufficientStorage).
			WithArg("free_bytes", int64(freeBytesAvailable)).
			WithArg("need_bytes", int64(need))
	}
	return nil
}

// limitText 截断命令输出，避免日志被 robocopy 输出淹没。
func limitText(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

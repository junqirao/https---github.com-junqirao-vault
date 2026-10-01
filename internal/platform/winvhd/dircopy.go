//go:build windows

package winvhd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// CopyTree 把一个目录的内容完整复制到目标目录，并校验文件数与总字节数一致。
//
// dstDir 通常是已格式化并挂载后的卷根（如 `V:\`）。复制使用 robocopy /E，
// 退出码语义与 copy.go 中的单文件复制一致：0~7 视为成功或部分成功，>= 8 才是真失败。
//
// 校验（见 docs/implementation.md 5.10 步骤 ⑤）：目标侧的文件数与总字节数必须与源一致，
// 否则判定复制不完整并返回 platform.copy_failed（原始 robocopy 输出只进日志）。
func (m *Manager) CopyTree(ctx context.Context, srcDir, dstDir string) (files int, bytes int64, err error) {
	if err := ctxErr(ctx); err != nil {
		return 0, 0, err
	}
	if srcDir == "" || dstDir == "" {
		return 0, 0, apperr.InvalidParam("src_dir")
	}
	srcClean := filepath.Clean(srcDir)
	info, err := os.Stat(srcClean)
	if err != nil {
		return 0, 0, mapVHDError("copytree_src", err)
	}
	if !info.IsDir() {
		return 0, 0, apperr.InvalidParam("src_dir")
	}

	srcFiles, srcBytes, err := countTree(srcClean)
	if err != nil {
		return 0, 0, mapVHDError("copytree_src", err)
	}

	if err := m.runRobocopyTree(ctx, srcClean, dstDir); err != nil {
		return 0, 0, err
	}

	dstFiles, dstBytes, err := countTree(dstDir)
	if err != nil {
		return 0, 0, mapVHDError("copytree_dst", err)
	}
	if dstFiles < srcFiles || dstBytes < srcBytes {
		m.logger.Error("目录复制校验失败",
			"src_dir", srcClean, "src_files", srcFiles, "src_bytes", srcBytes,
			"dst_files", dstFiles, "dst_bytes", dstBytes)
		return dstFiles, dstBytes, apperr.New(CodeCopyFailed, http.StatusInternalServerError).
			WithArg("src_files", srcFiles).
			WithArg("dst_files", dstFiles).
			WithArg("src_bytes", srcBytes).
			WithArg("dst_bytes", dstBytes)
	}

	m.logger.Info("已复制目录内容",
		"src_dir", srcClean, "dst_dir", dstDir, "files", dstFiles, "bytes", dstBytes)
	return dstFiles, dstBytes, nil
}

// runRobocopyTree 用 robocopy /E 递归复制整个目录树。
func (m *Manager) runRobocopyTree(ctx context.Context, srcDir, dstDir string) error {
	args := []string{
		srcDir, dstDir, "/E", "/COPY:DAT", "/R:1", "/W:1",
		"/NFL", "/NDL", "/NP", "/NJH", "/NJS",
	}
	cmd := exec.CommandContext(ctx, "robocopy", args...)
	// robocopy 是控制台程序：隐藏窗口，避免在无控制台的父进程下弹出黑框。
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
		m.logger.Error("robocopy 目录复制失败",
			"exit_code", exitCode,
			"output", limitText(string(output)))
		return apperr.New(CodeCopyFailed, http.StatusInternalServerError).WithArg("exit_code", exitCode)
	}
	return nil
}

// countTree 统计目录树下的常规文件数与总字节数。
func countTree(root string) (files int, bytes int64, err error) {
	err = filepath.Walk(root, func(_ string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fi.Mode().IsRegular() {
			files++
			bytes += fi.Size()
		}
		return nil
	})
	return files, bytes, err
}

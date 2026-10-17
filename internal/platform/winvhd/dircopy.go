//go:build windows

package winvhd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"vault/internal/apperr"
	"vault/internal/platform/winps"
)

// copyProgressInterval 是拷贝期间推算进度的采样间隔。
//
// robocopy 的输出被 /NFL /NDL /NP /NJH /NJS 压到只剩总结，本身不打字节进度，所以改用
// "目标卷剩余空间少了多少"当作已拷入字节：目标盘是刚格式化的新盘，除了这次拷入没有任何
// 东西在写它，两个数一一对应。2 秒一次 GetDiskFreeSpaceEx，相对整段拷贝可忽略不计。
const copyProgressInterval = 2 * time.Second

// CopyTree 把一个目录的内容完整复制到目标目录，并校验文件数与总字节数一致。
//
// dstDir 通常是已格式化并挂载后的卷根（如 `V:\`）。复制使用 robocopy /E，
// 退出码语义与 copy.go 中的单文件复制一致：0~7 视为成功或部分成功，>= 8 才是真失败。
//
// 校验（见 docs/implementation.md 5.10 步骤 ⑤）：目标侧的文件数与总字节数不得少于源，
// 否则判定复制不完整并返回 platform.copy_failed（原始 robocopy 输出只进日志）。
// 两侧统计一律跳过卷系统保留目录（见 volumeReservedDirs），它们由系统自建且不可读。
//
// onCopy 用于上报拷贝进度（已写入 / 总字节），可为 nil（见 platform.CopyProgressFunc）。
func (m *Manager) CopyTree(ctx context.Context, srcDir, dstDir string, onCopy func(copied, total int64)) (files int, bytes int64, err error) {
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

	if err := m.runRobocopyTree(ctx, srcClean, dstDir, srcBytes, onCopy); err != nil {
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

// runRobocopyTree 用 robocopy /E 递归复制整个目录树，并在拷贝期间回报字节进度。
func (m *Manager) runRobocopyTree(ctx context.Context, srcDir, dstDir string, srcBytes int64, onCopy func(copied, total int64)) error {
	args := []string{
		srcDir, dstDir, "/E", "/COPY:DAT", "/R:1", "/W:1",
		"/NFL", "/NDL", "/NP", "/NJH", "/NJS",
	}
	cmd := exec.CommandContext(ctx, "robocopy", args...)
	// robocopy 是控制台程序：隐藏窗口，避免在无控制台的父进程下弹出黑框。
	winps.PrepareHiddenCommand(cmd)
	// 不用 CombinedOutput（它一次阻塞到进程结束）：复制要在后台跑，主协程得腾出来采样进度。
	// 输出仍要接住 —— 失败时它是唯一能说明原因的东西（见下面 exitCode >= 8 的日志）。
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		m.logger.Error("robocopy 启动失败", "err", err.Error())
		return apperr.New(CodeCopyFailed, http.StatusInternalServerError).WithCause(err)
	}
	err := m.waitRobocopy(cmd, dstDir, srcBytes, onCopy)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
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
			"output", limitText(output.String()))
		return apperr.New(CodeCopyFailed, http.StatusInternalServerError).WithArg("exit_code", exitCode)
	}
	if exitCode != 0 {
		m.logger.Debug("robocopy 部分成功", "exit_code", exitCode)
	}
	return nil
}

// waitRobocopy 等 robocopy 结束，同时按目标卷剩余空间推算已拷入字节并回报给 onCopy。
//
// 返回的即 cmd.Wait 的错误（语义与原来的 CombinedOutput 一致）。
// 只在"上一次采样之后确实又写进去了"时才回报：30G 的拷贝要跑几十分钟，重复上报同一个
// 百分比只会白白刷一遍任务进度（每报一次就是一条 UPDATE）。
func (m *Manager) waitRobocopy(cmd *exec.Cmd, dstDir string, srcBytes int64, onCopy func(copied, total int64)) error {
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	if onCopy == nil || srcBytes <= 0 {
		return <-waited
	}
	base, ok := freeBytesOf(dstDir)
	if !ok {
		// 读不到目标卷（盘符还没挂上、路径不是卷根等）：宁可没有细粒度进度，也不编一个假的。
		m.logger.Warn("读不到目标卷可用空间，本次拷贝不上报进度", "dst_dir", dstDir)
		return <-waited
	}
	ticker := time.NewTicker(copyProgressInterval)
	defer ticker.Stop()
	var last int64
	for {
		select {
		case err := <-waited:
			return err
		case <-ticker.C:
			free, ok := freeBytesOf(dstDir)
			if !ok || free >= base {
				continue
			}
			copied := int64(base - free)
			if copied > srcBytes {
				copied = srcBytes
			}
			if copied <= last {
				continue
			}
			last = copied
			onCopy(copied, srcBytes)
		}
	}
}

// freeBytesOf 返回 path 所在卷的可用空间（字节）；读不到返回 false。
func freeBytesOf(path string) (uint64, bool) {
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(ptr, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, false
	}
	return freeBytesAvailable, true
}

// volumeReservedDirs 是 Windows 卷根上的系统保留目录（键为小写，NTFS 按大小写不敏感比较）。
//
// 它们由系统在卷格式化后自动创建，ACL 默认只给 SYSTEM 完全控制 —— 连管理员进程列目录
// 都会被拒（ERROR_ACCESS_DENIED）。它们不属于本次复制的内容，统计时必须整棵跳过：
// 否则一次**已经成功**的复制会在校验阶段因为"打不开目标盘的系统目录"被判失败
// （真实事故：本地目录上传建库连续 3 次重试，全部挂在 copytree_dst 的
//  `open G:\System Volume Information: Access is denied`，而上层的
//  dstFiles < srcFiles 容忍度根本没机会生效，因为遍历先报错了）。
var volumeReservedDirs = map[string]bool{
	"system volume information": true, // NTFS 的"系统卷信息"目录
	"$recycle.bin":              true, // 卷根回收站
}

// isReservedDirEntry 报告目录项是否属于卷系统保留目录，需要整棵跳过。
//
// 只看路径末段而不是 fi：目录自身 lstat 也可能失败（此时 fi 为 nil），靠名字判断才能
// 保证一定跳过。非目录一律不算命中 —— 同名**文件**属于用户数据，必须照常统计。
func isReservedDirEntry(path string, fi os.FileInfo) bool {
	if fi != nil && !fi.IsDir() {
		return false
	}
	return volumeReservedDirs[strings.ToLower(filepath.Base(path))]
}

// countTree 统计目录树下的常规文件数与总字节数。
//
// 卷系统保留目录整棵跳过（见 volumeReservedDirs）：它们既不属于复制内容，又默认拒绝
// 访问，走进去只会得到 Access is denied 并把成功的复制误判为失败。
// 其余错误（含源侧真正的读取失败）仍原样返回，不削弱校验。
func countTree(root string) (files int, bytes int64, err error) {
	err = filepath.Walk(root, func(path string, fi os.FileInfo, walkErr error) error {
		if isReservedDirEntry(path, fi) {
			// SkipDir 在 walkErr 非 nil 时同样安全：Walk 对目录上的 SkipDir 一律忽略。
			return filepath.SkipDir
		}
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

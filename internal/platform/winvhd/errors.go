//go:build windows

// Package winvhd 封装 Windows VirtDisk API（通过 github.com/Microsoft/go-winio/vhd 的纯 Go 绑定），
// 提供 VHDX 的创建、挂载、查询、复制、空间回收与磁盘标识重置能力。
//
// 该包是 internal/platform 下唯一必须依赖 Windows 的平台包，因此整体带 `windows` 构建约束。
package winvhd

import (
	"errors"
	"net/http"

	"golang.org/x/sys/windows"

	"vault/internal/apperr"
)

// 平台层错误码。
const (
	// CodeVHDFailed 未归类的 VHDX 操作失败。
	CodeVHDFailed = "platform.vhd_failed"
	// CodeAccessDenied 需要提权的操作被拒绝（ERROR_ACCESS_DENIED）。
	CodeAccessDenied = "platform.access_denied"
	// CodeSharingViolation 文件被占用 / 共享冲突（ERROR_SHARING_VIOLATION）。
	CodeSharingViolation = "platform.sharing_violation"
	// CodeInsufficientSpace 目标卷可用空间不足。
	CodeInsufficientSpace = "platform.insufficient_space"
	// CodeResetDiskIDUnavailable 缺少 Hyper-V 模块，无法重置磁盘标识。
	CodeResetDiskIDUnavailable = "platform.reset_disk_id_unavailable"
	// CodeCopyFailed 复制失败（robocopy 退出码 >= 8）。
	CodeCopyFailed = "platform.copy_failed"
)

// mapVHDError 把 Windows 错误号映射为平台层业务错误。op 仅作为插值参数，便于排查。
func mapVHDError(op string, err error) *apperr.Error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return apperr.New(CodeAccessDenied, http.StatusInternalServerError).
			WithCause(err).WithArg("op", op)
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return apperr.New(CodeSharingViolation, http.StatusConflict).
			WithCause(err).WithArg("op", op)
	default:
		return apperr.New(CodeVHDFailed, http.StatusInternalServerError).
			WithCause(err).WithArg("op", op)
	}
}

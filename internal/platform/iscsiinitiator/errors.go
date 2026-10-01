package iscsiinitiator

import (
	"net/http"

	"vault/internal/apperr"
)

// 平台层错误码（客户端 iSCSI 发起端）。
const (
	// CodeSessionNotEstablished 等待会话建立超时（目标未按预期建立连接）。
	CodeSessionNotEstablished = "platform.iscsi_session_not_established"
	// CodeDeviceInUse 会话上仍有在线设备，无法断开（HRESULT 0xefff0040）。
	CodeDeviceInUse = "platform.iscsi_device_in_use"
)

// ErrDeviceInUse 返回「会话上有在线设备，无法断开」的明确错误。
//
// 触发条件：未先把磁盘 Set-Disk -IsOffline $true 就调用 Disconnect-IscsiTarget。
// 调用方应先下线磁盘再重试（见 docs/implementation.md 5.5 卸载流程）。
func ErrDeviceInUse() *apperr.Error {
	return apperr.New(CodeDeviceInUse, http.StatusConflict)
}

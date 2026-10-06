//go:build linux

package linuxlvm

// 平台层错误码。
//
// 与 winvhd/volume 保持同名同风格（都是 platform.xxx），便于前端复用同一套 i18n 文案；
// 刻意**不** import internal/platform/winvhd —— 那个包带 windows 构建约束，Linux 编不过。
const (
	// CodeInsufficientSpace 目标卷/池可用空间不足（thin pool 水位闸门触发时也用它）。
	CodeInsufficientSpace = "platform.insufficient_space"
	// CodeVHDFailed 未归类的虚拟磁盘（LV）操作失败。
	CodeVHDFailed = "platform.vhd_failed"
	// CodeMountFailed 挂载/卸载/格式化失败。
	CodeMountFailed = "platform.mount_failed"
	// CodeCopyFailed 拷入内容后校验不通过（文件数/字节数少于源）。
	CodeCopyFailed = "platform.copy_failed"
	// CodeAccessDenied 权限不足（读设备节点、挂载等被拒）。
	CodeAccessDenied = "platform.access_denied"
	// CodeCommandFailed 外部命令（LVM/dm 工具）执行失败。
	CodeCommandFailed = "platform.command_failed"
	// CodePoolMissing 配置的卷组或 thin pool 不存在（尚未初始化）。
	//
	// 独立于 CodeCommandFailed：它说的是一件**运维能照做**的事（先去初始化存储池），
	// 而裸命令失败只会把 LVM 的 "Volume group \"vg0\" not found" 甩给用户 ——
	// 真机上就是这样让人对着"创建存储失败"完全无从下手。
	CodePoolMissing = "platform.pool_missing"
)

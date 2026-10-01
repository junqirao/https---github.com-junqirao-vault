//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"vault/internal/platform/winps"
)

// restrictFileToAdmins 把文件的访问控制列表收紧为「仅 SYSTEM 与 Administrators」，并移除继承。
//
// 必要性：本地数据目录默认是 %ProgramData%\Vault，该目录对普通用户可读；而
// identity.json 内含客户端私钥、agent-state.json 内含会话令牌。在 Windows 上
// `os.WriteFile(..., 0o600)` 只写模式位、**不会**改动 ACL，文件会继承父目录的可读权限，
// 于是同一台机器上的任何本地用户都能读到私钥。因此落盘后必须显式收紧权限。
//
// 用 SID 而不是账户名，避免非英文系统上 "SYSTEM"/"Administrators" 名称本地化的问题。
// 客户端本身以管理员权限运行，收紧后仍可正常读写。
func restrictFileToAdmins(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cmd := exec.Command("icacls", abs,
		"/inheritance:r",
		"/grant:r", "*S-1-5-18:F",
		"*S-1-5-32-544:F",
	)
	winps.PrepareHiddenCommand(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("收紧文件权限失败: %w: %s", err, string(out))
	}
	return nil
}

//go:build windows

package winps

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// stubGetDisk 是注入到脚本里的假 Get-Disk：一块本地盘（NVMe，500G）+ 两块**同容量**的
// iSCSI 盘（编号 3、4）——正是"池位盘预创建后同时挂多块盘"的真实形状。
//
// 之所以要注入：本脚本靠 Get-Disk 枚举真实磁盘，而 CI/开发机上根本没有 iSCSI 盘；
// 而这里要验证的是**脚本自己的参数解析与筛选逻辑**（不是 Get-Disk 的能力），
// 因此把它替换成可控的假实现即可（函数优先级高于 cmdlet，脚本内调用会被假实现接管）。
const stubGetDisk = `
function Get-Disk {
    [CmdletBinding()]
    param()
    @(
        [pscustomobject]@{ Number = 0; Size = 500107862016; BusType = 'NVMe'; PartitionStyle = 'GPT'; IsOffline = $false; IsReadOnly = $false; Location = '' }
        [pscustomobject]@{ Number = 3; Size = 1073741824; BusType = 'iSCSI'; PartitionStyle = 'RAW'; IsOffline = $false; IsReadOnly = $false; Location = 'D:\x.vhdx' }
        [pscustomobject]@{ Number = 4; Size = 1073741824; BusType = 'iSCSI'; PartitionStyle = 'GPT'; IsOffline = $false; IsReadOnly = $false; Location = 'D:\y.vhdx' }
    )
}
`

// findIscsiDiskOut 对应脚本的 JSON 输出（ok=false 时 reason 有值）。
type findIscsiDiskOut struct {
	OK             *bool  `json:"ok"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	Number         int    `json:"number"`
	CandidateCount int    `json:"candidate_count"`
}

// runFindIscsiDisk 用真实脚本（注入假 Get-Disk）跑一次，返回脚本的输出。
func runFindIscsiDisk(t *testing.T, usedDiskNumbers string) findIscsiDiskOut {
	t.Helper()

	pwsh, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("未找到 powershell.exe，跳过（本用例验证的是 Windows PowerShell 5.1 的解析行为）")
	}
	srcPath, err := ScriptPath(ScriptVolumeFindIscsiDisk)
	if err != nil {
		t.Fatalf("物化脚本失败: %v", err)
	}
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("读取脚本失败: %v", err)
	}

	// 插桩点：param() 块之后的第一条语句之后（param 必须仍是文件里的第一个语句）。
	const anchor = `try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }`
	if !strings.Contains(string(src), anchor) {
		t.Fatalf("脚本里找不到插桩锚点，请同步更新本测试：%s", anchor)
	}
	// string(src) 会把文件头部的 BOM 保留为 U+FEFF，写回时仍是 UTF-8 with BOM
	// （PowerShell 5.1 缺了 BOM 会按 ANSI 解析中文注释）。
	patched := strings.Replace(string(src), anchor, anchor+"\n"+stubGetDisk, 1)

	probe := filepath.Join(t.TempDir(), "find_iscsi_disk_probe.ps1")
	if err := os.WriteFile(probe, []byte(patched), 0o600); err != nil {
		t.Fatalf("写入探针脚本失败: %v", err)
	}

	cmd := exec.Command(pwsh,
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", probe,
		"-SizeBytes", strconv.Itoa(1073741824),
		"-UsedDiskNumbers", usedDiskNumbers,
		// 关掉缓存刷新：本用例不需要它，也不想在测试里碰需要管理员权限的 cmdlet。
		"-RefreshCache", "false",
	)
	// 脚本找不到磁盘时以 exit 1 收场（这是它的正常协议），因此这里不看 err，只看输出。
	out, _ := cmd.Output()

	// 从后往前取第一行合法 JSON（与 runner.lastEnvelope 同一策略）。
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var parsed findIscsiDiskOut
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}
		return parsed
	}
	t.Fatalf("脚本没有输出合法 JSON，原始输出：\n%s", string(out))
	return findIscsiDiskOut{}
}

// TestFindIscsiDiskScriptParsesUsedDiskNumbers 锁定本脚本最要命的一个坑：
//
// Windows PowerShell 5.1 的 ConvertFrom-Json 把 JSON 数组**整体当成一个对象**输出（不展开），
// 于是 `@(ConvertFrom-Json ... | Where-Object {...})` 拿到的 `$_` 是整个数组，`[int]` 转换直接抛
// "无法将 System.Object[] 转换为 System.Int32" → 脚本以 find_iscsi_disk_failed 收场。
//
// 表现为**本机只要已经挂了一块盘，后续每次挂载都在 find_disk 阶段失败**
// （真实事故：池位盘预创建后连挂第二块盘必然失败）。
func TestFindIscsiDiskScriptParsesUsedDiskNumbers(t *testing.T) {
	t.Run("空已占用列表", func(t *testing.T) {
		got := runFindIscsiDisk(t, "[]")
		if got.OK == nil || !*got.OK {
			t.Fatalf("期望成功，实际 ok=%v reason=%s message=%s", got.OK, got.Reason, got.Message)
		}
		if got.Number != 3 {
			t.Fatalf("空占用列表应选编号最小的候选 3，实际 %d", got.Number)
		}
		if got.CandidateCount != 2 {
			t.Fatalf("候选数应为 2，实际 %d", got.CandidateCount)
		}
	})

	t.Run("单元素已占用列表", func(t *testing.T) {
		got := runFindIscsiDisk(t, "[3]")
		if got.OK == nil || !*got.OK {
			t.Fatalf("UsedDiskNumbers=[3] 时不应失败（这正是曾经的崩溃点），实际 ok=%v reason=%s message=%s",
				got.OK, got.Reason, got.Message)
		}
		if got.Number != 4 {
			t.Fatalf("编号 3 已被占用，应选 4，实际 %d", got.Number)
		}
	})

	t.Run("多元素已占用列表", func(t *testing.T) {
		got := runFindIscsiDisk(t, "[0,3]")
		if got.OK == nil || !*got.OK {
			t.Fatalf("UsedDiskNumbers=[0,3] 时不应失败，实际 ok=%v reason=%s message=%s", got.OK, got.Reason, got.Message)
		}
		if got.Number != 4 {
			t.Fatalf("应选 4，实际 %d", got.Number)
		}
	})

	t.Run("候选全被占用时报告未找到", func(t *testing.T) {
		got := runFindIscsiDisk(t, "[3,4]")
		if got.OK == nil || *got.OK {
			t.Fatalf("两块盘都被占用时不应返回候选，实际 ok=%v number=%d", got.OK, got.Number)
		}
		if got.Reason != "disk_not_found" {
			t.Fatalf("期望 reason=disk_not_found，实际 %q（message=%s）", got.Reason, got.Message)
		}
	})
}

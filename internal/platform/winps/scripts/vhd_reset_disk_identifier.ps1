<#
.SYNOPSIS
    重置 VHDX 的磁盘标识（DiskIdentifier）。

.DESCRIPTION
    直接文件复制得到的 VHDX 会继承源盘相同的磁盘标识，Windows 会报
    Event ID 158「Disk N has the same disk identifiers as one or more disks connected
    to the system」(KB2983588)，并导致 VSS/备份失败，因此复制后必须重置。

    本脚本走 Hyper-V 模块的 Set-VHD -ResetDiskIdentifier（官方确认可用）。
    若目标环境无 Hyper-V 模块，请由 Go 侧根据能力探测结果提前返回
    platform.reset_disk_id_unavailable，让上层决定降级策略（例如提示安装 Hyper-V 管理工具）。

.NOTES
    Set-VHD 要求 VHDX 未被本机挂载、未被 iSCSI 发布 —— 需实测确认具体约束。
#>

param(
    [string]$Path
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Path) { throw 'Path 参数不能为空' }
    if (-not (Test-Path -LiteralPath $Path)) { throw "VHDX 文件不存在：$Path" }

    $command = Get-Command -Name Set-VHD -ErrorAction SilentlyContinue
    if (-not $command) {
        throw 'Hyper-V 模块不可用（缺少 Set-VHD），无法重置磁盘标识'
    }

    Set-VHD -Path $Path -ResetDiskIdentifier -ErrorAction Stop

    [pscustomobject]@{ ok = $true; path = $Path; method = 'Set-VHD' } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'reset_disk_identifier_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

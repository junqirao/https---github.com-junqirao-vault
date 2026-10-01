<#
.SYNOPSIS
    设置磁盘的上线/下线与只读状态（Set-Disk）。

.NOTES
    客户端卸载前的顺序要求：先把盘 Offline，再 Disconnect-IscsiTarget，
    否则会出现 HRESULT 0xefff0040（会话上有在线设备无法登出）。
    Offline / ReadOnly 传入空串表示"该项不改动"。
#>

param(
    [int]$DiskNumber,
    [string]$Offline = '',
    [string]$ReadOnly = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    $disk = Get-Disk -Number $DiskNumber -ErrorAction Stop

    if ($Offline -ne '') {
        Set-Disk -Number $DiskNumber -IsOffline ([System.Boolean]::Parse($Offline)) -ErrorAction Stop
    }
    if ($ReadOnly -ne '') {
        Set-Disk -Number $DiskNumber -IsReadOnly ([System.Boolean]::Parse($ReadOnly)) -ErrorAction Stop
    }

    $disk = Get-Disk -Number $DiskNumber -ErrorAction Stop
    [pscustomobject]@{
        ok           = $true
        number       = [int]$disk.Number
        is_offline   = [bool]$disk.IsOffline
        is_read_only = [bool]$disk.IsReadOnly
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'set_disk_state_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

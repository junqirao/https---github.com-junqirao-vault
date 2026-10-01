<#
.SYNOPSIS
    刷新主机存储缓存（Update-HostStorageCache）。

.NOTES
    挂载 VHDX 后立即 Get-Disk 可能查不到对应磁盘，需要先刷新缓存。
    可选的 VolumePath 参数在当前实现中不使用（参数语义在不同版本上不一致 —— 需实测确认），
    默认只执行无参的 Update-HostStorageCache。
#>

param(
    [string]$VolumePath = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    Update-HostStorageCache -ErrorAction Stop
    [pscustomobject]@{ ok = $true } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'update_host_storage_cache_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

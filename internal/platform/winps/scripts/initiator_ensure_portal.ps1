<#
.SYNOPSIS
    幂等地为目标门户（TargetPortal）添加到本地 iSCSI 发起端。

.NOTES
    由 Go 侧以 `-Address <ip> -Port 3260` 调用，参数不做字符串拼接。
    已存在同一「地址 + 端口」的门户时直接返回 action=exists，不重复添加。

    ⚠️ 需实测确认：New-IscsiTargetPortal / Get-IscsiTargetPortal 的参数名
    （TargetPortalAddress / TargetPortalPortNumber）与返回对象的属性名。
#>

param(
    [string]$Address,
    [int]$Port = 3260
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Address) { throw 'Address 参数不能为空' }
    if ($Port -le 0) { $Port = 3260 }

    $existing = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetPortalAddress -eq $Address -and [int]$_.TargetPortalPortNumber -eq $Port
    })

    if ($existing.Count -gt 0) {
        [pscustomobject]@{
            ok      = $true
            action  = 'exists'
            address = $Address
            port    = $Port
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    New-IscsiTargetPortal -TargetPortalAddress $Address -TargetPortalPortNumber $Port -ErrorAction Stop | Out-Null

    [pscustomobject]@{
        ok      = $true
        action  = 'created'
        address = $Address
        port    = $Port
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'ensure_portal_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

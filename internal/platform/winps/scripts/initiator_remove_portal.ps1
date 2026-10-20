<#
.SYNOPSIS
    移除 iSCSI 目标门户（连同它的「已发现目标」缓存）。幂等。

.NOTES
    由 Go 侧以 `-PortalAddress <ip> -PortalPort 3260` 调用。

    目的：本系统是「用一次删一次」——卸载后不留门户与发现缓存，避免 iSCSI 发起程序里
    越积越多没用的条目（真实反馈："iSCSI 发起程序里一大堆没用的死连接"）。
    门户上仍有活动会话时删除会失败：如实回报，由上层决定（别的挂载还在用同一门户时也不该删）。

    最后一行才是 JSON。
#>

param(
    [string]$PortalAddress,
    [int]$PortalPort = 3260
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# Remove-IscsiTargetPortal 是 ShouldProcess cmdlet：非交互进程里弹确认会把调用卡死。
$ConfirmPreference = 'None'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $PortalAddress) { throw 'PortalAddress 参数不能为空' }
    if ($PortalPort -le 0) { $PortalPort = 3260 }

    $command = Get-Command -Name Remove-IscsiTargetPortal -ErrorAction SilentlyContinue
    if (-not $command) {
        [pscustomobject]@{
            ok      = $true
            action  = 'unsupported'
            message = '当前系统未提供 Remove-IscsiTargetPortal，未清理门户（需实测确认）'
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $portals = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetPortalAddress -eq $PortalAddress -and [int]$_.TargetPortalPortNumber -eq $PortalPort
    })
    if ($portals.Count -eq 0) {
        [pscustomobject]@{
            ok            = $true
            action        = 'none'
            portal        = ($PortalAddress + ':' + $PortalPort)
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $errors = @()
    foreach ($portal in $portals) {
        try {
            Remove-IscsiTargetPortal -TargetPortalAddress $PortalAddress -TargetPortalPortNumber $PortalPort -Confirm:$false -ErrorAction Stop | Out-Null
        } catch {
            $errors += $_.Exception.Message
        }
    }

    # 复核：门户必须真的没了（避免"报成功却没删掉"）。
    $remaining = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetPortalAddress -eq $PortalAddress -and [int]$_.TargetPortalPortNumber -eq $PortalPort
    })
    if ($remaining.Count -gt 0) {
        [pscustomobject]@{
            ok      = $false
            reason  = 'remove_portal_failed'
            message = ('仍有门户未移除：' + ($errors -join ' ; '))
            portal  = ($PortalAddress + ':' + $PortalPort)
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    [pscustomobject]@{
        ok     = $true
        action = 'removed'
        portal = ($PortalAddress + ':' + $PortalPort)
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'remove_portal_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

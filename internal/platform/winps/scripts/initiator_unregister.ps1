<#
.SYNOPSIS
    取消指定 iSCSI 目标的会话持久化（避免机器重启后自动重连）。

.NOTES
    由 Go 侧以 `-TargetIQN <iqn>` 调用。幂等：无会话视为成功（action=none）。

    ⚠️ 需实测确认：Unregister-IscsiSession 的参数名（-SessionIdentifier）与
    Get-IscsiSession 返回对象的 SessionIdentifier 属性名。
#>

param(
    [string]$TargetIQN
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $TargetIQN) { throw 'TargetIQN 参数不能为空' }

    $command = Get-Command -Name Unregister-IscsiSession -ErrorAction SilentlyContinue
    if (-not $command) {
        [pscustomobject]@{
            ok         = $true
            action     = 'unsupported'
            message    = '当前系统未提供 Unregister-IscsiSession，无法取消持久化（需实测确认）'
            target_iqn = $TargetIQN
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetNodeAddress -eq $TargetIQN
    })
    if ($sessions.Count -eq 0) {
        [pscustomobject]@{
            ok         = $true
            action     = 'none'
            target_iqn = $TargetIQN
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $count = 0
    foreach ($session in $sessions) {
        Unregister-IscsiSession -SessionIdentifier $session.SessionIdentifier -ErrorAction Stop | Out-Null
        $count++
    }

    [pscustomobject]@{
        ok            = $true
        action        = 'unregistered'
        target_iqn    = $TargetIQN
        session_count = $count
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'unregister_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

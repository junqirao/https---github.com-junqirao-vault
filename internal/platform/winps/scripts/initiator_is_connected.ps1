<#
.SYNOPSIS
    查询指定 iSCSI 目标是否存在已连接的会话。

.NOTES
    由 Go 侧以 `-TargetIQN <iqn>` 调用。未连接不是错误，返回 ok=true + connected=false。
    ⚠️ 需实测确认：Get-IscsiSession 的 IsConnected / TargetNodeAddress 属性名。
#>

param(
    [string]$TargetIQN
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $TargetIQN) { throw 'TargetIQN 参数不能为空' }

    $connected = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetNodeAddress -eq $TargetIQN -and $_.IsConnected
    })
    $all = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetNodeAddress -eq $TargetIQN
    })

    [pscustomobject]@{
        ok              = $true
        connected       = [bool]($connected.Count -gt 0)
        session_count   = $all.Count
        connected_count = $connected.Count
        target_iqn      = $TargetIQN
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'is_connected_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

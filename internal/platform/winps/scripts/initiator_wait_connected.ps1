<#
.SYNOPSIS
    轮询等待指定目标的 iSCSI 会话建立；超时返回 ok=false + reason=not_connected。

.NOTES
    由 Go 侧以 `-TargetIQN <iqn> -TimeoutSeconds 60` 调用。
    ⚠️ 需实测确认：Get-IscsiSession 的 IsConnected / TargetNodeAddress 属性名。
#>

param(
    [string]$TargetIQN,
    [int]$TimeoutSeconds = 60,
    [int]$PollMilliseconds = 1000
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $TargetIQN) { throw 'TargetIQN 参数不能为空' }
    if ($TimeoutSeconds -le 0) { $TimeoutSeconds = 60 }
    if ($PollMilliseconds -le 0) { $PollMilliseconds = 1000 }

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    $connected = $false
    $count = 0
    while ((Get-Date) -lt $deadline) {
        $sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
            $_.TargetNodeAddress -eq $TargetIQN -and $_.IsConnected
        })
        if ($sessions.Count -gt 0) {
            $connected = $true
            $count = $sessions.Count
            break
        }
        Start-Sleep -Milliseconds $PollMilliseconds
    }

    if (-not $connected) {
        [pscustomobject]@{
            ok              = $false
            reason          = 'not_connected'
            message         = "等待 iSCSI 会话连接超时（$TimeoutSeconds 秒）：$TargetIQN"
            timeout_seconds = $TimeoutSeconds
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    [pscustomobject]@{
        ok            = $true
        connected     = $true
        target_iqn    = $TargetIQN
        session_count = $count
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'wait_connected_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

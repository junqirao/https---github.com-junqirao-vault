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

# ⚠️ 与 initiator_connect.ps1 中的同名函数必须保持一致：平台会改写目标名，
# 会话上的 TargetNodeAddress 与我们下发的名字并不逐字相等，用 -eq 逐字比较会让
# **已经登录成功**的会话被判成"未连接"（挂载失败从 connect 阶段挪到 wait 阶段）。
function Get-TargetShortName {
    param([string]$Iqn)
    $text = [string]$Iqn
    $idx = $text.LastIndexOf(':')
    if ($idx -ge 0 -and $idx -lt ($text.Length - 1)) { return $text.Substring($idx + 1) }
    return $text
}

function Test-SameTarget {
    param([string]$Left, [string]$Right)
    if (-not $Left -or -not $Right) { return $false }
    if ($Left -eq $Right) { return $true }
    $ls = Get-TargetShortName -Iqn $Left
    if ($ls -and $Right.IndexOf($ls, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) { return $true }
    $rs = Get-TargetShortName -Iqn $Right
    if ($rs -and $Left.IndexOf($rs, [System.StringComparison]::OrdinalIgnoreCase) -ge 0) { return $true }
    return $false
}

try {
    if (-not $TargetIQN) { throw 'TargetIQN 参数不能为空' }
    if ($TimeoutSeconds -le 0) { $TimeoutSeconds = 60 }
    if ($PollMilliseconds -le 0) { $PollMilliseconds = 1000 }

    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    $connected = $false
    $count = 0
    while ((Get-Date) -lt $deadline) {
        $sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
            $_.IsConnected -and (Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN)
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

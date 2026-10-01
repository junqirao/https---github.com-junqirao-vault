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

# ⚠️ 与 initiator_connect.ps1 中的同名函数必须保持一致：平台会改写目标名，
# 会话上的 TargetNodeAddress 与我们下发的名字并不逐字相等（详见该脚本 .NOTES）。
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

    $connected = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        $_.IsConnected -and (Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN)
    })
    $all = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN
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

<#
.SYNOPSIS
    断开指定 iSCSI 目标的全部会话。

.NOTES
    由 Go 侧以 `-TargetIQN <iqn>` 调用。幂等：无会话视为成功（action=none）。

    ⚠️ 关键：若会话上仍有「在线（online）设备」，Disconnect-IscsiTarget 会返回 HRESULT
    0xefff0040（session has devices in use）。此时脚本输出 reason=device_in_use，
    调用方必须先 Set-Disk -IsOffline $true 再重试（见 docs/implementation.md 5.5）。

    ⚠️ 需实测确认：Disconnect-IscsiTarget 的参数名与 HRESULT 的暴露方式
    （本脚本同时匹配异常信息里的 0xefff0040 与 HResult 数值 -268505024）。
#>

param(
    [string]$TargetIQN
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 0xEFFF0040 视为有符号 32 位整数时的取值。
$deviceInUseHResult = -268505024

# ⚠️ 与 initiator_connect.ps1 中的同名函数必须保持一致：平台会改写目标名，会话上的
# TargetNodeAddress 与我们下发的名字并不逐字相等，用 -eq 会"看不见"自己的会话 ——
# 卸载时既不报错也不断开（action=none），残留会话会挡住下一次挂载。
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

    $before = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN
    })
    if ($before.Count -eq 0) {
        [pscustomobject]@{
            ok         = $true
            action     = 'none'
            target_iqn = $TargetIQN
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $attempt = 0
    while ($attempt -lt 20) {
        $sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
            Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN
        })
        if ($sessions.Count -eq 0) { break }

        try {
            # 用**会话自己的** TargetNodeAddress 断开：平台改写后的名字与我们下发的
            # 名字不逐字相等，拿下发的名字去 Disconnect 会找不到会话。
            foreach ($session in $sessions) {
                Disconnect-IscsiTarget -NodeAddress ([string]$session.TargetNodeAddress) -ErrorAction Stop | Out-Null
            }
        } catch {
            $message = $_.Exception.Message
            $hresult = 0
            try { $hresult = [int]$_.Exception.HResult } catch { }
            if ($message -match '0xefff0040' -or $hresult -eq $deviceInUseHResult) {
                [pscustomobject]@{
                    ok         = $false
                    reason     = 'device_in_use'
                    message    = $message
                    target_iqn = $TargetIQN
                } | ConvertTo-Json -Compress -Depth 5
                exit 1
            }
            throw
        }
        $attempt++
    }

    $remaining = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN
    })

    [pscustomobject]@{
        ok              = $true
        action          = 'disconnected'
        target_iqn      = $TargetIQN
        remaining_count = $remaining.Count
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'disconnect_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

﻿<#
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
# ⚠️ Disconnect-IscsiTarget 是 ShouldProcess cmdlet。代理是以非交互方式起 PowerShell 的，
# 一旦它想弹确认提示，就会以"Windows PowerShell 处于非交互模式。朗读和提示功能不可用。"
# 失败（真实日志：iSCSI 断开失败 / disconnect_failed，卸载流程因此中断）。
# 这里显式关掉确认提示；下面调用点再补 -Confirm:$false 双保险。
$ConfirmPreference = 'None'
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

    # 断开循环：每轮重新枚举（会话可能在过程中自行消失），逐条尝试断开。
    #
    # ⚠️ 关键：单条会话断开失败**不能中断整个流程**。真实事故：进程中途重启留下一条失效会话，
    # Disconnect-IscsiTarget 对它报 "Invalid Session Id."；旧实现在第一次异常就 throw 出去，
    # 于是后续会话没断成、残留会话挡住下一次 Connect-IscsiTarget
    # （"The target has already been logged in via an iSCSI session."，挂载失败（阶段：connect））。
    # 现在改为：记录错误、继续跑；一轮下来毫无进展就提前退出，交由下面的判定与上层处理。
    $attempt = 0
    $errors = @()
    while ($attempt -lt 20) {
        $sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
            Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN
        })
        if ($sessions.Count -eq 0) { break }

        $progress = $false
        foreach ($session in $sessions) {
            $disconnected = $false
            # 先用**会话自己的** TargetNodeAddress 断开（平台改写后的名字与我们下发的名字不逐字
            # 相等，拿下发的名字去 Disconnect 会找不到会话）；失败再退回按 SessionIdentifier 断。
            $forms = @(
                @{ Label = 'NodeAddress'; Splat = @{ NodeAddress = [string]$session.TargetNodeAddress } },
                @{ Label = 'SessionIdentifier'; Splat = @{ SessionIdentifier = [string]$session.SessionIdentifier } }
            )
            foreach ($form in $forms) {
                $splat = $form.Splat
                try {
                    Disconnect-IscsiTarget @splat -Confirm:$false -ErrorAction Stop | Out-Null
                    $disconnected = $true
                    break
                } catch {
                    $message = $_.Exception.Message
                    $hresult = 0
                    try { $hresult = [int]$_.Exception.HResult } catch { }
                    if ($message -match '0xefff0040' -or $hresult -eq $deviceInUseHResult) {
                        # 会话上仍有在线设备：这是调用方的事（先 Set-Disk -IsOffline），立刻上报，
                        # 让上层补下线后重试（见 docs/implementation.md 5.5）。
                        [pscustomobject]@{
                            ok         = $false
                            reason     = 'device_in_use'
                            message    = $message
                            target_iqn = $TargetIQN
                        } | ConvertTo-Json -Compress -Depth 5
                        exit 1
                    }
                    $errors += ('[' + [string]$form.Label + '] ' + $message)
                }
            }
            if ($disconnected) { $progress = $true }
        }

        # 一轮下来一条都没断掉：再空转 20 次也没意义（会话多处于失效状态），提前退出。
        if (-not $progress) { break }
        $attempt++
    }

    # 判定：会话断开后需要一点时间才从列表里消失，别把"还没走完"当失败 —— 有界重试后仍剩下才算失败。
    $remaining = @()
    for ($check = 0; $check -lt 5; $check++) {
        if ($check -gt 0) { Start-Sleep -Milliseconds 300 }
        $remaining = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
            Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN
        })
        if ($remaining.Count -eq 0) { break }
    }
    if ($remaining.Count -gt 0) {
        $detail = ''
        if ($errors.Count -gt 0) { $detail = '：' + (($errors | Select-Object -Unique) -join ' ; ') }
        [pscustomobject]@{
            ok              = $false
            reason          = 'disconnect_failed'
            message         = ('仍有 ' + $remaining.Count + ' 条会话未能断开' + $detail)
            target_iqn      = $TargetIQN
            remaining_count = $remaining.Count
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    [pscustomobject]@{
        ok         = $true
        action     = 'disconnected'
        target_iqn = $TargetIQN
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'disconnect_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

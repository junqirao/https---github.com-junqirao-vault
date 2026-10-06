<#
.SYNOPSIS
    取消指定 iSCSI 目标的会话持久化（避免机器重启后自动重连）。

.NOTES
    由 Go 侧以 `-TargetIQN <iqn>` 调用。幂等：无会话视为成功（action=none）。

    ⚠️ 只对**持久会话**调用 Unregister-IscsiSession：该 cmdlet 的语义是"删掉持久登录信息"，
    对非持久会话（IsPersistent=$false，即我们挂载时用的 persistent=false）MSiSCSI 会回
    "Failed to remove persistent login information."（真实日志：iSCSI 取消持久化失败 /
    unregister_failed）。那不是故障，是我们问错了对象 —— 直接跳过。
    属性读不到（老系统）时保持原语义，仍尝试取消，避免漏掉真正的持久化。

    ⚠️ 需实测确认：Unregister-IscsiSession 的参数名（-SessionIdentifier）与
    Get-IscsiSession 返回对象的 SessionIdentifier 属性名。
#>

param(
    [string]$TargetIQN
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# Unregister-IscsiSession 同样是 ShouldProcess cmdlet：非交互会话下一旦弹确认就会以
# "Windows PowerShell 处于非交互模式。朗读和提示功能不可用。"失败。
$ConfirmPreference = 'None'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# ⚠️ 与 initiator_connect.ps1 / initiator_disconnect.ps1 中的同名函数必须保持一致：平台会
# 改写目标名，会话上的 TargetNodeAddress 与我们下发的名字并不逐字相等，用 -eq 会"看不见"
# 自己的会话 —— 于是这里返回 none、持久化残留，机器重启后自动重连。
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
        Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN
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
    $skipped = 0
    foreach ($session in $sessions) {
        if ($session.IsPersistent -eq $false) {
            $skipped++
            continue
        }
        Unregister-IscsiSession -SessionIdentifier $session.SessionIdentifier -Confirm:$false -ErrorAction Stop | Out-Null
        $count++
    }

    if ($count -eq 0) {
        # 全是非持久会话：本来就没有持久化要取消，如实回 none，不要伪装成 unregistered。
        [pscustomobject]@{
            ok            = $true
            action        = 'none'
            target_iqn    = $TargetIQN
            session_count = $sessions.Count
            skipped_count = $skipped
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    [pscustomobject]@{
        ok            = $true
        action        = 'unregistered'
        target_iqn    = $TargetIQN
        session_count = $count
        skipped_count = $skipped
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'unregister_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

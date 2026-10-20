<#
.SYNOPSIS
    清理 iSCSI 发起端里属于本系统、但已经没有对应挂载的残留会话与持久目标。

.NOTES
    由 Go 侧以 `-KeepJson '["<iqn>", ...]'` 调用（保留清单 = 当前仍在挂载的目标 IQN）。

    为什么需要：进程中途被杀 / 卸载失败会把会话留在发起端，它会一直重连（界面上就是一堆
    "死连接"），越积越多（真实反馈："iSCSI 发起程序里一大堆没用的死连接，用一次删一次"）。
    本脚本**只动短名以 vault- / vcache- 开头、且不在保留清单里的目标**，绝不碰用户自己配的
    iSCSI 目标或门户。

    断开失败（如仍有在线设备）只跳过、不报错；持久目标（收藏）用 iscsicli RemoveTarget
    尽力清除。最后一行才是 JSON。
#>

param(
    [string]$KeepJson = '[]'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$ConfirmPreference = 'None'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 与 initiator_connect.ps1 / initiator_disconnect.ps1 中的同名函数保持一致：
# 平台会改写目标名，只能用短名做模糊匹配。
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

# 只认本系统的目标命名：短名以 vault-（服务端目标/临时母盘目标）或 vcache-（本机缓存代理目标）开头。
function Test-IsVaultTarget {
    param([string]$Iqn)
    $short = Get-TargetShortName -Iqn $Iqn
    if (-not $short) { return $false }
    $lower = $short.ToLowerInvariant()
    return $lower.StartsWith('vault-') -or $lower.StartsWith('vcache-')
}

try {
    $keep = @()
    if ($KeepJson) {
        $decoded = ConvertFrom-Json -InputObject $KeepJson
        foreach ($item in @($decoded)) {
            if ($item) { $keep += [string]$item }
        }
    }

    $removed = 0
    $skipped = 0
    $sessions = @(Get-IscsiSession -ErrorAction SilentlyContinue)
    foreach ($session in $sessions) {
        $name = [string]$session.TargetNodeAddress
        if (-not (Test-IsVaultTarget -Iqn $name)) { continue }

        $kept = $false
        foreach ($k in $keep) {
            if (Test-SameTarget -Left $name -Right $k) { $kept = $true; break }
        }
        if ($kept) { continue }

        $done = $false
        foreach ($form in @(
                @{ Label = 'NodeAddress'; Splat = @{ NodeAddress = $name } },
                @{ Label = 'SessionIdentifier'; Splat = @{ SessionIdentifier = [string]$session.SessionIdentifier } })) {
            $splat = $form.Splat
            try {
                Disconnect-IscsiTarget @splat -Confirm:$false -ErrorAction Stop | Out-Null
                $done = $true
                break
            } catch { }
        }
        if ($done) { $removed++ } else { $skipped++ }
    }

    # 门户清理：删掉**没有任何会话在用**的门户 —— 门户是「已发现目标」缓存的容器，
    # 不删它，Get-IscsiTarget 里就会一直堆着 IsConnected=False 的历史目标；Windows 枚举
    # iSCSI 设备时因此变慢（真实反馈：没开客户端时打开资源管理器要很久）。
    #
    # 只删"没有会话"的门户：仍在挂载的目标其会话会保留门户。若读不出会话所属门户
    # （老系统属性缺失），保守起见整段跳过，避免误删正在使用的门户。
    $portalsRemoved = 0
    $liveSessions = @(Get-IscsiSession -ErrorAction SilentlyContinue)
    $activePortals = @{}
    $mappedSessions = 0
    foreach ($session in $liveSessions) {
        $addr = ''
        try { $addr = [string]$session.TargetPortalAddress } catch { }
        if ($addr) {
            $port = 0
            try { $port = [int]$session.TargetPortalPortNumber } catch { }
            $activePortals[($addr.ToLowerInvariant() + ':' + $port)] = $true
            $mappedSessions++
        }
    }
    if ($mappedSessions -eq $liveSessions.Count -and (Get-Command -Name Get-IscsiTargetPortal -ErrorAction SilentlyContinue)) {
        foreach ($portal in @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue)) {
            $addr = ''
            try { $addr = [string]$portal.TargetPortalAddress } catch { }
            if (-not $addr) { continue }
            $port = 0
            try { $port = [int]$portal.TargetPortalPortNumber } catch { }
            $key = $addr.ToLowerInvariant() + ':' + $port
            if ($activePortals.ContainsKey($key)) { continue }
            try {
                Remove-IscsiTargetPortal -TargetPortalAddress $addr -TargetPortalPortNumber $port -Confirm:$false -ErrorAction Stop | Out-Null
                $portalsRemoved++
            } catch { }
        }
    }

    # 持久目标（收藏）：本系统不使用，但历史版本可能留下，尽力清掉本系统的那些。
    $persistentRemoved = 0
    if (Get-Command -Name iscsicli -ErrorAction SilentlyContinue) {
        try {
            $listOutput = & iscsicli ListPersistentTargets 2>$null | Out-String
            foreach ($line in ($listOutput -split "`r?`n")) {
                $trimmed = $line.Trim()
                if (-not $trimmed) { continue }
                # 持久目标行里会带完整目标名（iqn...）；只取本系统的那些。
                $match = [regex]::Match($trimmed, 'iqn\.[^\s]+')
                if (-not $match.Success) { continue }
                $iqn = $match.Value
                if (-not (Test-IsVaultTarget -Iqn $iqn)) { continue }
                $kept = $false
                foreach ($k in $keep) { if (Test-SameTarget -Left $iqn -Right $k) { $kept = $true; break } }
                if ($kept) { continue }
                try {
                    & iscsicli RemoveTarget $iqn 2>$null | Out-Null
                    $persistentRemoved++
                } catch { }
            }
        } catch { }
    }

    [pscustomobject]@{
        ok                 = $true
        action             = 'pruned'
        removed            = $removed
        skipped            = $skipped
        persistent_removed = $persistentRemoved
        portals_removed    = $portalsRemoved
        keep_count         = $keep.Count
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'prune_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

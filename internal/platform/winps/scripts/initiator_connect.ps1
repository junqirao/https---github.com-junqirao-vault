<#
.SYNOPSIS
    连接 iSCSI 目标（支持单向 CHAP），可选持久化（重启后自动重连）。

.NOTES
    由 Go 侧以 `-TargetIQN <iqn> -PortalAddress <ip> -PortalPort 3260 -AuthMode none|chap`
    调用；CHAP 密钥只作为参数使用，绝不写入输出或日志。

    ⚠️⚠️ 本脚本必须自己刷新「已发现目标」列表（Update-IscsiTarget，见 TrySyncDiscovery），
    请勿删除该调用。真实事故（挂载失败（阶段：connect），原始报错
    "connect_failed: The target name is not found or is marked as hidden from login"）：
      Connect-IscsiTarget 的 -NodeAddress 只能连**发起端已经发现**的目标，而「已发现目标」
      列表只在 New-IscsiTargetPortal 那一刻由 SendTargets 填充过一次。本系统是
      「1 分配 = 1 target」，每次挂载都是**全新 IQN**；调用方在门户已存在时直接跳过门户创建
      → 从不重新发现 → 新目标永远不在列表里 → 登录必报 target name is not found。
      此时服务端完全正常（enabled=true、initiators=IQN:*、mapped_devices=1，门户 TCP 可达、
      CHAP 也正确），极易被误判成服务端故障（真实工单：反复排查 20+ 次都卡在这里）。
      设计文档 7.2 本就要求客户端具备 "New-IscsiTargetPortal / Update-IscsiTarget" 两项能力。

    ⚠️ 登录用**发现列表里的名字**，不用逐字拼出来的名字：平台会把我们下发的 TargetName
    改写成 TargetIqn（iqn.1991-05.com.microsoft:<主机名>-<我方 IQN>-target），形态或大小写
    一旦不一致，登录同样报 not found。以发现列表为准可同时兼容两种形态。

    ⚠️ 需实测确认（本机无 iSCSI 环境，以下均需在真机验证）：
      - Connect-IscsiTarget 的 -AuthenticationType 取值（NONE / ONEWAYCHAP / MUTUALCHAP）；
      - -IsPersistent 是开关（SwitchParameter）还是布尔值；
      - -ChapSecret 的参数类型（SecureString 或 string）；
      - 门户相关参数名（TargetPortalAddress / TargetPortalPortNumber）；
      - Get-IscsiTarget 的 NodeAddress 属性名（已发现目标列表）；
      - Update-IscsiTarget 的刷新形态（三种形态自动逐个尝试，见 TrySyncDiscovery）。
    本脚本按运行时探测到的参数类型自适应，与 iscsi_set_target.ps1 的处理方式保持一致。
#>

param(
    [string]$TargetIQN,
    [string]$PortalAddress,
    [int]$PortalPort = 3260,
    [string]$AuthMode = 'none',
    [string]$ChapUser = '',
    [string]$ChapSecret = '',
    [string]$Persistent = 'true'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# ───────────────────────────── 目标名比较与发现列表 ─────────────────────────────
# ⚠️ 下面四个函数与 initiator_wait_connected.ps1 / initiator_is_connected.ps1 /
# initiator_disconnect.ps1 中的同名函数必须保持一致（脚本各自独立进程，无法共享代码）。
# 平台会改写目标名，形态随平台/版本而变，因此不能用逐字相等判断"是不是同一个目标"。

# Get-TargetShortName 取 IQN 冒号之后的部分；本系统该值为 vault-<repo>-<alloc>，
# 逐分配唯一，不会误配到别的盘。
function Get-TargetShortName {
    param([string]$Iqn)
    $text = [string]$Iqn
    $idx = $text.LastIndexOf(':')
    if ($idx -ge 0 -and $idx -lt ($text.Length - 1)) { return $text.Substring($idx + 1) }
    return $text
}

# Test-SameTarget：相等（-eq 对字符串即大小写不敏感），或一方的短名出现在另一方名字里
# （覆盖"被平台套上自己的命名权"的改写形态）。
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

# Get-DiscoveredTargets 读取发起端「已发现目标」的名字列表（Get-IscsiTarget）。
function Get-DiscoveredTargets {
    try {
        return @(Get-IscsiTarget -ErrorAction SilentlyContinue |
            ForEach-Object { [string]$_.NodeAddress } | Where-Object { $_ })
    } catch {
        return @()
    }
}

# TrySyncDiscovery 刷新「已发现目标」列表（SendTargets）—— 挂载失败的真凶就在这一步缺失。
# 三种调用形态按顺序尝试（各 Windows 版本参数不同），全失败也不抛错：
# 调用方回读列表，并在连接失败时把真因写进错误信息。
function TrySyncDiscovery {
    param([string]$Address)
    $errors = @()
    $forms = @(
        @{ Label = 'bare'; Splat = @{} },
        @{ Label = 'NodeAddress'; Splat = @{ NodeAddress = $Address } },
        @{ Label = 'TargetPortalAddress'; Splat = @{ TargetPortalAddress = $Address } }
    )
    foreach ($form in $forms) {
        try {
            $splat = $form.Splat
            Update-IscsiTarget @splat -ErrorAction Stop | Out-Null
            return [pscustomobject]@{ ok = $true; form = [string]$form.Label; errors = $errors }
        } catch {
            $errors += ('[' + [string]$form.Label + '] ' + $_.Exception.Message)
        }
    }
    return [pscustomobject]@{ ok = $false; form = ''; errors = $errors }
}

# 先声明，保证异常发生在流程早期时 catch 里也有完整信息。
$discovered = @()
$discovery = 'skipped'
$discoveryErrors = @()
$loginIQN = $TargetIQN
$canEnumerateTargets = $false

try {
    if (-not $TargetIQN) { throw 'TargetIQN 参数不能为空' }
    if (-not $PortalAddress) { throw 'PortalAddress 参数不能为空' }
    if ($PortalPort -le 0) { $PortalPort = 3260 }

    $mode = ($AuthMode).ToLowerInvariant()
    if ($mode -ne 'none' -and $mode -ne 'chap') { throw "不支持的 AuthMode：$AuthMode" }
    if ($mode -eq 'chap' -and -not ($ChapUser -and $ChapSecret)) {
        throw 'AuthMode=chap 时必须同时提供 ChapUser 与 ChapSecret'
    }

    # 幂等：已存在连接中的会话时直接返回，避免重复 Connect。
    # ⚠️ 必须用 Test-SameTarget：平台改写后的会话名与我们下发的名字不逐字相等。
    $connected = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        $_.IsConnected -and (Test-SameTarget -Left ([string]$_.TargetNodeAddress) -Right $TargetIQN)
    })
    if ($connected.Count -gt 0) {
        [pscustomobject]@{
            ok                 = $true
            action             = 'already_connected'
            target_iqn         = $TargetIQN
            login_iqn          = [string]$connected[0].TargetNodeAddress
            session_count      = $connected.Count
            persistent         = $false
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    # 门户不存在则先添加（需实测确认参数名）。
    $portal = @(Get-IscsiTargetPortal -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetPortalAddress -eq $PortalAddress -and [int]$_.TargetPortalPortNumber -eq $PortalPort
    })
    if ($portal.Count -eq 0) {
        New-IscsiTargetPortal -TargetPortalAddress $PortalAddress -TargetPortalPortNumber $PortalPort -ErrorAction Stop | Out-Null
    }

    # ── 关键一步：确保目标出现在「已发现」列表里（文件头 .NOTES 有完整事故说明）。 ──
    $canEnumerateTargets = [bool](Get-Command -Name Get-IscsiTarget -ErrorAction SilentlyContinue)
    $discovered = @(Get-DiscoveredTargets)
    $matched = @($discovered | Where-Object { Test-SameTarget -Left $_ -Right $TargetIQN })
    if ($matched.Count -gt 0) {
        $discovery = 'cached'
    } else {
        $sync = TrySyncDiscovery -Address $PortalAddress
        $discovery = if ($sync.ok) { 'refreshed:' + [string]$sync.form } else { 'refresh_failed' }
        $discoveryErrors = @($sync.errors)
        $discovered = @(Get-DiscoveredTargets)
        $matched = @($discovered | Where-Object { Test-SameTarget -Left $_ -Right $TargetIQN })
    }
    if ($matched.Count -gt 0) {
        # 用平台实际发现到的名字登录：大小写 / 改写形态都以它为准。
        $loginIQN = [string]$matched[0]
    }

    $command = Get-Command -Name Connect-IscsiTarget -ErrorAction Stop
    $splat = @{
        NodeAddress             = $loginIQN
        TargetPortalAddress     = $PortalAddress
        TargetPortalPortNumber  = $PortalPort
        ErrorAction             = 'Stop'
    }
    if ($mode -eq 'chap') {
        $splat['AuthenticationType'] = 'ONEWAYCHAP'
    } else {
        $splat['AuthenticationType'] = 'NONE'
    }

    $persistRequested = ($Persistent -eq 'true')
    if ($persistRequested) {
        if ($command.Parameters.ContainsKey('IsPersistent')) {
            $splat['IsPersistent'] = $true
        } else {
            throw 'Connect-IscsiTarget 不支持 -IsPersistent（需实测确认）'
        }
    }

    if ($mode -eq 'chap') {
        $splat['ChapUsername'] = $ChapUser
        # 需实测确认：-ChapSecret 的参数类型
        $secretParam = $command.Parameters['ChapSecret']
        if ($secretParam -and $secretParam.ParameterType -eq [string]) {
            $splat['ChapSecret'] = $ChapSecret
        } else {
            $splat['ChapSecret'] = ConvertTo-SecureString -String $ChapSecret -AsPlainText -Force
        }
    }

    $result = Connect-IscsiTarget @splat

    $sessionIdentifier = ''
    if ($result) { $sessionIdentifier = [string]$result.SessionIdentifier }

    [pscustomobject]@{
        ok                 = $true
        action             = 'connected'
        target_iqn         = $TargetIQN
        login_iqn          = $loginIQN
        discovery          = $discovery
        discovered_count   = $discovered.Count
        session_identifier = $sessionIdentifier
        persistent         = $persistRequested
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    # 注意：绝不回显 ChapSecret。
    # 把"发起端到底发现了哪些目标"一并带出去 —— 这条信息是
    # "target name is not found" 的唯一有效判据（服务端日志此时看起来一切正常）。
    if (-not $canEnumerateTargets) {
        $hint = '发起端不支持 Get-IscsiTarget，无法列出已发现目标'
    } elseif ($discovered.Count -gt 0) {
        $hint = '发起端已发现的目标: ' + ($discovered -join ' , ')
    } else {
        $hint = '发起端已发现的目标: (空) —— 服务端目标未启用/未授权该发起程序，或门户未刷新'
    }
    if ($discoveryErrors.Count -gt 0) {
        $hint = $hint + ' ｜ 刷新发现列表失败: ' + ($discoveryErrors -join ' ; ')
    }
    [pscustomobject]@{
        ok         = $false
        reason     = 'connect_failed'
        target_iqn = $TargetIQN
        login_iqn  = $loginIQN
        discovery  = $discovery
        discovered = $discovered
        message    = ($_.Exception.Message + ' ｜ ' + $hint)
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

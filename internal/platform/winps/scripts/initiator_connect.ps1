<#
.SYNOPSIS
    连接 iSCSI 目标（支持单向 CHAP），可选持久化（重启后自动重连）。

.NOTES
    由 Go 侧以 `-TargetIQN <iqn> -PortalAddress <ip> -PortalPort 3260 -AuthMode none|chap`
    调用；CHAP 密钥只作为参数使用，绝不写入输出或日志。

    ⚠️ 需实测确认（本机无 iSCSI 环境，以下均需在真机验证）：
      - Connect-IscsiTarget 的 -AuthenticationType 取值（NONE / ONEWAYCHAP / MUTUALCHAP）；
      - -IsPersistent 是开关（SwitchParameter）还是布尔值；
      - -ChapSecret 的参数类型（SecureString 或 string）；
      - 门户相关参数名（TargetPortalAddress / TargetPortalPortNumber）。
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
    $connected = @(Get-IscsiSession -ErrorAction SilentlyContinue | Where-Object {
        $_.TargetNodeAddress -eq $TargetIQN -and $_.IsConnected
    })
    if ($connected.Count -gt 0) {
        [pscustomobject]@{
            ok                 = $true
            action             = 'already_connected'
            target_iqn         = $TargetIQN
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

    $command = Get-Command -Name Connect-IscsiTarget -ErrorAction Stop
    $splat = @{
        NodeAddress             = $TargetIQN
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
        session_identifier = $sessionIdentifier
        persistent         = $persistRequested
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    # 注意：绝不回显 ChapSecret。
    [pscustomobject]@{
        ok      = $false
        reason  = 'connect_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

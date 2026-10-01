<#
.SYNOPSIS
    查询 iSCSI 目标（可指定 TargetName，返回 JSON 数组）。

.NOTES
    ⚠️ 两处实测结论（Windows Server，IscsiTarget 模块 2.0.0.0）：

    1) 目标对象**没有 Enabled 属性**（属性清单里只有 Status 等）。启用状态必须读 Status：
       启用后 = 1（名称 NotConnected，尚无会话）、显式 -Enable $false 后 = 3。
       故这里按 Status 的名称判定（Disabled 类名称视为停用），并回报 status 原值便于核对。

    2) 映射关系**不在虚拟盘侧**：Get-IscsiVirtualDisk 没有 TargetName 属性
       （只有 Path/Description/Status/...），侧推必然为空；映射实际挂在目标侧 ——
       LunMappings 是 { TargetName, Path, Lun } 的集合。因此 mapped_devices 从目标读取。

    initiator_ids 统一输出为 "类型:值"（如 IQN:iqn.xxx / IPAddress:1.2.3.4），与上层下发
    时使用的格式保持一致；若原样输出对象数组，上层按字符串列表解析会失败。
#>

param(
    [string]$TargetName = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    Import-Module -Name IscsiTarget -ErrorAction Stop

    $targets = @()
    if ($TargetName) {
        $targets = @(Get-IscsiServerTarget -TargetName $TargetName -ErrorAction SilentlyContinue)
    } else {
        $targets = @(Get-IscsiServerTarget -ErrorAction SilentlyContinue)
    }

    $items = @()
    foreach ($target in $targets) {
        $name = [string]$target.TargetName

        # 授权列表：归一为 "类型:值" 字符串（Method 是枚举，[string] 转换得到名称）。
        #
        # 名称再按上层（domain.InitiatorIDType）的拼写归一：枚举名与业务常量必须逐字一致，
        # 否则上层拿回来做"已授权列表"比较时会因大小写/拼写差异误判。
        $canonicalTypes = @{
            'iqn'         = 'IQN'
            'ipaddress'   = 'IPAddress'
            'ipv6address' = 'IPv6Address'
            'dnsname'     = 'DNSName'
            'macaddress'  = 'MACAddress'
        }
        $initiatorIds = @()
        foreach ($id in @($target.InitiatorIds)) {
            if ($null -eq $id) { continue }
            $method = ''
            try { $method = [string]$id.Method } catch { }
            $value = ''
            try { $value = [string]$id.Value } catch { }
            if ($method) {
                $key = $method.ToLowerInvariant()
                if ($canonicalTypes.ContainsKey($key)) { $method = $canonicalTypes[$key] }
                $initiatorIds += ($method + ':' + $value)
            } else {
                $initiatorIds += $value
            }
        }

        # 映射设备：从目标侧的 LunMappings 读取（虚拟盘侧没有 TargetName 属性）。
        $devices = @()
        foreach ($mapping in @($target.LunMappings)) {
            if ($null -eq $mapping) { continue }
            $path = ''
            try { $path = [string]$mapping.Path } catch { }
            if ($path) { $devices += $path }
        }

        # 启用状态：按**实际存在**的属性判定，并回报依据。
        #
        # ⚠️ 实测：目标对象没有 Enabled 属性（`Select-Object TargetName,Enabled` 得到
        # Enabled=null），此时 [bool]$target.Enabled 恒为 $false —— 上层 reconcile 会永远
        # 认为"目标未启用"而每轮重复下发启用命令。实际可用的只有 Status：
        # 启用后 = 1（名称 NotConnected）、显式停用后 = 3，故按名称判定：
        # Disabled / Stopped / Offline / Inactive 视为停用，其余视为启用。
        $enabled = $false
        $enabledSource = 'absent'
        $status = ''
        $statusValue = ''
        $enabledProperty = $target.PSObject.Properties['Enabled']
        if ($enabledProperty -and $null -ne $enabledProperty.Value) {
            $enabled = [bool]$enabledProperty.Value
            $enabledSource = 'Enabled'
        } elseif ($target.PSObject.Properties['Status']) {
            $status = [string]$target.Status
            try { $statusValue = [string][int]$target.Status } catch { $statusValue = '' }
            $enabled = -not ($status -match '^(Disabled|Stopped|Offline|Inactive)$')
            $enabledSource = 'Status'
        }

        $items += [pscustomobject]@{
            name           = $name
            enabled        = $enabled
            enabled_source = $enabledSource
            status         = $status
            status_value   = $statusValue
            initiator_ids  = $initiatorIds
            mapped_devices = $devices
        }
    }

    [pscustomobject]@{
        ok      = $true
        count   = @($items).Count
        targets = @($items)
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'get_targets_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

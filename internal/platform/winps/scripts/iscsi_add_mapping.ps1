<#
.SYNOPSIS
    建立「iSCSI 虚拟盘 ↔ 目标」映射（Add-IscsiVirtualDiskTargetMapping）。

.NOTES
    ⚠️ 需实测确认：`Add-IscsiVirtualDiskTargetMapping` 的参数名在不同 Windows 版本上存在差异
    （有版本用 -DevicePath，有版本用 -Path）。本脚本先取 `Get-Command` 的实际参数集再决定调用形式。

    幂等判断**必须从目标侧看**：实测虚拟盘对象没有 TargetName 属性
    （Get-IscsiVirtualDisk 只有 Path/Description/Status/...），原先"从虚拟盘侧推"的写法
    永远判为"未映射"，于是每次发布都会对同一块盘重复 Add 同一映射（在部分版本上会直接失败，
    表现为 add_mapping_failed；这正是"重复挂载/重新发布"失败的根因之一）。
    正确来源是 Get-IscsiServerTarget 的 LunMappings（{ TargetName, Path, Lun }）。
    添加后再复核一次，避免"报成功却没真正映射"。
#>

param(
    [string]$TargetName,
    [string]$DevicePath
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $TargetName) { throw 'TargetName 参数不能为空' }
    if (-not $DevicePath) { throw 'DevicePath 参数不能为空' }
    Import-Module -Name IscsiTarget -ErrorAction Stop

    $command = Get-Command -Name Add-IscsiVirtualDiskTargetMapping -ErrorAction Stop
    $parameterNames = @($command.Parameters.Keys)

    # 映射是否存在：只认目标侧的 LunMappings（虚拟盘对象没有 TargetName 属性）。
    function Test-MappingExists {
        param([string]$Name, [string]$Path)
        foreach ($target in @(Get-IscsiServerTarget -ErrorAction SilentlyContinue)) {
            if ([string]$target.TargetName -ne $Name) { continue }
            foreach ($mapping in @($target.LunMappings)) {
                if ($null -eq $mapping) { continue }
                $mappingPath = ''
                try { $mappingPath = [string]$mapping.Path } catch { }
                if ($mappingPath -and $mappingPath -eq $Path) { return $true }
            }
        }
        return $false
    }

    # 幂等：该虚拟盘已映射到同一目标则跳过
    if (Test-MappingExists -Name $TargetName -Path $DevicePath) {
        [pscustomobject]@{
            ok          = $true
            action      = 'skipped'
            target_name = $TargetName
            device_path = $DevicePath
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    if ($parameterNames -notcontains 'TargetName') {
        throw 'Add-IscsiVirtualDiskTargetMapping 缺少 -TargetName 参数（需实测确认参数集）'
    }

    if ($parameterNames -contains 'DevicePath') {
        Add-IscsiVirtualDiskTargetMapping -TargetName $TargetName -DevicePath $DevicePath -ErrorAction Stop
    } elseif ($parameterNames -contains 'Path') {
        Add-IscsiVirtualDiskTargetMapping -TargetName $TargetName -Path $DevicePath -ErrorAction Stop
    } else {
        throw 'Add-IscsiVirtualDiskTargetMapping 缺少 -DevicePath / -Path 参数（需实测确认参数集）'
    }

    # 复核：映射必须真的建立，否则明确报失败（避免"报成功却没映射"）。
    if (-not (Test-MappingExists -Name $TargetName -Path $DevicePath)) {
        throw ("Add-IscsiVirtualDiskTargetMapping 执行后仍未在目标 $TargetName 上看到 " +
            "$DevicePath 的映射（LunMappings 未更新）")
    }

    [pscustomobject]@{
        ok             = $true
        action         = 'added'
        verified_added = $true
        target_name    = $TargetName
        device_path    = $DevicePath
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'add_mapping_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

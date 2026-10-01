<#
.SYNOPSIS
    解除「iSCSI 虚拟盘 ↔ 目标」映射（Remove-IscsiVirtualDiskTargetMapping）。

.NOTES
    ⚠️ 需实测确认：与 Add-* 同理，-DevicePath / -Path 参数名在不同版本上可能存在差异。

    映射是否存在**必须从目标侧判断**：实测虚拟盘对象没有 TargetName 属性
    （Get-IscsiVirtualDisk 只有 Path/Description/Status/...），原先"从虚拟盘侧推"的
    写法永远得到"未映射"，于是本脚本每次都直接返回 not_found、**从不真正解除映射**，
    留下脏映射直到目标被删除。正确来源是 Get-IscsiServerTarget 的 LunMappings
    （{ TargetName, Path, Lun }）。删除后再从目标侧复核一次，避免"报成功但没删掉"。

    幂等：未映射视为成功（保证清理流程可重复执行）。
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

    $command = Get-Command -Name Remove-IscsiVirtualDiskTargetMapping -ErrorAction Stop
    $parameterNames = @($command.Parameters.Keys)

    # 映射是否存在：只认目标侧的 LunMappings（虚拟盘侧没有 TargetName 属性）。
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

    # 幂等：未映射则直接成功返回
    if (-not (Test-MappingExists -Name $TargetName -Path $DevicePath)) {
        [pscustomobject]@{
            ok          = $true
            action      = 'not_found'
            target_name = $TargetName
            device_path = $DevicePath
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    if ($parameterNames -notcontains 'TargetName') {
        throw 'Remove-IscsiVirtualDiskTargetMapping 缺少 -TargetName 参数（需实测确认参数集）'
    }

    # 取该映射的 Lun（cmdlet 支持 -Lun 时一并传入，避免"参数集不完整"类失败）。
    $lun = $null
    foreach ($target in @(Get-IscsiServerTarget -ErrorAction SilentlyContinue)) {
        if ([string]$target.TargetName -ne $TargetName) { continue }
        foreach ($mapping in @($target.LunMappings)) {
            if ($null -eq $mapping) { continue }
            $mappingPath = ''
            try { $mappingPath = [string]$mapping.Path } catch { }
            if ($mappingPath -and $mappingPath -eq $DevicePath) {
                try { $lun = [int]$mapping.Lun } catch { $lun = $null }
                break
            }
        }
        if ($null -ne $lun) { break }
    }

    $removeArgs = @{ TargetName = $TargetName }
    if ($parameterNames -contains 'DevicePath') {
        $removeArgs['DevicePath'] = $DevicePath
    } elseif ($parameterNames -contains 'Path') {
        $removeArgs['Path'] = $DevicePath
    } else {
        throw 'Remove-IscsiVirtualDiskTargetMapping 缺少 -DevicePath / -Path 参数（需实测确认参数集）'
    }
    if ($null -ne $lun -and $parameterNames -contains 'Lun') { $removeArgs['Lun'] = $lun }

    Remove-IscsiVirtualDiskTargetMapping @removeArgs -ErrorAction Stop

    # 复核：映射必须真的消失，否则明确报失败（避免"报成功但没删掉"）。
    if (Test-MappingExists -Name $TargetName -Path $DevicePath) {
        throw ("Remove-IscsiVirtualDiskTargetMapping 执行后映射仍然存在（目标 $TargetName，" +
            "设备 $DevicePath）")
    }

    [pscustomobject]@{
        ok               = $true
        action           = 'removed'
        verified_removed = $true
        target_name      = $TargetName
        device_path      = $DevicePath
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'remove_mapping_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

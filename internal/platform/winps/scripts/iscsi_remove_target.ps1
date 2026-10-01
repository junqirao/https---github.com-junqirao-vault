<#
.SYNOPSIS
    删除 iSCSI 目标（Remove-IscsiServerTarget）。

.NOTES
    幂等：目标不存在视为成功。
    删除目标前先解除它名下的全部「虚拟盘 ↔ 目标」映射，避免留下孤儿映射。
    返回 unmapped_devices 记录本次解除了哪些映射。

    ⚠️ 实测结论（Windows Server，IscsiTarget 2.0.0.0）：
      - 映射就在**目标对象自己**的 LunMappings 上（{ TargetName, Path, Lun }），
        不需要任何侧推；原实现从 Get-IscsiVirtualDisk.TargetName 侧推，而该属性
        在本版本不存在，导致"先解映射"这段实际是空转；
      - Remove-IscsiVirtualDiskTargetMapping 的参数是 **-Path**（没有 -DevicePath），
        原实现用 -DevicePath + -ErrorAction SilentlyContinue，报错被整段吞掉。
      本脚本按运行时参数集自适应，参数缺失时明确报错而非静默空转。
#>

param(
    [string]$TargetName
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $TargetName) { throw 'TargetName 参数不能为空' }
    Import-Module -Name IscsiTarget -ErrorAction Stop

    $target = Get-IscsiServerTarget -TargetName $TargetName -ErrorAction SilentlyContinue
    if (-not $target) {
        [pscustomobject]@{ ok = $true; action = 'not_found'; target_name = $TargetName } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    # 先解除本目标名下的全部映射：直接读目标对象自己的 LunMappings。
    $removeCommand = Get-Command -Name Remove-IscsiVirtualDiskTargetMapping -ErrorAction SilentlyContinue
    $pathParameter = ''
    $lunParameter = $false
    if ($removeCommand) {
        $parameterNames = @($removeCommand.Parameters.Keys)
        if ($parameterNames -contains 'Path') { $pathParameter = 'Path' }
        elseif ($parameterNames -contains 'DevicePath') { $pathParameter = 'DevicePath' }
        $lunParameter = $parameterNames -contains 'Lun'
    }

    $unmappedDevices = @()
    foreach ($mapping in @($target.LunMappings)) {
        if ($null -eq $mapping) { continue }
        $mappingPath = ''
        try { $mappingPath = [string]$mapping.Path } catch { }
        if (-not $mappingPath) { continue }
        if (-not $pathParameter) {
            throw 'Remove-IscsiVirtualDiskTargetMapping 缺少 -Path / -DevicePath 参数（需实测确认参数集）'
        }
        $removeArgs = @{ TargetName = $TargetName }
        $removeArgs[$pathParameter] = $mappingPath
        if ($lunParameter) {
            $lun = $null
            try { $lun = [int]$mapping.Lun } catch { $lun = $null }
            if ($null -ne $lun) { $removeArgs['Lun'] = $lun }
        }
        Remove-IscsiVirtualDiskTargetMapping @removeArgs -ErrorAction Stop
        $unmappedDevices += $mappingPath
    }

    Remove-IscsiServerTarget -TargetName $TargetName -ErrorAction Stop

    [pscustomobject]@{
        ok               = $true
        action           = 'removed'
        target_name      = $TargetName
        unmapped_devices = $unmappedDevices
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'remove_target_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

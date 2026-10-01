<#
.SYNOPSIS
    移除 iSCSI 虚拟盘登记（Remove-IscsiVirtualDisk）。

.NOTES
    幂等：未登记视为成功。
    注意：本脚本**只删登记对象，不删除 .vhdx 文件**；文件需由上层另行删除。
    删除前会先解除该虚拟盘上的全部映射，否则 Remove-IscsiVirtualDisk 可能失败。
    返回 unmapped_targets 记录本次真正解除了哪些目标上的映射。

    ⚠️ 映射来源与参数名的实测结论（Windows Server，IscsiTarget 2.0.0.0）：
      - 映射**不在虚拟盘侧**：Get-IscsiVirtualDisk 没有 TargetName 属性
        （只有 Path/Description/Status/DiskType/...），原实现靠它侧推必然为空，
        于是"先解映射"实际从未执行过；正确来源是目标对象的 LunMappings
        （{ TargetName, Path, Lun }）。
      - Remove-IscsiVirtualDiskTargetMapping 的参数是 **-Path**（不存在 -DevicePath）；
        原实现用 -DevicePath 且带 -ErrorAction SilentlyContinue，错误被整段吞掉。
      本脚本按运行时参数集自适应，并在参数缺失时**明确报错**而不是静默空转。
#>

param(
    [string]$Path
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Path) { throw 'Path 参数不能为空' }
    Import-Module -Name IscsiTarget -ErrorAction Stop

    $disk = Get-IscsiVirtualDisk -Path $Path -ErrorAction SilentlyContinue
    if (-not $disk) {
        [pscustomobject]@{ ok = $true; action = 'not_found'; path = $Path } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    # 解除该虚拟盘上的全部映射：从**目标侧**的 LunMappings 查（虚拟盘侧没有映射信息）。
    $removeCommand = Get-Command -Name Remove-IscsiVirtualDiskTargetMapping -ErrorAction SilentlyContinue
    $pathParameter = ''
    $lunParameter = $false
    if ($removeCommand) {
        $parameterNames = @($removeCommand.Parameters.Keys)
        if ($parameterNames -contains 'Path') { $pathParameter = 'Path' }
        elseif ($parameterNames -contains 'DevicePath') { $pathParameter = 'DevicePath' }
        $lunParameter = $parameterNames -contains 'Lun'
    }

    $unmappedTargets = @()
    foreach ($target in @(Get-IscsiServerTarget -ErrorAction SilentlyContinue)) {
        $targetName = [string]$target.TargetName
        foreach ($mapping in @($target.LunMappings)) {
            if ($null -eq $mapping) { continue }
            $mappingPath = ''
            try { $mappingPath = [string]$mapping.Path } catch { }
            if (-not $mappingPath -or $mappingPath -ne $Path) { continue }
            if (-not $pathParameter) {
                throw 'Remove-IscsiVirtualDiskTargetMapping 缺少 -Path / -DevicePath 参数（需实测确认参数集）'
            }
            $removeArgs = @{ TargetName = $targetName }
            $removeArgs[$pathParameter] = $Path
            if ($lunParameter) {
                $lun = $null
                try { $lun = [int]$mapping.Lun } catch { $lun = $null }
                if ($null -ne $lun) { $removeArgs['Lun'] = $lun }
            }
            Remove-IscsiVirtualDiskTargetMapping @removeArgs -ErrorAction Stop
            $unmappedTargets += $targetName
        }
    }

    Remove-IscsiVirtualDisk -Path $Path -ErrorAction Stop

    [pscustomobject]@{
        ok               = $true
        action           = 'removed'
        path             = $Path
        unmapped_targets = $unmappedTargets
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'virtual_disk_remove_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

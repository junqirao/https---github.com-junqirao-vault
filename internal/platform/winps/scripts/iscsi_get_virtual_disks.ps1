<#
.SYNOPSIS
    查询 iSCSI 虚拟盘列表（可指定单个 Path）。

.NOTES
    只查询登记对象，不触碰 .vhdx 文件本身。

    ⚠️ 实测（IscsiTarget 模块 2.0.0.0）：虚拟盘对象**没有 TargetName 属性**
    （属性只有 Path/Description/Status/DiskType/Size/SerialNumber/...），
    因此"从虚拟盘侧推映射"必然得到空列表。映射关系实际挂在**目标**侧：
    Get-IscsiServerTarget 的 LunMappings = { TargetName, Path, Lun } 集合。
    这里先从目标侧建立 Path → 目标名 的索引，再回填每块虚拟盘的 target_names；
    若某个版本上确实存在 TargetName 属性，则作为兜底合并进来。
#>

param(
    [string]$Path = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    Import-Module -Name IscsiTarget -ErrorAction Stop

    $disks = @()
    if ($Path) {
        $disks = @(Get-IscsiVirtualDisk -Path $Path -ErrorAction SilentlyContinue)
    } else {
        $disks = @(Get-IscsiVirtualDisk -ErrorAction SilentlyContinue)
    }

    # 目标侧映射索引：小写化后的虚拟盘路径 → 目标名集合（路径大小写不敏感）。
    $mappedByPath = @{}
    foreach ($target in @(Get-IscsiServerTarget -ErrorAction SilentlyContinue)) {
        foreach ($mapping in @($target.LunMappings)) {
            if ($null -eq $mapping) { continue }
            $mappingPath = ''
            try { $mappingPath = [string]$mapping.Path } catch { }
            if (-not $mappingPath) { continue }
            $key = $mappingPath.ToLowerInvariant()
            if (-not $mappedByPath.ContainsKey($key)) {
                $mappedByPath[$key] = New-Object System.Collections.ArrayList
            }
            $targetName = ''
            try { $targetName = [string]$mapping.TargetName } catch { }
            if ($targetName -and -not $mappedByPath[$key].Contains($targetName)) {
                [void]$mappedByPath[$key].Add($targetName)
            }
        }
    }

    $items = @()
    foreach ($disk in $disks) {
        $path = [string]$disk.Path
        $targetNames = @()
        if ($path -and $mappedByPath.ContainsKey($path.ToLowerInvariant())) {
            $targetNames = @($mappedByPath[$path.ToLowerInvariant()])
        }
        # 兜底：个别版本若真有 TargetName 属性，则合并（去重）。
        if ($null -ne $disk.TargetName) {
            foreach ($name in @($disk.TargetName)) {
                if ($name -and -not ($targetNames -contains $name)) { $targetNames += $name }
            }
        }
        $items += [pscustomobject]@{
            path         = [string]$disk.Path
            description  = [string]$disk.Description
            target_names = $targetNames
        }
    }

    [pscustomobject]@{
        ok    = $true
        count = @($items).Count
        disks = @($items)
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'virtual_disk_query_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

<#
.SYNOPSIS
    为指定磁盘分配盘符（已有盘符则复用），返回盘符（形如 "E:"）。

.NOTES
    由 Go 侧以 `-DiskNumber <n>` 调用。幂等：磁盘已挂载盘符时直接复用。
    ⚠️ 需实测确认：Add-PartitionAccessPath 的 -AssignDriveLetter 开关行为与
    无可用盘符时的报错形态；Get-Partition 返回对象的 UseOnlyDriveLetter 属性。
#>

param(
    [int]$DiskNumber
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    Get-Disk -Number $DiskNumber -ErrorAction Stop | Out-Null

    $partition = @(Get-Partition -DiskNumber $DiskNumber -ErrorAction Stop |
        Where-Object { $_.Type -ne 'Reserved' } |
        Sort-Object PartitionNumber |
        Select-Object -First 1)
    if ($partition.Count -eq 0) {
        [pscustomobject]@{
            ok      = $false
            reason  = 'no_partition'
            message = "磁盘 $DiskNumber 上没有可挂载的分区"
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    $partition = $partition[0]
    if ($partition.DriveLetter -and [string]$partition.DriveLetter -ne '') {
        [pscustomobject]@{
            ok               = $true
            action           = 'reuse'
            drive_letter     = ([string]$partition.DriveLetter + ':')
            partition_number = [int]$partition.PartitionNumber
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    Add-PartitionAccessPath -DiskNumber $DiskNumber -PartitionNumber $partition.PartitionNumber -AssignDriveLetter -ErrorAction Stop

    $updated = Get-Partition -DiskNumber $DiskNumber -PartitionNumber $partition.PartitionNumber -ErrorAction Stop
    if (-not $updated.DriveLetter -or [string]$updated.DriveLetter -eq '') {
        [pscustomobject]@{
            ok      = $false
            reason  = 'assign_drive_letter_failed'
            message = '已调用 Add-PartitionAccessPath，但分区仍未获得盘符'
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    [pscustomobject]@{
        ok               = $true
        action           = 'assigned'
        drive_letter     = ([string]$updated.DriveLetter + ':')
        partition_number = [int]$updated.PartitionNumber
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'mount_drive_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

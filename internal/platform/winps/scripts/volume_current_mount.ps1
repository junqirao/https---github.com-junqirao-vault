<#
.SYNOPSIS
    查询指定磁盘当前的挂载点（盘符优先，其次目录）。

.NOTES
    由 Go 侧以 `-DiskNumber <n>` 调用。未挂载时返回 mount_path='' 且 ok=true。
    卷 GUID 形式的 AccessPaths（\\?\Volume{...}\）不算业务挂载点，会被忽略。

    ⚠️ 需实测确认：Get-Partition 返回对象的 AccessPaths 属性内容与格式。
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
            ok         = $true
            mount_path = ''
            mount_mode = ''
            disk_number  = $DiskNumber
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }
    $partition = $partition[0]

    if ($partition.DriveLetter -and [string]$partition.DriveLetter -ne '') {
        [pscustomobject]@{
            ok               = $true
            mount_path       = ([string]$partition.DriveLetter + ':')
            mount_mode       = 'letter'
            disk_number      = $DiskNumber
            partition_number = [int]$partition.PartitionNumber
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $directory = ''
    foreach ($path in @($partition.AccessPaths)) {
        if (-not $path) { continue }
        if ($path -match '^\\\\\?\\Volume') { continue }
        if ($path -match '^[A-Za-z]:\\$') { continue }
        $directory = $path
        break
    }
    $mountMode = ''
    if ($directory) { $mountMode = 'directory' }

    [pscustomobject]@{
        ok               = $true
        mount_path       = $directory
        mount_mode       = $mountMode
        disk_number      = $DiskNumber
        partition_number = [int]$partition.PartitionNumber
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'current_mount_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

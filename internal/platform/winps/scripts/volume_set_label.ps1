<#
.SYNOPSIS
    设置指定磁盘所在卷的卷标（文件系统标签）。

.NOTES
    由 Go 侧以 `-DiskNumber <n> -Label <文本>` 调用。
    用途：盘符模式下把存储库名称写到卷标，资源管理器里该盘即以库名显示
    （用户诉求："盘符或目录名应该等于存储库的名称"；Windows 无法用库名做盘符，
    只能落到卷标上）。
    ⚠️ 需要该卷已有盘符：Set-Volume 只能用 -DriveLetter / -Path 定位目标卷，
    无盘符时本脚本返回 no_drive_letter（调用方按 best effort 告警处理）。
    调用方需先做字符清洗与长度截断（NTFS 卷标上限 32 字符），本脚本不代劳。
#>

param(
    [int]$DiskNumber,
    [string]$Label
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if ([string]::IsNullOrWhiteSpace($Label)) {
        [pscustomobject]@{
            ok      = $false
            reason  = 'empty_label'
            message = '卷标不能为空'
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    Get-Disk -Number $DiskNumber -ErrorAction Stop | Out-Null

    $partition = @(Get-Partition -DiskNumber $DiskNumber -ErrorAction Stop |
        Where-Object { $_.Type -ne 'Reserved' } |
        Sort-Object PartitionNumber |
        Select-Object -First 1)
    if ($partition.Count -eq 0) {
        [pscustomobject]@{
            ok      = $false
            reason  = 'no_partition'
            message = "磁盘 $DiskNumber 上没有可用的分区"
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    $partition = $partition[0]
    if (-not $partition.DriveLetter -or [string]$partition.DriveLetter -eq '') {
        [pscustomobject]@{
            ok      = $false
            reason  = 'no_drive_letter'
            message = '该分区没有盘符，无法定位卷来设置卷标'
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    $letter = ([string]$partition.DriveLetter).TrimEnd([char]':')
    Set-Volume -DriveLetter $letter -NewFileSystemLabel $Label -ErrorAction Stop

    $volume = Get-Volume -DriveLetter $letter -ErrorAction Stop
    [pscustomobject]@{
        ok                = $true
        drive_letter      = ($letter + ':')
        file_system_label = [string]$volume.FileSystemLabel
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'set_label_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

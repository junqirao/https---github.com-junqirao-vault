<#
.SYNOPSIS
    幂等地初始化 + 分区 + 格式化 VHDX 对应的磁盘，返回分配的盘符。

.DESCRIPTION
    幂等规则：
      - 仅当 PartitionStyle 为 RAW 时才 Initialize-Disk（重复初始化会报错）；
      - 复用已存在的第一个非保留分区，没有才 New-Partition；
      - 已有文件系统且与目标一致时跳过格式化；
      - 已有其它文件系统时**拒绝改写**并报错（避免覆盖已有数据）。

.NOTES
    Get-Volume -Partition / Format-Volume -Partition 的参数行为需实测确认（不同版本 Storage 模块略有差异）。
    没有可用盘符时 drive_letter 返回空串，由上层决定后续处理（例如改为目录挂载点）。
#>

param(
    [string]$VhdxPath,
    [string]$FileSystem = 'NTFS',
    [string]$Label = 'VAULT'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 按 VHDX 路径反查磁盘：先 Get-DiskImage 精确匹配，再退化为 Location 比对（必须先归一化路径）。
#
# ⚠️ 归一化不可省：Get-Disk 的 Location 永远是**长路径**（C:\Users\Administrator\...），
# 而调用方可能传来 8.3 短路径（C:\Users\ADMINI~1\...）。直接字符串比较会
# "磁盘明明已挂载却报 disk_not_found"，导致建盘流水线在格式化这一步整体失败。
# [System.IO.Path]::GetFullPath 会把 8.3 展开成长名（已实测），并统一大小写与分隔符。
function Find-DiskByVhdxPath {
    param([string]$Path)

    # ① Get-DiskImage：按镜像文件定位（本项目用 VirtDisk API 挂载，与 Mount-DiskImage 同机制）
    try {
        $img = Get-DiskImage -ImagePath $Path -ErrorAction Stop
        if ($img -and $img.Attached) {
            $disk = @($img | Get-Disk -ErrorAction SilentlyContinue) | Select-Object -First 1
            if ($disk) { return @{ disk = $disk; source = 'disk_image' } }
        }
    } catch { }

    $wanted = $Path
    try { $wanted = [System.IO.Path]::GetFullPath($Path) } catch { }

    foreach ($candidate in @(Get-Disk -ErrorAction SilentlyContinue)) {
        $location = [string]$candidate.Location
        if (-not $location) { continue }
        if ($location -eq $Path) { return @{ disk = $candidate; source = 'location' } }
        $normalized = $location
        try { $normalized = [System.IO.Path]::GetFullPath($location) } catch { }
        if ($normalized -eq $wanted) { return @{ disk = $candidate; source = 'location' } }
    }
    return $null
}

try {
    if (-not $VhdxPath) { throw 'VhdxPath 参数不能为空' }
    if (-not $FileSystem) { throw 'FileSystem 参数不能为空' }

    # 1) 反查磁盘号（刚挂载时缓存可能不含，需要先刷新）
    $refreshNote = ''
    $found = Find-DiskByVhdxPath -Path $VhdxPath
    if (-not $found) {
        # 刷新存储缓存需要管理员权限；刷新失败不应改变"是否找到磁盘"的判定语义
        try { Update-HostStorageCache -ErrorAction Stop } catch { $refreshNote = '（刷新存储缓存失败：' + $_.Exception.Message + '）' }
        $found = Find-DiskByVhdxPath -Path $VhdxPath
    }
    if (-not $found) {
        [pscustomobject]@{
            ok      = $false
            reason  = 'disk_not_found'
            message = ("未找到与 $VhdxPath 对应的磁盘" + $refreshNote)
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }
    $disk = $found.disk
    $diskNumber = [int]$disk.Number
    $partitionStyle = [string]$disk.PartitionStyle

    # 2) 初始化（仅 RAW）
    if ($partitionStyle -eq 'RAW') {
        Initialize-Disk -Number $diskNumber -PartitionStyle GPT -ErrorAction Stop | Out-Null
        $partitionStyle = 'GPT'
    }

    # 3) 复用已有分区，否则新建（跳过 Reserved/MSR 等保留分区）
    $partition = @(Get-Partition -DiskNumber $diskNumber -ErrorAction SilentlyContinue |
        Where-Object { $_.Type -ne 'Reserved' } |
        Sort-Object PartitionNumber) | Select-Object -First 1
    if (-not $partition) {
        $partition = New-Partition -DiskNumber $diskNumber -UseMaximumSize -AssignDriveLetter -ErrorAction Stop
    }

    # 4) 盘符：已有则复用；否则尝试分配（无可用盘符时留空）
    $driveLetter = ''
    if ($partition.DriveLetter -and [int][char]$partition.DriveLetter -ne 0) {
        $driveLetter = [string]$partition.DriveLetter
    } else {
        Add-PartitionAccessPath -DiskNumber $diskNumber -PartitionNumber $partition.PartitionNumber -AssignDriveLetter -ErrorAction SilentlyContinue
        $partition = Get-Partition -DiskNumber $diskNumber -PartitionNumber $partition.PartitionNumber -ErrorAction Stop
        if ($partition.DriveLetter -and [int][char]$partition.DriveLetter -ne 0) {
            $driveLetter = [string]$partition.DriveLetter
        }
    }

    # 5) 格式化（幂等）
    $volume = $null
    try { $volume = Get-Volume -Partition $partition -ErrorAction Stop } catch { $volume = $null }
    $currentFileSystem = ''
    if ($volume) { $currentFileSystem = [string]$volume.FileSystem }

    if ($currentFileSystem -and ($currentFileSystem -ne $FileSystem)) {
        throw "分区已格式化为 $currentFileSystem，拒绝改写为 $FileSystem（避免数据丢失）"
    }

    if (-not $currentFileSystem) {
        $formatArgs = @{ Partition = $partition; FileSystem = $FileSystem; Confirm = $false }
        if ($Label) { $formatArgs['NewFileSystemLabel'] = $Label }
        Format-Volume @formatArgs -ErrorAction Stop | Out-Null
        try { $volume = Get-Volume -Partition $partition -ErrorAction Stop } catch { $volume = $null }
    }

    $finalFileSystem = [string]$FileSystem
    $sizeBytes = 0
    if ($volume) {
        if ($volume.FileSystem) { $finalFileSystem = [string]$volume.FileSystem }
        if ($volume.Size) { $sizeBytes = [long]$volume.Size }
    }

    [pscustomobject]@{
        ok               = $true
        drive_letter     = $driveLetter
        partition_number = [int]$partition.PartitionNumber
        file_system      = $finalFileSystem
        size_bytes       = $sizeBytes
        disk_number      = $diskNumber
        partition_style  = $partitionStyle
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'ensure_formatted_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

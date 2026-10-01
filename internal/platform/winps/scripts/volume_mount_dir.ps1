<#
.SYNOPSIS
    把指定磁盘的分区挂载到已存在的空目录（Add-PartitionAccessPath -AccessPath）。

.NOTES
    由 Go 侧以 `-DiskNumber <n> -AccessPath <绝对路径>` 调用，参数不做字符串拼接。
    ⚠️ 目录挂载的三个硬前提（见 docs/implementation.md 5.5）：
      1) 目标目录必须已存在；
      2) 目标目录必须为空；
      3) 目标目录所在卷必须为 NTFS。
    三者任一不满足时返回明确的 reason（dir_not_found / dir_not_empty / parent_not_ntfs）。

    ⚠️ 需实测确认：Add-PartitionAccessPath -AccessPath 对「NTFS 空目录」的校验细节，
    以及 Get-Partition 返回对象的 AccessPaths 属性是否包含已挂载的目录路径。
#>

param(
    [int]$DiskNumber,
    [string]$AccessPath
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $AccessPath) { throw 'AccessPath 参数不能为空' }
    Get-Disk -Number $DiskNumber -ErrorAction Stop | Out-Null

    $full = [System.IO.Path]::GetFullPath($AccessPath)
    if (-not (Test-Path -LiteralPath $full)) {
        [pscustomobject]@{
            ok          = $false
            reason      = 'dir_not_found'
            message     = "挂载目录不存在：$full"
            access_path = $full
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }
    $item = Get-Item -LiteralPath $full -Force
    if (-not $item.PSIsContainer) {
        [pscustomobject]@{
            ok          = $false
            reason      = 'not_a_directory'
            message     = "挂载点不是目录：$full"
            access_path = $full
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    $children = @(Get-ChildItem -LiteralPath $full -Force)
    if ($children.Count -gt 0) {
        [pscustomobject]@{
            ok          = $false
            reason      = 'dir_not_empty'
            message     = "挂载目录必须为空：$full"
            access_path = $full
            child_count = $children.Count
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    # 父卷文件系统检查（仅当挂载点位于某个盘符根下时可判定，否则跳过）。
    $root = [System.IO.Path]::GetPathRoot($full)
    if ($root -match '^[A-Za-z]:\\$') {
        $letter = $root.Substring(0, 1)
        $volume = Get-Volume -DriveLetter $letter -ErrorAction SilentlyContinue
        if ($volume -and $volume.FileSystem -and $volume.FileSystem -ne 'NTFS') {
            [pscustomobject]@{
                ok          = $false
                reason      = 'parent_not_ntfs'
                message     = "挂载目录所在卷不是 NTFS（$($volume.FileSystem)）：$root"
                access_path = $full
            } | ConvertTo-Json -Compress -Depth 5
            exit 1
        }
    }

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

    $existing = @($partition.AccessPaths | Where-Object { $_ -eq $full })
    if ($existing.Count -gt 0) {
        [pscustomobject]@{
            ok               = $true
            action           = 'reuse'
            access_path      = $full
            partition_number = [int]$partition.PartitionNumber
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    Add-PartitionAccessPath -DiskNumber $DiskNumber -PartitionNumber $partition.PartitionNumber -AccessPath $full -ErrorAction Stop

    [pscustomobject]@{
        ok               = $true
        action           = 'assigned'
        access_path      = $full
        partition_number = [int]$partition.PartitionNumber
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'mount_dir_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

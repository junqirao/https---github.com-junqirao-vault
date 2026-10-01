<#
.SYNOPSIS
    按 Get-Disk 的 Location 属性（即 VHDX 全路径）反查磁盘号。

.NOTES
    刚挂载的 VHDX 可能还不在存储缓存里，因此查不到时会先 Update-HostStorageCache 再查一次。
    查不到时返回 {"ok":false,"reason":"disk_not_found"} 并 exit 1，便于上层映射为 disk.not_found。
#>

param(
    [string]$VhdxPath,
    [string]$RefreshCache = 'true'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 按 VHDX 路径反查磁盘：两条路径，先精确后兜底。
#
#  ① Get-DiskImage -ImagePath：直接按镜像文件定位（不做任何字符串比对），
#     这是最可靠的方式；本项目的挂载走 Go 的 VirtDisk API（AttachVirtualDisk），
#     与 Mount-DiskImage 是同一套挂载机制，因此同样能被枚举到；
#     若某版本上确实枚举不到，会自动退化为下面的 ②，不影响正确性；
#  ② 退化为按 Get-Disk 的 Location 比对，但**必须先归一化路径**：
#     Location 永远是长路径（C:\Users\Administrator\...），而调用方传来的可能是
#     8.3 短路径（C:\Users\ADMINI~1\...）——直接字符串比较会"磁盘明明挂载了却说找不到"。
#     [System.IO.Path]::GetFullPath 会把 8.3 展开成长名（已实测），同时统一大小写/分隔符。
function Find-DiskByVhdxPath {
    param([string]$Path)

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

    $refreshNote = ''
    $found = Find-DiskByVhdxPath -Path $VhdxPath
    if (-not $found -and $RefreshCache -eq 'true') {
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
    [pscustomobject]@{
        ok              = $true
        number          = [int]$disk.Number
        partition_style = [string]$disk.PartitionStyle
        size            = [long]$disk.Size
        location        = [string]$disk.Location
        source          = [string]$found.source
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'find_disk_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

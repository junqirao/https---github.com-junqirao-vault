<#
.SYNOPSIS
    查询指定路径所在卷的可用空间与总空间。

.NOTES
    优先用 Storage 模块的 Get-Volume -FilePath 定位所在卷（该参数在部分版本上需实测确认）；
    失败时回退到 .NET DriveInfo（要求路径位于某个盘符卷上）。
#>

param(
    [string]$Path
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Path) { throw 'Path 参数不能为空' }

    $volume = $null
    try { $volume = Get-Volume -FilePath $Path -ErrorAction Stop } catch { $volume = $null }

    if ($volume -and $volume.Size) {
        [pscustomobject]@{
            ok            = $true
            free          = [long]$volume.SizeRemaining
            total         = [long]$volume.Size
            file_system   = [string]$volume.FileSystem
            drive_letter  = [string]$volume.DriveLetter
            source        = 'Get-Volume'
        } | ConvertTo-Json -Compress -Depth 5
    } else {
        $resolved = Resolve-Path -LiteralPath $Path -ErrorAction Stop
        $root = [System.IO.Path]::GetPathRoot($resolved.ProviderPath)
        if (-not $root) { throw "无法确定 $Path 所在的卷根目录" }
        $drive = New-Object System.IO.DriveInfo($root)
        if (-not $drive.IsReady) { throw "卷不可用：$root" }
        [pscustomobject]@{
            ok           = $true
            free         = [long]$drive.AvailableFreeSpace
            total        = [long]$drive.TotalSize
            file_system  = [string]$drive.DriveFormat
            drive_letter = $root.TrimEnd('\')
            source       = 'DriveInfo'
        } | ConvertTo-Json -Compress -Depth 5
    }
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'freespace_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

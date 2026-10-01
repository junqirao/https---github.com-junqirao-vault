<#
.SYNOPSIS
    查询指定路径所在卷的文件系统名（如 NTFS）。

.NOTES
    优先用 Get-Volume -FilePath（需实测确认）；失败时回退到 .NET DriveInfo。
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

    if ($volume -and $volume.FileSystem) {
        [pscustomobject]@{
            ok           = $true
            file_system  = [string]$volume.FileSystem
            drive_letter = [string]$volume.DriveLetter
            source       = 'Get-Volume'
        } | ConvertTo-Json -Compress -Depth 5
    } else {
        $resolved = Resolve-Path -LiteralPath $Path -ErrorAction Stop
        $root = [System.IO.Path]::GetPathRoot($resolved.ProviderPath)
        if (-not $root) { throw "无法确定 $Path 所在的卷根目录" }
        $drive = New-Object System.IO.DriveInfo($root)
        if (-not $drive.IsReady) { throw "卷不可用：$root" }
        [pscustomobject]@{
            ok           = $true
            file_system  = [string]$drive.DriveFormat
            drive_letter = $root.TrimEnd('\')
            source       = 'DriveInfo'
        } | ConvertTo-Json -Compress -Depth 5
    }
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'filesystem_query_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

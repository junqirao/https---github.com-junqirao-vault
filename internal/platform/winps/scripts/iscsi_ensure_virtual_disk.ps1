<#
.SYNOPSIS
    幂等地把已有 VHDX 纳入 iSCSI 虚拟盘登记（Import-IscsiVirtualDisk）。

.NOTES
    已登记则跳过（Import 对已存在文件会报错，因此必须"先查后建"）。
    参数通过 -File 传入；输出为一行压缩 JSON，失败时 exit 1。

    注意：iSCSI 目标使用的 VHDX 必须先在本机分离（Dismount-DiskImage）。
#>

param(
    [string]$Path,
    [string]$Description = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Path) { throw 'Path 参数不能为空' }
    if (-not (Test-Path -LiteralPath $Path)) { throw "VHDX 文件不存在：$Path" }

    # 本机仍挂载的 VHDX 不能被 iSCSI 纳管（同一份文件不能被本地挂载与 iSCSI 发布同时持有）
    $image = Get-DiskImage -ImagePath $Path -ErrorAction SilentlyContinue
    if ($image -and $image.Attached) {
        throw "VHDX 仍在本机挂载，请先分离：$Path"
    }

    Import-Module -Name IscsiTarget -ErrorAction Stop

    $existing = $null
    try { $existing = Get-IscsiVirtualDisk -Path $Path -ErrorAction Stop } catch { $existing = $null }

    if ($existing) {
        $action = 'skipped'
        if ($Description -and ([string]$existing.Description -ne $Description)) {
            Set-IscsiVirtualDisk -Path $Path -Description $Description -ErrorAction Stop
            $action = 'updated'
        }
    } else {
        if ($Description) {
            Import-IscsiVirtualDisk -Path $Path -Description $Description -ErrorAction Stop
        } else {
            Import-IscsiVirtualDisk -Path $Path -ErrorAction Stop
        }
        $action = 'imported'
    }

    [pscustomobject]@{
        ok          = $true
        action      = $action
        path        = $Path
        description = [string]$Description
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'virtual_disk_import_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

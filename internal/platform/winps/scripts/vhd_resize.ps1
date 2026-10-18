<#
.SYNOPSIS
    扩容 VHDX（只扩不缩）。

.DESCRIPTION
    走 Hyper-V 模块的 Resize-VHD。母盘扩容只在维护态、且没有客户端挂载时调用，
    因此这里直接对离线盘执行；Hyper-V 模块缺失时如实报错，由上层提示需要安装。

.NOTES
    脚本最后一行才是 JSON，因此即使有 verbose 输出也不会影响上层解析。
#>

param(
    [string]$Path,
    [long]$SizeBytes
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Path) { throw 'Path 参数不能为空' }
    if ($SizeBytes -le 0) { throw 'SizeBytes 必须大于 0' }
    if (-not (Test-Path -LiteralPath $Path)) { throw "VHDX 文件不存在：$Path" }
    if (-not (Get-Command -Name Resize-VHD -ErrorAction SilentlyContinue)) {
        throw '缺少 Hyper-V 模块（Resize-VHD），无法扩容 VHDX'
    }

    Resize-VHD -Path $Path -SizeBytes $SizeBytes -ErrorAction Stop

    [pscustomobject]@{
        ok         = $true
        path       = $Path
        size_bytes = $SizeBytes
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'resize_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

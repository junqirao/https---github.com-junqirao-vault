<#
.SYNOPSIS
    回收 VHDX 空间（Retrim / Compact）。

.DESCRIPTION
    优先走 Hyper-V 模块：Mount-VHD -ReadOnly → Optimize-VHD → Dismount-VHD。
    无 Hyper-V 时回退到 Mount-DiskImage + Optimize-Volume -ReTrim + Dismount-DiskImage
    （只需 Storage 模块，但要求卷已分配盘符）。

.NOTES
    仅应对"空闲"的动态扩展盘执行（未挂载或只读挂载），且同一卷同一时刻只跑一个任务。
    Optimize-VHD 的 -Mode 取值（Full / Retrim / Quick）与 Mount-VHD 的只读挂载约束需实测确认。
    脚本最后一行才是 JSON，因此即使有 verbose 输出也不会影响上层解析。
#>

param(
    [string]$Path,
    [string]$Mode = 'Full'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $Path) { throw 'Path 参数不能为空' }
    if (-not (Test-Path -LiteralPath $Path)) { throw "VHDX 文件不存在：$Path" }

    $method = ''
    if (Get-Command -Name Optimize-VHD -ErrorAction SilentlyContinue) {
        Mount-VHD -Path $Path -ReadOnly -ErrorAction Stop
        try {
            Optimize-VHD -Path $Path -Mode $Mode -ErrorAction Stop
        } finally {
            Dismount-VHD -Path $Path -ErrorAction SilentlyContinue
        }
        $method = 'Optimize-VHD'
    } else {
        Mount-DiskImage -ImagePath $Path -ErrorAction Stop | Out-Null
        try {
            $volume = @(Get-DiskImage -ImagePath $Path -ErrorAction Stop |
                Get-Disk |
                Get-Partition |
                Where-Object { $_.DriveLetter -and [int][char]$_.DriveLetter -ne 0 } |
                Get-Volume) | Select-Object -First 1
            if (-not $volume) { throw '挂载的 VHDX 中没有已分配盘符的卷，无法执行 Retrim' }
            Optimize-Volume -DriveLetter $volume.DriveLetter -ReTrim -ErrorAction Stop
        } finally {
            Dismount-DiskImage -ImagePath $Path -Confirm:$false -ErrorAction SilentlyContinue
        }
        $method = 'Optimize-Volume'
    }

    [pscustomobject]@{
        ok     = $true
        path   = $Path
        mode   = $Mode
        method = $method
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'optimize_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

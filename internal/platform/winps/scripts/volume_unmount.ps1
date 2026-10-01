<#
.SYNOPSIS
    移除磁盘的挂载点（盘符或目录），不改变磁盘数据（Remove-PartitionAccessPath）。

.NOTES
    由 Go 侧以 `-AccessPath <"E:" 或 "C:\Vault\...">` 调用，参数不做字符串拼接。
    幂等：找不到对应挂载点时返回 action=none 并成功。

    ⚠️ 已实测确认（2026-10-01，Windows PowerShell 5.1）：

      - Remove-PartitionAccessPath 的参数集是**互斥**的：-DriveLetter 自成一套；
        -DiskNumber/-PartitionNumber 属于另一套，而**那一套里没有 -DriveLetter**。
        三者混着传会在**参数绑定阶段**直接失败，报
        「无法使用指定的命名参数解析参数集。」—— 命令根本没执行。
        真实事故：盘符模式卸载永远卡在 mount_point，而界面按阶段套的文案却显示
        "这块盘正被程序占用"，用户怎么找都找不到占用进程（根本没有占用）。
      - 因此：盘符模式只传 -DriveLetter；目录模式用 -DiskNumber/-PartitionNumber + -AccessPath
        （该参数集里 -AccessPath 为必填）。
      - Get-Partition 返回对象的 AccessPaths 属性包含已挂载的目录路径（本脚本据此反查分区）。
#>

param(
    [string]$AccessPath
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $AccessPath) { throw 'AccessPath 参数不能为空' }

    if ($AccessPath -match '^[A-Za-z]:$') {
        # 盘符模式：形如 "E:"
        $letter = $AccessPath.Substring(0, 1).ToUpperInvariant()
        $partition = Get-Partition -DriveLetter $letter -ErrorAction SilentlyContinue | Select-Object -First 1
        if (-not $partition) {
            [pscustomobject]@{
                ok          = $true
                action      = 'none'
                access_path = $AccessPath
            } | ConvertTo-Json -Compress -Depth 5
            exit 0
        }
        # ⚠️ 只能传 -DriveLetter：它与 -DiskNumber/-PartitionNumber 属于**不同参数集**，
        # 混传会在参数绑定阶段就失败（「无法使用指定的命名参数解析参数集。」），命令压根没执行，
        # 于是卸载永远卡在这一步（真实事故，详见文件头 .NOTES）。
        Remove-PartitionAccessPath -DriveLetter $letter -ErrorAction Stop

        [pscustomobject]@{
            ok          = $true
            action      = 'removed'
            mode        = 'letter'
            access_path = $AccessPath
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    # 目录模式：按 AccessPaths 反查分区
    $full = [System.IO.Path]::GetFullPath($AccessPath)
    $partition = @(Get-Partition -ErrorAction SilentlyContinue |
        Where-Object { @($_.AccessPaths) -contains $full } |
        Select-Object -First 1)
    if ($partition.Count -eq 0) {
        [pscustomobject]@{
            ok          = $true
            action      = 'none'
            access_path = $full
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }
    $partition = $partition[0]
    Remove-PartitionAccessPath -DiskNumber $partition.DiskNumber -PartitionNumber $partition.PartitionNumber -AccessPath $full -ErrorAction Stop

    [pscustomobject]@{
        ok          = $true
        action      = 'removed'
        mode        = 'directory'
        access_path = $full
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'unmount_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

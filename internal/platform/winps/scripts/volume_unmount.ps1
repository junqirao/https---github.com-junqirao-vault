<#
.SYNOPSIS
    移除磁盘的挂载点（盘符或目录），不改变磁盘数据（Remove-PartitionAccessPath）。

.NOTES
    由 Go 侧以 `-AccessPath <"E:" 或 "C:\Vault\...">` 调用，参数不做字符串拼接。
    幂等：找不到对应挂载点时返回 action=none 并成功。

    ⚠️ 需实测确认：Remove-PartitionAccessPath 必须二选一传 -DriveLetter 或 -AccessPath；
    Get-Partition 返回对象的 AccessPaths 属性是否包含已挂载的目录路径。
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
        Remove-PartitionAccessPath -DiskNumber $partition.DiskNumber -PartitionNumber $partition.PartitionNumber -DriveLetter $letter -ErrorAction Stop

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

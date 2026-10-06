<#
.SYNOPSIS
    移除磁盘的挂载点（盘符或目录），不改变磁盘数据（Remove-PartitionAccessPath）。

.NOTES
    由 Go 侧以 `-AccessPath <"E:" 或 "C:\Vault\...">` 调用，参数不做字符串拼接。
    幂等：找不到对应挂载点时返回 action=none 并成功。
    目录模式下若路径已是"残留的卷挂载点"（分区不再认领它、但 junction 还在），返回
    action=cleaned 表示顺手摘掉了它（见下面目录分支的说明）。

    ⚠️ 已实测确认（2026-10-01，Windows PowerShell 5.1）：移除挂载点**只有一种写法**，
    盘符与目录共用同一行命令 —— Get-Partition 定位分区 + 显式 -AccessPath。

      - Remove-PartitionAccessPath 的参数集**互斥**，而两个"省事"的写法都实测不可用：
          · -DriveLetter 与 -DiskNumber/-PartitionNumber 混传 → 在**参数绑定阶段**直接失败，
            报「无法使用指定的命名参数解析参数集。」，命令根本没执行；
          · 只传 -DriveLetter → 绑定通过，但该参数集自行推算出的 AccessPath 被 WMI 的
            MSFT_Partition.RemoveAccessPath 拒绝，报「传递给方法的一个或多个参数值无效。」
        两种写法后果完全一样：盘符模式卸载永远卡在 mount_point，而界面按阶段套的文案显示
        "这块盘正被程序占用"，用户怎么找都找不到占用进程（根本没有占用）。
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
        # ⚠️ 与目录模式**同一种写法**，别改回 -DriveLetter（两种写法都实测不可用，详见文件头 .NOTES）：
        #   混传 -DriveLetter/-DiskNumber → 参数绑定即失败；只传 -DriveLetter → WMI 拒绝它
        #   自行推算的 AccessPath（「传递给方法的一个或多个参数值无效。」）。盘符必须显式写全
        #   "H:\"（Get-Partition 的 AccessPaths 里就是这个形式）。
        Remove-PartitionAccessPath -DiskNumber $partition.DiskNumber -PartitionNumber $partition.PartitionNumber -AccessPath ($letter + ':\') -ErrorAction Stop

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
        # 分区已经不认领这个路径了，但**目录本身可能还是残留的卷挂载点**：实测（2026-10-06）
        # Remove-PartitionAccessPath 之后 Windows 会把 junction 留在原地（Get-Partition 的
        # AccessPaths 已空，路径却仍指向 \??\Volume{...}\），卷于是依旧可达。只报 action=none
        # 就等于把僵尸留在本机 —— 下一轮挂载会把新目录建进这个卷里、再把卷挂到自己的卷内，
        # 资源管理器里 C:\Vault\lib\lib\lib… 无限递归（见 volume_mount_dir.ps1 的 .NOTES）。
        # 悬空的 junction 也读得到 Target，所以这里能识别。
        $stale = Get-Item -LiteralPath $full -Force -ErrorAction SilentlyContinue
        if ($stale -and ($stale.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
            $target = @($stale.Target) | Where-Object { $_ } | Select-Object -First 1
            if ($target -match 'Volume\{[0-9a-fA-F\-]+\}') {
                # 只删链接（RemoveDirectory 对 junction 只删挂载点本身，卷内数据不受影响）。
                [System.IO.Directory]::Delete($full, $false)
                [pscustomobject]@{
                    ok          = $true
                    action      = 'cleaned'
                    access_path = $full
                } | ConvertTo-Json -Compress -Depth 5
                exit 0
            }
        }
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

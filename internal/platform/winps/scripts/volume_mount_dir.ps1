<#
.SYNOPSIS
    把指定磁盘的分区挂载到目录（Add-PartitionAccessPath -AccessPath）。

.NOTES
    由 Go 侧以 `-DiskNumber <n> -AccessPath <绝对路径>` 调用，参数不做字符串拼接。
    ⚠️ 目录挂载的前提（见 docs/implementation.md 5.5）：
      1) 目标目录为空 —— 不存在则由本脚本创建（等价 mkdir -p，Go 侧不再自己建目录，原因见下）；
      2) 目标目录所在卷必须为 NTFS；
      3) 目标目录**不能落在本磁盘自己的卷里面**（见下面的"僵尸挂载点"）。
    不满足时返回明确的 reason（dir_not_empty / parent_not_ntfs / not_a_directory）。

    ⚠️ "父目录本身就是本卷的挂载点" → 挂载点自指 → 无限嵌套（真实事故 2026-10-06）：
    老版本把用户填的父目录**直接当成挂载点**，于是卷根暴露在 C:\Vault 上。此后用户在界面上
    配置 C:\Vault\WIN-0E595H11JSS_test2 时，"在 C:\Vault 下建一级目录"实际是在**卷内部**建
    目录，卷随后又被挂到卷内这个路径上 —— 挂载点指向自己，资源管理器里
    …\WIN-0E595H11JSS_test2\WIN-0E595H11JSS_test2\… 无限递归（用户截图）。
    这一步是修复而不是报错：用户诉求就是"在父目录下多一级再挂上去"，这个挂载点属于程序自己的
    历史包袱，不该让用户手动清（Go 侧也不该在它下面建目录，那正是把垃圾建进卷里的做法）。

    本脚本在挂载前按下面的顺序处理（实测两种形态都要兼容）：
      0) 目标路径**已经**是本分区的挂载点 ⇒ 直接 reuse（幂等），绝不动它；
      1) 向上找目标路径最近的卷挂载点祖先（含自身），判断它是不是**本磁盘自己的卷**：
           · 本分区当前就认领这个路径（AccessPaths 命中）——**最常见**，用户看到的就是这种：
             父目录此刻仍合法地挂在卷上（不是残留）；
           · 或者它是卸载后 Windows 留在原地的 junction 残骸：Get-Partition 的 AccessPaths
             已经空了，但路径仍指向 \??\Volume{GUID}\（卷因此依旧可达）——此时用 junction 的
             Target GUID 与本磁盘卷 GUID 比对。
         ⚠️ 卷 GUID 优先从 $partition.AccessPaths 的 `\\?\Volume{GUID}\` 里提取，Get-Volume
            只作兜底：实测卷离线/属性不全时 Get-Volume 静默给空值，整个判定会失效。
      2) 确认是本磁盘的卷 ⇒
           · 先删卷内 $full 与祖先之间各层残留（junction 只摘链接、空目录删掉；有真实内容
             则按 dir_not_empty 如实报错，绝不删用户数据）；
           · 再摘掉祖先挂载点：分区仍认领它时走 Remove-PartitionAccessPath（正规摘除；
             只删目录链接会留下"分区认领一条不存在的路径"的不一致状态），
             之后若路径仍是 junction（残骸）再用 Directory.Delete 只摘链接；
           · 回到真实目录重建 $full 并挂载 —— 否则这一轮挂载又挂在"卷内路径"上；
           · 挂载后补清卷内同名自指残留（卷当时不可达时那一层只能在挂载后才删得到）。
    返回 action='healed' 表示发生过修复（Go 侧据此告警，便于日后排查）。

    ⚠️ 需实测确认：Add-PartitionAccessPath -AccessPath 对「NTFS 空目录」的校验细节。
#>

param(
    [int]$DiskNumber,
    [string]$AccessPath
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 若路径（或自身）是卷挂载点，返回它指向的卷 GUID 文本（形如 Volume{...}），否则返回空串。
# 悬空的 junction 也读得到 Target（实测 2026-10-06），这正是识别僵尸挂载点的关键。
function Get-PathVolumeGuidText([string]$path) {
    $item = Get-Item -LiteralPath $path -Force -ErrorAction SilentlyContinue
    if (-not $item) { return '' }
    if (-not ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) { return '' }
    $target = @($item.Target) | Where-Object { $_ } | Select-Object -First 1
    if (-not $target) { return '' }
    if ($target -match 'Volume\{[0-9a-fA-F\-]+\}') { return $Matches[0] }
    return ''
}

# 本磁盘数据分区当前认领的挂载点里，与 $path 等价的那一条（原样字符串）。
# 返回值要原样拿去调 Remove-PartitionAccessPath —— AccessPaths 里目录形式带尾反斜杠
# （如 "C:\Vault\"），传成别的形态会摘不掉。
function Get-ClaimedAccessPath($partition, [string]$path) {
    $want = ([string]$path).TrimEnd('\')
    foreach ($ap in @($partition.AccessPaths)) {
        if (([string]$ap).TrimEnd('\') -ieq $want) { return [string]$ap }
    }
    return ''
}

# 本磁盘数据分区的卷 GUID 文本（形如 Volume{...}）；取不到返回空串。
#
# ⚠️ 优先从 $partition.AccessPaths 里的 `\\?\Volume{GUID}\` 提取，**不要**只依赖 Get-Volume：
# 实测（2026-10-06）卷离线、或 Get-Partition -DiskNumber 返回的实例属性不完整时，
# Get-Volume 会静默返回空值，导致本脚本的僵尸判定整个失效 —— 修复分支不触发，
# 于是"还是无限嵌套"（用户复现）。Get-Volume 只作为兜底。
function Get-PartitionVolumeGuidText($partition) {
    foreach ($ap in @($partition.AccessPaths)) {
        if (([string]$ap) -match 'Volume\{[0-9a-fA-F\-]+\}') { return $Matches[0] }
    }
    $vol = $partition | Get-Volume -ErrorAction SilentlyContinue
    if (-not $vol) {
        $vol = Get-Volume -Partition $partition -ErrorAction SilentlyContinue | Select-Object -First 1
    }
    if ($vol -and ([string]$vol.UniqueId) -match 'Volume\{[0-9a-fA-F\-]+\}') { return $Matches[0] }
    return ''
}

try {
    if (-not $AccessPath) { throw 'AccessPath 参数不能为空' }
    Get-Disk -Number $DiskNumber -ErrorAction Stop | Out-Null

    $full = [System.IO.Path]::GetFullPath($AccessPath)

    # 数据分区（盘符与目录都挂在它上面）。
    # ⚠️ 刻意用"全量 Get-Partition 再筛"而不是 `Get-Partition -DiskNumber`：实测后者返回的实例
    # 有时缺 AccessPaths/卷信息（同一块盘换种取法就没有），而本脚本的卷归属判定要靠它。
    $partition = @(Get-Partition -ErrorAction Stop |
        Where-Object { $_.DiskNumber -eq $DiskNumber -and $_.Type -ne 'Reserved' } |
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

    # 幂等：本分区已经挂在这个路径上 ⇒ 直接复用。
    # ⚠️ 必须放在"僵尸修复"之前：此时 $full 自己就是本卷的挂载点，会被下面的向上查找
    # 当成"路径穿过了本卷"而误清理（把合法挂载点删掉再重建）。
    if (Get-ClaimedAccessPath $partition $full) {
        [pscustomobject]@{
            ok               = $true
            action           = 'reuse'
            access_path      = $full
            partition_number = [int]$partition.PartitionNumber
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    $diskVolumeGuid = Get-PartitionVolumeGuidText $partition

    # 目标路径向上最近的卷挂载点祖先（含自身）：命中说明路径穿过了某个卷挂载点。
    $zombie = ''
    $zombieGuid = ''
    $cursor = $full
    while ($cursor) {
        $guid = Get-PathVolumeGuidText $cursor
        if ($guid) {
            $zombie = $cursor
            $zombieGuid = $guid
            break
        }
        $parent = Split-Path -Parent $cursor
        if (-not $parent -or $parent -eq $cursor) { break }
        $cursor = $parent
    }

    # 那个祖先是不是**本磁盘自己的卷**：两条独立判据，取或。
    #   ① 本分区当前就认领它（AccessPaths 命中）——最直接，不依赖卷 GUID，也不受卷离线影响；
    #   ② 它是指向本磁盘卷的 junction 残骸（Target 的 Volume{GUID} 与本卷一致）。
    $zombieClaim = ''
    if ($zombie) { $zombieClaim = Get-ClaimedAccessPath $partition $zombie }
    $isOwnVolume = $false
    if ($zombie) {
        $isOwnVolume = [bool]($zombieClaim -or
            ($zombieGuid -and $diskVolumeGuid -and ($zombieGuid -ieq $diskVolumeGuid)))
    }

    $created = $false
    $healed = $false
    $innerRel = ''
    if ($isOwnVolume) {
        # 目标路径落在本磁盘自己的卷里（用户的"父目录"就是本卷的挂载点）。
        # 修复顺序：① 先把卷内 $full 一侧的残留删掉（此时卷还挂在 $zombie 上，可达）；
        #           ② 再把 $zombie 这个挂载点摘掉，让父目录回到真实目录；
        #           ③ 然后才能建 $full 并挂载 —— 否则这一轮挂载本身又挂在"卷内路径"上。
        $zombiePrefix = $zombie.TrimEnd('\') + '\'
        $rel = ''
        if ($full.StartsWith($zombiePrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
            $rel = $full.Substring($zombiePrefix.Length).TrimStart('\')
        }

        # ① 卷内 $full 与 $zombie 之间（不含 $zombie 自己）的每一层：都在卷内部，是历史包袱。
        #    只删链接与非空目录（Directory.Delete(p,false) 对 junction 只摘链接，卷内数据不动）。
        $chain = @()
        $walk = $full
        while ($walk -and $walk.StartsWith($zombiePrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
            $chain += $walk
            $walk = Split-Path -Parent $walk
        }
        foreach ($p in $chain) {
            $item = Get-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
            if (-not $item) { continue }
            if (-not ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
                $kids = @(Get-ChildItem -LiteralPath $p -Force -ErrorAction SilentlyContinue)
                if ($kids.Count -gt 0) {
                    # 卷内该位置有真实内容：绝不删（可能是用户数据），按"目录非空"如实报错。
                    [pscustomobject]@{
                        ok          = $false
                        reason      = 'dir_not_empty'
                        message     = "挂载目录必须为空，但它落在本磁盘自己的卷里且已有内容：$p"
                        access_path = $full
                        child_count = $kids.Count
                    } | ConvertTo-Json -Compress -Depth 5
                    exit 1
                }
            }
            [System.IO.Directory]::Delete($p, $false)
        }

        # ② 摘掉 $zombie 上的挂载点。
        #    ⚠️ $zombie 常常**不是**"卸载后残留"，而是本分区此刻仍在认领的挂载点
        #    （实测：Get-Partition 的 AccessPaths 里就是 "C:\Vault\"）。这种情况必须走
        #    Remove-PartitionAccessPath 正规摘除；只删目录链接会留下"分区认领着一条
        #    不存在的路径"的不一致状态，且删正在使用的挂载点本身也可能失败。
        if ($zombieClaim) {
            Remove-PartitionAccessPath -DiskNumber $DiskNumber `
                -PartitionNumber $partition.PartitionNumber `
                -AccessPath $zombieClaim -ErrorAction Stop
        }
        # 摘除后若路径仍是 junction（Windows 有时把链接留在原地：AccessPaths 已空、
        # 路径仍指向 \??\Volume{GUID}\），再只删链接。
        $zombieItem = Get-Item -LiteralPath $zombie -Force -ErrorAction SilentlyContinue
        if ($zombieItem -and ($zombieItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
            [System.IO.Directory]::Delete($zombie, $false)
        }

        # ③ 回到真实目录重建目标路径（$zombie 若被摘掉/删掉，-Force 会连带重建父目录）。
        New-Item -ItemType Directory -Path $full -Force | Out-Null
        $created = $true
        $healed = $true
        $innerRel = $rel
    }

    if (-not $created -and -not (Test-Path -LiteralPath $full)) {
        New-Item -ItemType Directory -Path $full -Force | Out-Null
        $created = $true
    }
    if (-not (Test-Path -LiteralPath $full)) {
        [pscustomobject]@{
            ok          = $false
            reason      = 'dir_not_found'
            message     = "挂载目录不存在且无法创建：$full"
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

    # 幂等复用已在最前面判过（那时 $partition 的 AccessPaths 还是最新的）。
    Add-PartitionAccessPath -DiskNumber $DiskNumber -PartitionNumber $partition.PartitionNumber -AccessPath $full -ErrorAction Stop

    if ($healed -and $innerRel) {
        # 卷内与挂载点同名的残留（自指 junction）：卷当时若不可达（悬空 junction）就删不到，
        # 现在卷挂上来了再删一次 —— 留着它，资源管理器里照样无限递归。
        # 循环删是为了兜住历史上攒下的多层同名自指链接（删一层往往就够，但别赌）。
        for ($i = 0; $i -lt 8; $i++) {
            $innerJunk = Join-Path $full $innerRel
            $innerItem = Get-Item -LiteralPath $innerJunk -Force -ErrorAction SilentlyContinue
            if (-not $innerItem) { break }
            if (-not ($innerItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) { break }
            [System.IO.Directory]::Delete($innerJunk, $false)
        }
    }

    [pscustomobject]@{
        ok                 = $true
        action             = $(if ($healed) { 'healed' } else { 'assigned' })
        access_path        = $full
        partition_number   = [int]$partition.PartitionNumber
        created            = $created
        cleaned_mount_path = $zombie
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'mount_dir_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

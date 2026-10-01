<#
.SYNOPSIS
    按容量与已占用磁盘列表定位客户端侧的 iSCSI 磁盘，返回磁盘号。

.NOTES
    由 Go 侧以 `-SizeBytes <n> -UsedDiskNumbers <json 数组>` 调用，参数不做字符串拼接。
    已占用磁盘（本代理本次会话已挂载的磁盘）会被排除，避免同一尺寸的多块盘互相抢占。

    ⚠️ 需实测确认：多块「同尺寸 + 同 BusType=iSCSI」的磁盘无法仅凭容量区分，
    真实环境需要用 unique id（Get-Disk 的 UniqueId / SerialNumber / Path）精确定位。
    本脚本在候选多于 1 个时仍返回第一个候选，并通过 candidate_count 告知上层（上层应记 WARN）。

    刚连接上的 iSCSI 盘可能还不在存储缓存里，因此首次查不到时会先 Update-HostStorageCache 再查一次。
#>

param(
    [long]$SizeBytes = 0,
    [string]$UsedDiskNumbers = '[]',
    [string]$RefreshCache = 'true'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 在 BusType 为 iSCSI 的磁盘中按容量筛选，并排除已占用磁盘号。
function Find-IscsiDisk {
    param(
        [long]$Size,
        [int[]]$Used
    )
    $candidates = @(Get-Disk -ErrorAction SilentlyContinue | Where-Object {
        $_.BusType -eq 'iSCSI' -and [long]$_.Size -eq $Size -and ($Used -notcontains [int]$_.Number)
    } | Sort-Object Number)
    return $candidates
}

try {
    if ($SizeBytes -le 0) { throw 'SizeBytes 必须为正数' }

    # ⚠️ Windows PowerShell 5.1 的 ConvertFrom-Json 把 JSON 数组**整体当成一个对象**输出
    # （不展开元素），因此 `@(ConvertFrom-Json ... | Where-Object {...})` 交到手里的 `$_`
    # 是**整个数组**而不是它的元素 —— 再 `[int]` 转换就抛
    # "无法将 System.Object[] 转换为 System.Int32"，整个脚本以 find_iscsi_disk_failed 收场。
    # 后果是**本机只要已经挂了一块盘（UsedDiskNumbers 非空），后续每次挂载都失败**
    # （真实事故：池位盘预创建后连挂第二块盘必然失败）。
    # 修法：不要在管道里消费它，在**变量**上用 @(...) 强制展开成元素。
    $used = @()
    if ($UsedDiskNumbers -and $UsedDiskNumbers -ne '') {
        $decoded = ConvertFrom-Json -InputObject $UsedDiskNumbers
        foreach ($item in @($decoded)) {
            if ($null -ne $item) { $used += [int]$item }
        }
    }

    $refreshNote = ''
    $candidates = @(Find-IscsiDisk -Size $SizeBytes -Used $used)
    if ($candidates.Count -eq 0 -and $RefreshCache -eq 'true') {
        # 刷新存储缓存需要管理员权限；刷新失败不应改变「是否找到磁盘」的判定语义。
        try { Update-HostStorageCache -ErrorAction Stop } catch { $refreshNote = '（刷新存储缓存失败：' + $_.Exception.Message + '）' }
        $candidates = @(Find-IscsiDisk -Size $SizeBytes -Used $used)
    }

    if ($candidates.Count -eq 0) {
        [pscustomobject]@{
            ok      = $false
            reason  = 'disk_not_found'
            message = ("未找到容量为 $SizeBytes 字节的 iSCSI 磁盘" + $refreshNote)
        } | ConvertTo-Json -Compress -Depth 5
        exit 1
    }

    $disk = $candidates[0]
    [pscustomobject]@{
        ok              = $true
        number          = [int]$disk.Number
        size            = [long]$disk.Size
        bus_type        = [string]$disk.BusType
        partition_style = [string]$disk.PartitionStyle
        is_offline      = [bool]$disk.IsOffline
        is_read_only    = [bool]$disk.IsReadOnly
        candidate_count = $candidates.Count
        location        = [string]$disk.Location
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'find_iscsi_disk_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

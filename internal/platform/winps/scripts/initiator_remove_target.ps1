<#
.SYNOPSIS
    清理本地 iSCSI 发起端已发现的目标缓存（避免重名/残留冲突）。

.NOTES
    由 Go 侧以 `-TargetIQN <iqn>` 调用，幂等（目标不存在视为成功）。

    ⚠️ 需实测确认：系统的 IscsiInitiator 模块是否提供 Remove-IscsiTarget，
    以及其参数名（-NodeAddress）。未提供时脚本返回 action=unsupported 且仍然 exit 0
    （尽力而为，不阻塞上层卸载流程）。
#>

param(
    [string]$TargetIQN
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    if (-not $TargetIQN) { throw 'TargetIQN 参数不能为空' }

    $command = Get-Command -Name Remove-IscsiTarget -ErrorAction SilentlyContinue
    if (-not $command) {
        [pscustomobject]@{
            ok         = $true
            action     = 'unsupported'
            message    = '当前系统未提供 Remove-IscsiTarget，未清理发现缓存（需实测确认）'
            target_iqn = $TargetIQN
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    Remove-IscsiTarget -NodeAddress $TargetIQN -ErrorAction SilentlyContinue | Out-Null

    [pscustomobject]@{
        ok         = $true
        action     = 'removed'
        target_iqn = $TargetIQN
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'remove_target_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

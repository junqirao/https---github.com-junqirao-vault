<#
.SYNOPSIS
    探测 iSCSI 目标服务器能力（IscsiTarget 模块 + WinTarget 服务）。

.NOTES
    由 Go 侧以 `powershell -File <脚本> -ServiceName WinTarget` 调用（见 internal/platform/winps）。
    参数一律通过 param() 声明，调用方以 -Name value 形式传入，不做字符串拼接。
    输出约定：成功时 stdout 最后一行是含 ok=true 的压缩 JSON；失败时输出 ok=false 并 exit 1。
#>

param(
    [string]$ServiceName = 'WinTarget'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    $module = Get-Module -ListAvailable -Name IscsiTarget | Select-Object -First 1
    if (-not $module) {
        throw 'IscsiTarget 模块不可用：请安装 iSCSI 目标服务器角色（Install-WindowsFeature FS-iSCSITarget-Server）'
    }

    if (-not (Get-Module -Name IscsiTarget)) {
        Import-Module -Name IscsiTarget -ErrorAction Stop
    }

    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $service) {
        throw "找不到 iSCSI 目标服务（$ServiceName）"
    }

    [pscustomobject]@{
        ok             = $true
        module_version = [string]$module.Version
        service_name   = [string]$ServiceName
        service_status = [string]$service.Status
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'iscsi_unavailable'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

<#
.SYNOPSIS
    探测 iSCSI 发起端（Initiator）能力：IscsiInitiator 模块 + MSiSCSI 服务。

.NOTES
    由 Go 侧以 `powershell -File <脚本> -ServiceName MSiSCSI` 调用（见 internal/platform/winps）。
    参数一律通过 param() 声明，调用方以 -Name value 形式传入，不做字符串拼接。
    输出约定：成功时 stdout 最后一行是含 ok=true 的压缩 JSON；失败时输出 ok=false 并 exit 1。

    ⚠️ 需实测确认：发起端 cmdlet（Get-IscsiSession / Connect-IscsiTarget 等）在部分系统上
    归属于 IscsiInitiator 模块，模块名与发起端服务名可能存在差异；本脚本已做「模块缺失时
    退化为按命令是否存在判断」的兜底。
#>

param(
    [string]$ServiceName = 'MSiSCSI'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    # 需实测确认：模块名
    $module = Get-Module -ListAvailable -Name IscsiInitiator | Select-Object -First 1
    if ($module -and -not (Get-Module -Name IscsiInitiator)) {
        Import-Module -Name IscsiInitiator -ErrorAction Stop
    }
    if (-not (Get-Command -Name Get-IscsiSession -ErrorAction SilentlyContinue)) {
        throw 'IscsiInitiator 模块不可用：请启用 Microsoft iSCSI 发起程序（服务 MSiSCSI）'
    }

    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $service) {
        throw "找不到 iSCSI 发起端服务（$ServiceName）"
    }

    $moduleVersion = ''
    if ($module) { $moduleVersion = [string]$module.Version }

    [pscustomobject]@{
        ok             = $true
        module_version = $moduleVersion
        service_name   = [string]$ServiceName
        service_status = [string]$service.Status
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'initiator_unavailable'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

<#
.SYNOPSIS
    确保 iSCSI 发起端服务（默认 MSiSCSI）处于运行状态。

.NOTES
    由 Go 侧以 `powershell -File <脚本> -ServiceName MSiSCSI` 调用（见 internal/platform/winps）。
    输出约定：成功时 stdout 最后一行是含 ok=true 的压缩 JSON；失败时输出 ok=false 并 exit 1。

    ⚠️ 为什么必须有这一步（真实工单）：
      Connect-IscsiTarget / Get-IscsiSession 等 cmdlet 依赖该服务的 WMI 提供程序。服务未运行时
      调用会直接抛 CIM 异常，而挂载链路只把错误压成 reason=connect_failed，界面只能显示
      "挂载失败（阶段：connect）"，真因（服务没起来）被埋进日志，极难排查。
      设计文档 5.5 步骤 ③a 本就要求 "Start-Service msiscsi / 确保服务运行"，此前实现缺失。

    幂等与边界：
      - 已在运行则不做任何改动（action=already_running）；
      - 只"启动"，**不修改启动类型**（那是宿主机策略，应由部署/镜像固化）；
      - 启动后等待进入 Running（Start-Service 返回时可能仍在 StartPending）。
#>

param(
    [string]$ServiceName = 'MSiSCSI',
    [int]$WaitSeconds = 20
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $service) {
        throw "找不到 iSCSI 发起端服务（$ServiceName）：请在「服务器管理器 → 添加角色和功能」中启用「iSCSI 发起程序」"
    }

    if ($service.Status -eq 'Running') {
        [pscustomobject]@{
            ok             = $true
            action         = 'already_running'
            service_name   = [string]$ServiceName
            service_status = [string]$service.Status
        } | ConvertTo-Json -Compress -Depth 5
        exit 0
    }

    Start-Service -Name $ServiceName -ErrorAction Stop

    # 等待进入 Running：Start-Service 返回时服务可能仍在 StartPending。
    $deadline = (Get-Date).AddSeconds($WaitSeconds)
    do {
        Start-Sleep -Milliseconds 300
        $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    } while ($service -and $service.Status -ne 'Running' -and (Get-Date) -lt $deadline)

    $finalStatus = 'unknown'
    if ($service) { $finalStatus = [string]$service.Status }
    if ($finalStatus -ne 'Running') {
        throw "启动 iSCSI 发起端服务（$ServiceName）失败：当前状态 $finalStatus"
    }

    [pscustomobject]@{
        ok             = $true
        action         = 'started'
        service_name   = [string]$ServiceName
        service_status = $finalStatus
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'initiator_service_unavailable'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

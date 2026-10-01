<#
.SYNOPSIS
    查询本机证书存储中的证书指纹（服务端 mTLS 自检 / 能力探测用）。

.NOTES
    可选能力。默认查询 LocalMachine\My；Thumbprint 为空时列出全部证书。
    Thumbprint 匹配时会忽略空格与大小写差异。
#>

param(
    [string]$Thumbprint = '',
    [string]$StoreLocation = 'LocalMachine',
    [string]$StoreName = 'My'
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

try {
    $store = New-Object System.Security.Cryptography.X509Certificates.X509Store($StoreName, $StoreLocation)
    $store.Open('ReadOnly')
    try {
        $certs = @()
        if ($Thumbprint) {
            $wanted = $Thumbprint -replace '\s', ''
            $certs = @($store.Certificates | Where-Object { $_.Thumbprint -eq $wanted })
        } else {
            $certs = @($store.Certificates)
        }

        $items = @()
        foreach ($cert in $certs) {
            $sha256 = ''
            $algorithm = $null
            try {
                $algorithm = [System.Security.Cryptography.SHA256]::Create()
                if ($algorithm) {
                    $sha256 = ([System.BitConverter]::ToString($algorithm.ComputeHash($cert.RawData))).Replace('-', '')
                }
            } catch {
                $sha256 = ''
            } finally {
                if ($algorithm) { $algorithm.Dispose() }
            }

            $items += [pscustomobject]@{
                subject           = [string]$cert.Subject
                thumbprint_sha1   = [string]$cert.Thumbprint
                thumbprint_sha256 = $sha256
                not_after         = [string]$cert.NotAfter.ToString('o')
                has_private_key   = [bool]$cert.HasPrivateKey
            }
        }

        [pscustomobject]@{
            ok           = $true
            count        = @($items).Count
            certificates = @($items)
        } | ConvertTo-Json -Compress -Depth 5
    } finally {
        $store.Close()
    }
} catch {
    [pscustomobject]@{
        ok      = $false
        reason  = 'cert_fingerprint_failed'
        message = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}

$ErrorActionPreference = 'Continue'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$dir = 'c:\Users\89412\Projects\golang\vault\internal\platform\winps\scripts'
$files = @(
    'initiator_disconnect.ps1',
    'initiator_unregister.ps1',
    'volume_mount_dir.ps1',
    'volume_unmount.ps1'
)
foreach ($f in $files) {
    $p = Join-Path $dir $f
    if (-not (Test-Path -LiteralPath $p)) { Write-Output "MISSING    $f"; continue }
    $errs = $null; $tokens = $null
    [System.Management.Automation.Language.Parser]::ParseFile($p, [ref]$tokens, [ref]$errs) | Out-Null
    if ($errs -and $errs.Count -gt 0) {
        Write-Output "PARSE FAIL $f"
        $errs | ForEach-Object { Write-Output ("  line {0}: {1}" -f $_.Extent.StartLineNumber, $_.Message) }
    } else {
        Write-Output "PARSE OK   $f"
    }
}

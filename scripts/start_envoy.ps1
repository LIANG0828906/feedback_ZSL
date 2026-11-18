param(
    [string]$Config = "config/envoy.yaml"
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

python -m connectlab init | Out-Null

$Envoy = Get-Command envoy.exe -ErrorAction SilentlyContinue
if ($null -eq $Envoy) {
    $Envoy = Get-Command envoy -ErrorAction SilentlyContinue
}
if ($null -eq $Envoy) {
    throw "Envoy 1.30 executable not found on PATH. Install the Windows build and retry."
}

& $Envoy.Source --version
& $Envoy.Source -c $Config --log-level info

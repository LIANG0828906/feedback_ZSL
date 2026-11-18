param(
    [string]$Config = ""
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

if ($Config -ne "") {
    $env:CONNECTLAB_CONFIG = $Config
}

python -m connectlab init
python -m connectlab serve

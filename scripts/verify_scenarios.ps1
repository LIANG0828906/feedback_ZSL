param(
    [string]$Config = ""
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

$Arguments = @("scripts/verify_scenarios.py")
if ($Config -ne "") {
    $Arguments += @("--config", $Config)
}

python @Arguments

param(
    [double]$Duration = 5,
    [int]$Seed = 309,
    [int]$Devices = 24
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $Root

if (-not $env:GO) {
    $env:GO = (Get-Command go -ErrorAction Stop).Source
}

python scripts/run_scenario.py --duration $Duration --seed $Seed --devices $Devices
if ($LASTEXITCODE -ne 0) {
    throw "scenario failed"
}
python scripts/check_report.py
if ($LASTEXITCODE -ne 0) {
    throw "report check failed"
}

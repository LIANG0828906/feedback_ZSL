$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

if (-not (Test-Path ".venv\Scripts\python.exe")) {
    py -3.11 -m venv .venv
}
& .venv\Scripts\python.exe -m pip install -r requirements.txt

& .venv\Scripts\python.exe -m minute_backfill.cli generate `
    --out data\demo --seed 42 --payment-rows 100000 --refund-rows 40000 --batches 128

& .venv\Scripts\python.exe -m minute_backfill.cli run `
    --data data\demo --state state\demo.db --out out\demo `
    --slots 6 --workers 2 --allowed-lateness-seconds 60

Write-Host "Report: out\demo\report.json"

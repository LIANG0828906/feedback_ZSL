$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

if (-not (Test-Path ".venv\Scripts\python.exe")) {
    py -3.11 -m venv .venv
}

$Python = Join-Path $Root ".venv\Scripts\python.exe"
& $Python -m pip install --upgrade pip
& $Python -m pip install -r requirements.txt
& $Python scripts\generate_samples.py

Write-Host ""
Write-Host "Setup complete."
Write-Host "Run app  : $Python run_app.py"
Write-Host "Run tests: `$env:QT_QPA_PLATFORM='offscreen'; $Python -m pytest -q"


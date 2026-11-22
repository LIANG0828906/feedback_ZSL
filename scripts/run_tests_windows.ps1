$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
$env:QT_QPA_PLATFORM = "offscreen"

$Python = Join-Path $Root ".venv\Scripts\python.exe"
if (-not (Test-Path $Python)) {
    throw "Virtual environment missing. Run scripts\setup_windows.ps1 first."
}

& $Python -m pytest -q
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
& $Python scripts\run_demo.py
exit $LASTEXITCODE


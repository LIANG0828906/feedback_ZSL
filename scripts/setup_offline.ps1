param(
    [string]$Wheelhouse = ".\\wheelhouse",
    [string]$Python = "py -3.10"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path $Wheelhouse)) {
    throw "Wheelhouse not found: $Wheelhouse"
}

Invoke-Expression "$Python -m venv .venv"
$venvPython = Join-Path (Get-Location) ".venv\\Scripts\\python.exe"
if (-not (Test-Path $venvPython)) {
    throw "Virtual environment creation failed."
}

& $venvPython -m pip install --no-index --find-links $Wheelhouse numpy PyQt5
& $venvPython -m pip check

Write-Host "Offline environment ready."
Write-Host "Run: .\\.venv\\Scripts\\python.exe run_checks.py"

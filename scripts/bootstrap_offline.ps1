param(
    [string]$Wheelhouse = "wheelhouse",
    [string]$PythonLauncher = "py",
    [string]$PythonVersion = "-3.11"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path ".venv")) {
    & $PythonLauncher $PythonVersion -m venv .venv
}

& ".venv\Scripts\python.exe" -m pip install --no-index `
    --find-links $Wheelhouse -r requirements.txt

Write-Host "Offline environment ready: .venv"


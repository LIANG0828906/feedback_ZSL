param(
    [string]$Wheelhouse = "wheelhouse"
)

$ErrorActionPreference = "Stop"
if (-not (Test-Path $Wheelhouse)) {
    throw "Wheelhouse not found: $Wheelhouse. Run scripts/fetch_wheels.ps1 once on a connected machine."
}

if (-not (Test-Path ".venv")) {
    py -3.11 -m venv .venv
}

& .\.venv\Scripts\python.exe -m pip install `
    --no-index `
    --find-links $Wheelhouse `
    -r requirements-windows.lock

if ($LASTEXITCODE -ne 0) {
    throw "offline dependency installation failed"
}

Write-Host "Dependencies installed offline into .venv"


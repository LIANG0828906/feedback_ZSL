param(
    [string]$Python = "py -3.11",
    [string]$Wheelhouse = "wheelhouse"
)

$ErrorActionPreference = "Stop"
$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

if (-not (Test-Path ".venv")) {
    Invoke-Expression "$Python -m venv .venv"
}

$VenvPython = Join-Path $Root ".venv\Scripts\python.exe"
& $VenvPython -m pip install --no-index --find-links $Wheelhouse -r requirements.txt
& $VenvPython -m mediaforge.samplegen --out samples --seed 20261123
& $VenvPython -m mediaforge selfcheck --samples samples --checkpoint artifacts\checkpoints

Write-Host "Environment ready. Start the workbench with:"
Write-Host "  .venv\Scripts\python.exe run_workbench.py"

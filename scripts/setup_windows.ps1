$ErrorActionPreference = "Stop"

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

if (-not (Test-Path ".venv")) {
    py -3.10 -m venv .venv
}

& ".\.venv\Scripts\python.exe" -m pip install --upgrade pip
& ".\.venv\Scripts\python.exe" -m pip install -r requirements-cpu.txt
& ".\.venv\Scripts\python.exe" scripts\check_torch.py

Write-Host "Environment ready. Run: .\scripts\run_demo.ps1"

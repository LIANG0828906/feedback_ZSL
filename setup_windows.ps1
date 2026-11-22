$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

py -3 -m venv .venv
.\.venv\Scripts\python.exe -m pip install --upgrade pip
.\.venv\Scripts\python.exe -m pip install -r requirements.txt
.\.venv\Scripts\python.exe -m scworkbench generate --out samples
Write-Host "Environment ready. Launch with: .\.venv\Scripts\python.exe run_workbench.py"

$ErrorActionPreference = "Stop"

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

if (-not (Test-Path ".venv\Scripts\python.exe")) {
    throw "Missing .venv. Run .\scripts\setup_windows.ps1 first."
}

$env:OMP_NUM_THREADS = "1"
$env:MKL_NUM_THREADS = "1"
& ".\.venv\Scripts\python.exe" -m mlp_ps.demo --config config\demo.json --repeat 1

$ErrorActionPreference = "Stop"

$Python = Get-Command py -ErrorAction SilentlyContinue
if (-not $Python) {
    throw "Python launcher 'py' was not found. Install Python 3.11 from python.org first."
}

py -3.11 -m venv .venv
& .\.venv\Scripts\python.exe -m pip install --upgrade pip
& .\.venv\Scripts\python.exe -m pip install -r requirements.txt
& .\.venv\Scripts\python.exe -m pip install -e .
& .\.venv\Scripts\python.exe -m transparent_pipeline.cli doctor
& .\.venv\Scripts\python.exe -m transparent_pipeline.cli generate --out samples --seed 20261123

Write-Host ""
Write-Host "Environment ready. Next commands:"
Write-Host "  .\.venv\Scripts\python.exe -m pytest -q"
Write-Host "  .\.venv\Scripts\python.exe -m transparent_pipeline.cli verify"
Write-Host "  .\.venv\Scripts\python.exe -m transparent_pipeline.cli serve"


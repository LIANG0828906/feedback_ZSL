$ErrorActionPreference = "Stop"

Write-Host "SpectraForge Windows bootstrap" -ForegroundColor Cyan
if (Get-Command py -ErrorAction SilentlyContinue) {
    py -3 -m venv .venv
} else {
    python -m venv .venv
}
& .\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
python -m pip install -r requirements.txt
python generate_samples.py --out samples
python -m spectraforge verify --workspace runtime\verify
Write-Host "Ready: python run_app.py" -ForegroundColor Green

# Windows 一键准备与验收脚本（PowerShell）
param(
    [string]$Python = "py -3.12"
)

if (-not (Test-Path .venv)) {
    Invoke-Expression "$Python -m venv .venv"
}
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
python -m audiorest.generate_samples samples
python -m scripts.run_demo --samples samples --workdir workdir
python -m unittest discover -s tests -v

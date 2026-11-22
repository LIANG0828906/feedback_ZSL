@echo off
setlocal
cd /d "%~dp0"
if not exist .venv\Scripts\activate.bat (
  echo [ERROR] .venv is missing. Run setup_windows.bat first.
  exit /b 1
)
call .venv\Scripts\activate.bat

if not exist artifacts\samples\manifest.json python -m imq_workbench generate --output-dir artifacts\samples --preset full
python -m unittest discover -v
if errorlevel 1 exit /b 1
python -m imq_workbench verify --output artifacts\verification_cli.json

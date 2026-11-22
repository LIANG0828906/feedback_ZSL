@echo off
setlocal
cd /d "%~dp0"

where py >nul 2>nul
if errorlevel 1 (
  echo [ERROR] Python launcher "py" was not found. Install Python 3.11 first.
  exit /b 1
)

if not exist .venv (
  py -3.11 -m venv .venv
  if errorlevel 1 exit /b 1
)

call .venv\Scripts\activate.bat
python -m pip --version >nul 2>nul
if errorlevel 1 python -m ensurepip --upgrade

if exist wheelhouse (
  echo Installing pinned dependencies from local wheelhouse...
  python -m pip install --no-index --find-links wheelhouse -r requirements.txt
) else (
  echo wheelhouse not found; installing pinned dependencies from the configured pip index...
  python -m pip install --upgrade pip
  python -m pip install -r requirements.txt
)
if errorlevel 1 exit /b 1

if not exist artifacts\samples\manifest.json (
  python -m imq_workbench generate --output-dir artifacts\samples --preset full
)

python -m imq_workbench check

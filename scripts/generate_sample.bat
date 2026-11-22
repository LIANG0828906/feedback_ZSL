@echo off
setlocal
cd /d "%~dp0\.."
if not exist ".venv\Scripts\python.exe" (
  echo Please create .venv and install requirements.txt first.
  exit /b 2
)
".venv\Scripts\python.exe" scripts\generate_sample.py --out artifacts\sample
endlocal

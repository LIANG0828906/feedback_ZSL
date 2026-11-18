@echo off
setlocal
cd /d "%~dp0\.."
if not exist ".venv\Scripts\python.exe" (
  echo Missing .venv. Run scripts\setup_windows.ps1 first.
  exit /b 2
)
set OMP_NUM_THREADS=1
set MKL_NUM_THREADS=1
".venv\Scripts\python.exe" -m mlp_ps.demo --config config\demo.json --repeat 1
exit /b %ERRORLEVEL%

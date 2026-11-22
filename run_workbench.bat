@echo off
setlocal
cd /d "%~dp0"
if not exist .venv\Scripts\activate.bat (
  echo [ERROR] .venv is missing. Run setup_windows.bat first.
  exit /b 1
)
call .venv\Scripts\activate.bat

python -m imq_workbench app --samples-dir artifacts\samples --checkpoint artifacts\state\checkpoints.sqlite --runs-dir artifacts\runs
if errorlevel 1 (
  echo.
  echo [WARN] GUI could not start. Falling back to the command-line entry point.
  python -m imq_workbench run --samples-dir artifacts\samples --sample gradient_gray_512 --mode resume
)

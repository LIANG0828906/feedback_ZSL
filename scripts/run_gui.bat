@echo off
setlocal
cd /d "%~dp0\.."
if not exist ".venv\Scripts\python.exe" (
  echo Please create .venv and install requirements-ui.txt first.
  exit /b 2
)
".venv\Scripts\python.exe" -m terrain_pipeline.cli gui "%~1" --cache "%~2"
endlocal


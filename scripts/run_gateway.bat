@echo off
setlocal
cd /d "%~dp0.."
if not exist ".venv\Scripts\python.exe" (
  echo Run scripts\setup.bat first.
  exit /b 1
)
if not exist "generated\models\v3\manifest.json" (
  echo Generated models are missing. Run scripts\generate_assets.bat first.
  exit /b 1
)
".venv\Scripts\python.exe" -m rec_gateway --config config\gateway.json %*

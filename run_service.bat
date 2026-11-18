@echo off
setlocal
cd /d "%~dp0"
if not exist metadata.db (
  python generate_metadata.py --db metadata.db
  if errorlevel 1 exit /b 1
)
python lineage_service.py --db metadata.db --host 127.0.0.1 --port 8008

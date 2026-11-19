@echo off
setlocal
cd /d "%~dp0"
python -m poweraudit simulate --config config\local.json
endlocal

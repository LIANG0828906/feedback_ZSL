@echo off
setlocal
cd /d "%~dp0"
python -m poweraudit serve --config config\local.json --with-simulator
endlocal

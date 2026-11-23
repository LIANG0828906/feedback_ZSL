@echo off
setlocal
cd /d "%~dp0"
python -m panoworkbench gui --dataset "%~1"
endlocal

@echo off
setlocal
cd /d "%~dp0"
if exist ".deps" set "PYTHONPATH=%CD%\.deps;%CD%;%PYTHONPATH%"
python -m panoworkbench gui --dataset "%~1"
endlocal

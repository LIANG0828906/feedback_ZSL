@echo off
REM Deterministic local environment for the rose window validator.
REM Requires Python 3.11 or 3.12 on PATH. No network use beyond pip wheels.
setlocal

py -3.12 -m venv .venv
if errorlevel 1 (
    py -3.11 -m venv .venv
)
if errorlevel 1 exit /b 1

call .venv\Scripts\activate.bat
python -m pip install --upgrade pip
python -m pip install -r requirements.txt
endlocal

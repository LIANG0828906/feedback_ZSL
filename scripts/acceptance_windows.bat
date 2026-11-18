@echo off
REM End-to-end deterministic acceptance run on Windows.
setlocal
call .venv\Scripts\activate.bat
set PYTHONPATH=src

if exist build_demo rmdir /s /q build_demo
mkdir build_demo

python -m rosewin.cli generate-scene --output build_demo\scene.json || exit /b 1
python -m rosewin.cli render --scene build_demo\scene.json --output-dir build_demo\render || exit /b 1
python -m rosewin.cli verify-recovery --scene build_demo\scene.json --workspace build_demo\recovery_check --interrupt-after 2 || exit /b 1
python -m unittest discover -s tests || exit /b 1
endlocal

@echo off
setlocal
python -m haptic_studio demo --out artifacts\demo --sample dense_9
if errorlevel 1 exit /b %errorlevel%
python -m haptic_studio crash-recover --out artifacts\crash
if errorlevel 1 exit /b %errorlevel%
endlocal


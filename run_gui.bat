@echo off
setlocal
if exist ".venv\Scripts\python.exe" (
  ".venv\Scripts\python.exe" -m edgeforge gui %*
) else (
  python -m edgeforge gui %*
)
exit /b %ERRORLEVEL%

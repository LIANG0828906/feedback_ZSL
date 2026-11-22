@echo off
setlocal
if exist ".venv\Scripts\python.exe" (
  ".venv\Scripts\python.exe" -m edgeforge %*
) else (
  python -m edgeforge %*
)
exit /b %ERRORLEVEL%

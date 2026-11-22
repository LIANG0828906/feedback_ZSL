@echo off
setlocal
if exist ".venv\Scripts\python.exe" (
  ".venv\Scripts\python.exe" -m unittest discover -v
) else (
  python -m unittest discover -v
)
exit /b %ERRORLEVEL%

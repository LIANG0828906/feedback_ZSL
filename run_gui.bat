@echo off
setlocal
cd /d "%~dp0"
if exist ".venv\Scripts\python.exe" (
  ".venv\Scripts\python.exe" -m brecon gui --workspace artifacts\demo %*
) else (
  py -3.11 -m brecon gui --workspace artifacts\demo %*
)
endlocal


@echo off
python "%~dp0run_regression.py" %*
exit /b %ERRORLEVEL%


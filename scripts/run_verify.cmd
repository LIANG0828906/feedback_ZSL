@echo off
setlocal
call "%~dp0common.cmd" || exit /b 1
"%ROOT%\bin\verify.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\realtime" -quality %QUALITY%
exit /b %errorlevel%

@echo off
setlocal
call "%~dp0common.cmd" || exit /b 1
"%ROOT%\bin\compare.exe" -a "%STORE_ROOT%\realtime" -b "%STORE_ROOT%\rebuild" || exit /b 1
"%ROOT%\bin\compare.exe" -a "%STORE_ROOT%\realtime" -b "%STORE_ROOT%\resume"
exit /b %errorlevel%

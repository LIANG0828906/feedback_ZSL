@echo off
setlocal
call "%~dp0common.cmd" || exit /b 1
if not exist "%DATA_DIR%\manifest.json" "%ROOT%\bin\generator.exe" -data "%DATA_DIR%"
"%ROOT%\bin\pipeline.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\rebuild" -quality %QUALITY% -mode rebuild -workers 1 -window %WINDOW% -stride %STRIDE% -halo %HALO%
exit /b %errorlevel%

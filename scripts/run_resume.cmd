@echo off
setlocal
call "%~dp0common.cmd" || exit /b 1
if not exist "%DATA_DIR%\manifest.json" "%ROOT%\bin\generator.exe" -data "%DATA_DIR%"
"%ROOT%\bin\pipeline.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\resume" -quality %QUALITY% -mode resume -workers %WORKERS% -window %WINDOW% -stride %STRIDE% -halo %HALO%
exit /b %errorlevel%

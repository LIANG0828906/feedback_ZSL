@echo off
setlocal
call "%~dp0common.cmd" || exit /b 1
if not exist "%DATA_DIR%\manifest.json" "%ROOT%\bin\generator.exe" -data "%DATA_DIR%"

echo == realtime (parallel) ==
"%ROOT%\bin\pipeline.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\realtime" -quality %QUALITY% -mode realtime -workers %WORKERS% -window %WINDOW% -stride %STRIDE% -halo %HALO% || exit /b 1
echo == full rebuild (single thread) ==
"%ROOT%\bin\pipeline.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\rebuild" -quality %QUALITY% -mode rebuild -workers 1 -window %WINDOW% -stride %STRIDE% -halo %HALO% || exit /b 1
echo == crash injection then resume ==
"%ROOT%\bin\pipeline.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\resume" -quality %QUALITY% -mode rebuild -workers %WORKERS% -crash-after 40
"%ROOT%\bin\pipeline.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\resume" -quality %QUALITY% -mode resume -workers %WORKERS% -window %WINDOW% -stride %STRIDE% -halo %HALO% || exit /b 1
echo == three-path comparison ==
"%ROOT%\bin\compare.exe" -a "%STORE_ROOT%\realtime" -b "%STORE_ROOT%\rebuild" || exit /b 1
"%ROOT%\bin\compare.exe" -a "%STORE_ROOT%\realtime" -b "%STORE_ROOT%\resume" || exit /b 1
echo == independent verification ==
"%ROOT%\bin\verify.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\realtime" -quality %QUALITY% || exit /b 1
"%ROOT%\bin\verify.exe" -data "%DATA_DIR%" -store "%STORE_ROOT%\resume" -quality %QUALITY%
exit /b %errorlevel%

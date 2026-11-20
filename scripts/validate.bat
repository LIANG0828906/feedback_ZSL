@echo off
setlocal
set ROOT=%~dp0..
set BUILD_DIR=%ROOT%\build
set SAMPLES=%ROOT%\out\validation-samples
set RUN_DIR=%ROOT%\out\validation-run

cmake -S "%ROOT%" -B "%BUILD_DIR%" -A x64
if errorlevel 1 exit /b 1
cmake --build "%BUILD_DIR%" --config Release
if errorlevel 1 exit /b 1
ctest --test-dir "%BUILD_DIR%" -C Release --output-on-failure
if errorlevel 1 exit /b 1

"%BUILD_DIR%\Release\battle_cli.exe" generate --out "%SAMPLES%" --seed 20261121 --cases 3
if errorlevel 1 exit /b 1

for %%C in (case-000 case-001 case-002) do (
    "%BUILD_DIR%\Release\battle_cli.exe" run ^
        --scene "%SAMPLES%\%%C\scene.bin" ^
        --ops "%SAMPLES%\%%C\ops.txt" ^
        --trace "%RUN_DIR%\%%C\trace.log" ^
        --snapshots "%RUN_DIR%\%%C\snapshots"
    if errorlevel 1 exit /b 1
    fc /b "%SAMPLES%\%%C\baseline\trace.log" "%RUN_DIR%\%%C\trace.log"
    if errorlevel 1 exit /b 1
    "%BUILD_DIR%\Release\battle_cli.exe" replay ^
        --trace "%RUN_DIR%\%%C\trace.log" ^
        --snapshots "%RUN_DIR%\%%C\snapshots"
    if errorlevel 1 exit /b 1
)

echo validation passed: all baseline traces are byte-identical
exit /b 0

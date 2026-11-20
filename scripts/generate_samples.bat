@echo off
setlocal
set ROOT=%~dp0..
set BUILD_DIR=%ROOT%\build
set OUT_DIR=%ROOT%\out\samples
if not "%~1"=="" set OUT_DIR=%~1
if "%SEED%"=="" set SEED=20261121
if "%CASES%"=="" set CASES=3

cmake -S "%ROOT%" -B "%BUILD_DIR%" -DCMAKE_BUILD_TYPE=Release
if errorlevel 1 exit /b 1
cmake --build "%BUILD_DIR%" --config Release
if errorlevel 1 exit /b 1
"%BUILD_DIR%\Release\battle_cli.exe" generate --out "%OUT_DIR%" --seed %SEED% --cases %CASES%
exit /b %errorlevel%


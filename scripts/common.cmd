@echo off
setlocal
set ROOT=%~dp0..
if "%DATA_DIR%"=="" set DATA_DIR=%ROOT%\data
if "%STORE_ROOT%"=="" set STORE_ROOT=%ROOT%\store
if "%QUALITY%"=="" set QUALITY=70
if "%WORKERS%"=="" set WORKERS=2
if "%WINDOW%"=="" set WINDOW=7
if "%STRIDE%"=="" set STRIDE=2
if "%HALO%"=="" set HALO=8

pushd "%ROOT%"
go build -o bin\generator.exe .\cmd\generator || goto :fail
go build -o bin\pipeline.exe .\cmd\pipeline || goto :fail
go build -o bin\compare.exe .\cmd\compare || goto :fail
go build -o bin\verify.exe .\cmd\verify || goto :fail
popd
goto :eof

:fail
popd
exit /b 1

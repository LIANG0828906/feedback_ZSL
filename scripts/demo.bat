@echo off
setlocal
cd /d "%~dp0\.."

call build.bat -q package
if errorlevel 1 exit /b %ERRORLEVEL%

set "JAVA_CMD=java"
if defined JAVA_HOME if exist "%JAVA_HOME%\bin\java.exe" set "JAVA_CMD=%JAVA_HOME%\bin\java.exe"
if exist ".tools\jdk17\bin\java.exe" if not defined JAVA_HOME set "JAVA_CMD=%CD%\.tools\jdk17\bin\java.exe"

"%JAVA_CMD%" -cp "target\edgekv-index.jar" io.edgekv.demo.DemoRunner --root "%CD%"
exit /b %ERRORLEVEL%

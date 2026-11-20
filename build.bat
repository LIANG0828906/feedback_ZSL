@echo off
setlocal
cd /d "%~dp0"

if not defined JAVA_HOME if exist ".tools\jdk17\bin\javac.exe" set "JAVA_HOME=%CD%\.tools\jdk17"
if defined JAVA_HOME set "PATH=%JAVA_HOME%\bin;%PATH%"

where javac >nul 2>nul
if errorlevel 1 (
  echo JAVA_HOME must point to a Java 17+ JDK with javac.
  exit /b 1
)

set "MVN=mvn"
if exist ".tools\maven\bin\mvn.cmd" set "MVN=%CD%\.tools\maven\bin\mvn.cmd"

if exist ".m2repo\org\junit" (
  call "%MVN%" -o %*
) else (
  call "%MVN%" %*
)
exit /b %ERRORLEVEL%

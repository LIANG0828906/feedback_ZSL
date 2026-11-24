@echo off
setlocal
echo Checking Go toolchain...
where go
if errorlevel 1 (
  echo Go was not found in PATH. Install Go 1.22 or newer and reopen Command Prompt.
  exit /b 1
)
go version
echo CGO_ENABLED will be forced to 0 because modernc.org/sqlite is pure Go.

@echo off
setlocal
set CGO_ENABLED=0
go test ./...
exit /b %errorlevel%

@echo off
setlocal
set CGO_ENABLED=0
cd /d "%~dp0\.."

go version || exit /b 1
go mod download || exit /b 1
go run ./cmd/workbench generate -data data || exit /b 1
go run ./cmd/workbench recompute -data data || exit /b 1
go run ./cmd/verify -data data || exit /b 1
start "" http://127.0.0.1:8080
go run ./cmd/workbench -addr 127.0.0.1:8080 -data data

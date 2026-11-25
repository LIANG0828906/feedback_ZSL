@echo off
setlocal
cd /d "%~dp0.."

echo [1/3] go version
go version
if errorlevel 1 exit /b 1

echo [2/3] go test ./...
go test ./...
if errorlevel 1 exit /b 1

echo [3/3] deterministic sample verification
go run ./cmd/terrainpatch generate -dir samples\local -seed 42
if errorlevel 1 exit /b 1
go run ./cmd/terrainpatch verify -samples samples\local -db data\verify.db
if errorlevel 1 exit /b 1

echo Verification passed.

$ErrorActionPreference = "Stop"

Write-Host "Checking Go..."
go version

go env -w GOOS=windows
go env -w CGO_ENABLED=1

Write-Host "Installing Wails CLI..."
go install github.com/wailsapp/wails/v2/cmd/wails@latest

$gopath = go env GOPATH
$env:Path = "$env:Path;$gopath\bin"

Write-Host "Installing pure-Go SQLite driver..."
go get modernc.org/sqlite
go mod tidy

Write-Host "Building StageCue Workbench..."
wails build

Write-Host "Build complete. See build/bin/."

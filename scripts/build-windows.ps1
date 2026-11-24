$ErrorActionPreference = "Stop"
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"

New-Item -ItemType Directory -Force -Path bin | Out-Null
go build -o bin/IconBench.exe ./cmd/workbench
go build -o bin/verify.exe ./cmd/verify

Write-Host "Built:"
Get-ChildItem bin\*.exe | Format-Table Name, Length

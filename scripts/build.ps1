param(
    [string]$OutputDir = (Join-Path $PSScriptRoot "..\bin")
)

$ErrorActionPreference = "Stop"
$repo = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $repo
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

go version
go mod download

$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -o (Join-Path $OutputDir "s3gw.exe") .\cmd\s3gw
go build -trimpath -o (Join-Path $OutputDir "samplegen.exe") .\cmd\samplegen
go build -trimpath -o (Join-Path $OutputDir "s3gwbench.exe") .\cmd\s3gwbench

Write-Host "Built Windows binaries in $OutputDir"

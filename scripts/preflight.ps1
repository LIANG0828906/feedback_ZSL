[CmdletBinding()]
param(
    [string]$DataDir = "data\checkpoints"
)

$ErrorActionPreference = "Stop"

Write-Host "== Go version =="
go version

$goos = (go env GOOS).Trim()
Write-Host "go env GOOS = $goos"
if ($env:OS -eq "Windows_NT" -and $goos -ne "windows") {
    throw "GOOS must be windows on a native Windows toolchain"
}

New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
$writeCheck = Join-Path $DataDir ".write-check"
"ok" | Set-Content -Path $writeCheck -Encoding ascii
Remove-Item $writeCheck
Write-Host "writable persistent directory: $DataDir"

Write-Host "== go build ./... =="
go build ./...

Write-Host "== go test ./... =="
go test ./...

Write-Host "PREFLIGHT_OK"

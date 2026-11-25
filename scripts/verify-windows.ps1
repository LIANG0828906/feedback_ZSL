# Windows PowerShell verification entry point.
# Requires Go 1.22+ on PATH. Run from the module root:
#   powershell -ExecutionPolicy Bypass -File .\scripts\verify-windows.ps1

$ErrorActionPreference = "Stop"

Write-Host "== Go environment =="
go version
$goos = (go env GOOS).Trim()
if ($goos -ne "windows") {
    throw "go env GOOS is '$goos'; run this script in native Windows or set 'go env -w GOOS=windows' before building."
}
go env GOOS GOARCH CGO_ENABLED

Write-Host "== Module =="
if (-not (Test-Path .\go.mod)) {
    go mod init battlekernel
}
go get modernc.org/sqlite@v1.29.10
go mod tidy

Write-Host "== Vet and tests =="
go vet ./...
go test ./...

Write-Host "== Build pure-Go Windows binaries =="
$env:CGO_ENABLED = "0"
New-Item -ItemType Directory -Force .\out\windows | Out-Null
go build -o .\out\windows\battlekernel.exe .\cmd\battlekernel
go build -o .\out\windows\review.exe .\cmd\review

Write-Host "== Generate samples and verify three paths =="
.\out\windows\battlekernel.exe verify --out .\out

Write-Host "== Independent reviewer =="
.\out\windows\review.exe --db .\out\data\battle-001.realtime.db --truth .\out\truth-fingerprints.json --project-id battle-001

Write-Host "== Local page =="
Write-Host "Run: .\out\windows\battlekernel.exe serve --addr 127.0.0.1:8080"

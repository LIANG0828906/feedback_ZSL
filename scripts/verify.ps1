param(
    [string]$OutputDir = "artifacts\demo"
)

$ErrorActionPreference = "Stop"

$env:GOMODCACHE = Join-Path $PWD ".gomodcache"
go env -w GOMODCACHE="$env:GOMODCACHE"

go version
go test ./...
go build -o .\bin\audioeval.exe .\cmd\audioeval
.\bin\audioeval.exe demo --dir $OutputDir
.\bin\audioeval.exe verify `
    --sample-dir (Join-Path $OutputDir "samples\q3") `
    --run-dir (Join-Path $OutputDir "runs\resume")

Write-Host "Validation artifacts: $OutputDir\validation.json"

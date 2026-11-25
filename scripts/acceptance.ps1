param(
  [string]$Out = "artifacts/local-acceptance"
)

$ErrorActionPreference = "Stop"
$env:GOTOOLCHAIN = "local"

Write-Host "== gofmt =="
$files = Get-ChildItem -Recurse -Filter *.go | Where-Object { $_.FullName -notmatch '\\wailsapp\\' }
& gofmt -w $files.FullName

Write-Host "== go vet =="
go vet ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "== go test =="
go test -count=1 ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "== go build =="
go build ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

New-Item -ItemType Directory -Force -Path $Out | Out-Null
go run ./cmd/iconcut generate -out (Join-Path $Out "samples")
go run ./cmd/iconcut run `
  -bundle (Join-Path $Out "samples/icon-192") `
  -db (Join-Path $Out "icon-192.db") `
  -out (Join-Path $Out "out")
go run ./cmd/verifycut -db (Join-Path $Out "icon-192.db")

if (Test-Path "artifacts/acceptance/icon-192") {
  go run ./cmd/iconcut run `
    -bundle "artifacts/acceptance/icon-192" `
    -db (Join-Path $Out "supplied-icon-192.db") `
    -out (Join-Path $Out "supplied-out")
  go run ./cmd/verifycut -db (Join-Path $Out "supplied-icon-192.db")
}

Write-Host "ACCEPTANCE_OK"

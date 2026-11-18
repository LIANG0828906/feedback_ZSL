$ErrorActionPreference = "Stop"

$env:GOMAXPROCS = "2"
go test ./...
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

go run ./cmd/validate `
  -samples samples/gateways.json `
  -out artifacts/validation-report.json `
  -work-dir artifacts/validation-run `
  -gomaxprocs 2 `
  -memory-limit-mib 1250
exit $LASTEXITCODE

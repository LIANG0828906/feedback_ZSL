$ErrorActionPreference = "Stop"

# modernc.org/sqlite is pure Go; do not require gcc on Windows.
$env:CGO_ENABLED = "0"
# Dependencies are expected in the local module cache after `go mod download`.
$env:GOPROXY = "off"

$DbDir = Join-Path $env:LOCALAPPDATA "RecVerify"
New-Item -ItemType Directory -Force -Path $DbDir | Out-Null
New-Item -ItemType Directory -Force -Path "artifacts" | Out-Null

Write-Host "== Go environment =="
go version
go env GOMODCACHE

Write-Host "== Generate deterministic local samples =="
go run ./cmd/gensample -out data\sample

Write-Host "== Run tests =="
go test ./...

Write-Host "== Three-path and parameter invalidation experiment =="
go run ./cmd/verify experiment `
  -data data\sample `
  -out artifacts\experiment-report.json `
  -db-dir artifacts\experiment-dbs `
  -parallelism 4 `
  -buffer-pixels-per-thread 24576 `
  -block-size 16 `
  -threshold 0.55 `
  -cancel-after 2

foreach ($Clip in @("noise", "watermark", "clean")) {
  $DbPath = Join-Path $DbDir "$Clip.db"
  $VerifyArgs = @(
    "run",
    "-sample", "data\sample\$Clip",
    "-db", $DbPath,
    "-report", "artifacts\report-$Clip.json",
    "-parallelism", "4",
    "-buffer-pixels-per-thread", "24576",
    "-block-size", "16",
    "-window", "3",
    "-threshold", "0.55"
  )
  if ($Clip -eq "noise") {
    $VerifyArgs += @("-inject-failure-once", "8:noise_variance", "-retry-failed=true")
  }
  go run ./cmd/verify @VerifyArgs

  go run ./cmd/review `
    -db $DbPath `
    -truth "data\sample\$Clip\truth.json" `
    -out "artifacts\review-$Clip.json"
}

Write-Host "Done. Persistent SQLite directory: $DbDir"

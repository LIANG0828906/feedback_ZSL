# End-to-end local example for Windows: sample generation, edit session run, tests.
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

Write-Host "== go version =="
go version

Write-Host "== generate deterministic sample =="
go run ./cmd/levelsession sample -scenario basic -out samples/basic

Write-Host "== run editing session (incremental + preview/finalize + three-path compare) =="
go run ./cmd/levelsession run -sample samples/basic -store artifacts/store -report artifacts/report

Write-Host "== resume from committed checkpoints =="
go run ./cmd/levelsession resume -sample samples/basic -store artifacts/store

Write-Host "== generate anomaly scenario samples =="
foreach ($s in @("budget", "conn", "conn-fail", "hist")) {
  go run ./cmd/levelsession sample -scenario $s -out "samples/$s" | Out-Null
  Write-Host "generated samples/$s"
}

Write-Host "== unit tests (error codes, consistency, recovery) =="
go test ./...

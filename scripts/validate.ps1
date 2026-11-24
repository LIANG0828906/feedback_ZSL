$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

$env:CGO_ENABLED = "0"
go vet ./...
go test -count=1 ./...
go build -o artifacts/integrity-diagnose.exe ./cmd/diagnose

.\artifacts\integrity-diagnose.exe gen -sample samples\default
.\artifacts\integrity-diagnose.exe crosscheck -sample samples\default -artifacts artifacts -json artifacts\crosscheck.json
.\artifacts\integrity-diagnose.exe review -db artifacts\recovery-checkpoint.db -fingerprint samples\default\truth.fingerprint.json -json artifacts\review.json

Write-Host "Validation passed. Start UI with:"
Write-Host ".\artifacts\integrity-diagnose.exe serve -sample samples\default -artifacts artifacts -addr 127.0.0.1:8088"

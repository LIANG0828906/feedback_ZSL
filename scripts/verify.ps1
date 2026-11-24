[CmdletBinding()]
param(
    [string]$SampleDir = "samples/default",
    [int64]$Seed = 241125
)

$ErrorActionPreference = "Stop"

go version
if ($LASTEXITCODE -ne 0) { throw "Go 1.22+ is required" }

go run ./cmd/editorpick generate --dir $SampleDir --seed $Seed
if ($LASTEXITCODE -ne 0) { throw "sample generation failed" }

go run ./cmd/editorpick verify --dir $SampleDir --report (Join-Path $SampleDir "verification-report.json")
if ($LASTEXITCODE -ne 0) { throw "three-path verification failed" }

go test ./... -count=1
if ($LASTEXITCODE -ne 0) { throw "tests failed" }

Write-Host "Verification completed successfully."

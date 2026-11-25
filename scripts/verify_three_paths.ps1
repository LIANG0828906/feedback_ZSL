$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
New-Item -ItemType Directory -Force -Path bin, work | Out-Null
go build -o bin/uvtool.exe ./cmd/uvtool

& bin/uvtool.exe verify -sample samples/sample_01 -edits configs/fixed_edits.json `
  -workdir work/verify-small -cancel-step 2 -cancel-after-stages 1 -workers 2 -buffer-mib 128 `
  | Out-File -Encoding utf8 work/verify-small.json
if ($LASTEXITCODE -ne 0) { throw "small sample verification failed" }

& bin/uvtool.exe verify -sample samples/sample_02 -edits configs/fixed_edits.json `
  -workdir work/verify-large -cancel-step 2 -cancel-after-stages 1 -workers 4 -buffer-mib 128 `
  | Out-File -Encoding utf8 work/verify-large.json
if ($LASTEXITCODE -ne 0) { throw "large sample verification failed" }

Write-Host "three-path verification passed for both committed samples"

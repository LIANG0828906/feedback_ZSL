$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
$work = "work/cancel-restart"
if (Test-Path $work) { Remove-Item -Recurse -Force $work }
New-Item -ItemType Directory -Force -Path bin, $work | Out-Null
go build -o bin/uvtool.exe ./cmd/uvtool

$db = "$work/cache.db"
$recovery = "$work/recovery"
$full = "$work/full"

& bin/uvtool.exe run -sample samples/sample_01 -params configs/default_params.json `
  -db $db -out "$work/initial" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "initial run failed" }

& bin/uvtool.exe run -sample samples/sample_01 -params configs/edit_u_gain.json `
  -db $db -out $recovery -cancel-after-stages 1 | Out-Null
if ($LASTEXITCODE -ne 7) {
  throw "expected graceful cancel exit 7, got $LASTEXITCODE"
}
$partial = Get-Content "$recovery/partial_run.json" -Raw
if ($partial -notmatch '"complete"\s*:\s*false') {
  throw "canceled run incorrectly claimed a complete report"
}

& bin/uvtool.exe run -sample samples/sample_01 -params configs/edit_u_gain.json `
  -db $db -out $recovery | Out-Null
if ($LASTEXITCODE -ne 0) { throw "restart failed" }

& bin/uvtool.exe run -sample samples/sample_01 -params configs/edit_u_gain.json `
  -db "$work/full.db" -out $full | Out-Null
if ($LASTEXITCODE -ne 0) { throw "full recompute failed" }

$a = (Get-FileHash "$recovery/enhanced.yuv444p" -Algorithm SHA256).Hash
$b = (Get-FileHash "$full/enhanced.yuv444p" -Algorithm SHA256).Hash
if ($a -ne $b) { throw "restart and full enhanced images differ" }
$a = (Get-FileHash "$recovery/difference.pgm" -Algorithm SHA256).Hash
$b = (Get-FileHash "$full/difference.pgm" -Algorithm SHA256).Hash
if ($a -ne $b) { throw "restart and full heatmaps differ" }
Write-Host "cancel persisted a partial snapshot; restart matched full recomputation byte-for-byte"

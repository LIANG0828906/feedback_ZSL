param(
    [string]$Addr = "127.0.0.1:18080",
    [string]$Sample = "samples/terrain-seed-42.json",
    [string]$Db = "artifacts/terrain-checkpoint.db"
)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
$env:CGO_ENABLED = "0"
if (-not $env:GOFLAGS) { $env:GOFLAGS = "-mod=vendor" }
& "$PSScriptRoot/Generate-Sample.ps1" -Out $Sample
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Db) | Out-Null
go run ./cmd/terrain-server -addr $Addr -sample $Sample -db $Db

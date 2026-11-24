param(
    [int]$Seed = 42,
    [string]$Out = "samples/terrain-seed-42.json"
)

$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
$env:CGO_ENABLED = "0"
if (-not $env:GOFLAGS) { $env:GOFLAGS = "-mod=vendor" }
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Out) | Out-Null
go run ./cmd/terrain-gen -seed $Seed -out $Out

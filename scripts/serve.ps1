$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")
if (-not (Test-Path "samples/local/project.json")) {
  go run ./cmd/terrainpatch generate -dir samples/local -seed 42
}
New-Item -ItemType Directory -Force data | Out-Null
go run ./cmd/terrainpatch serve -addr ":8080" -samples "samples/local" -db "data/terrain.db" -workers 2 -vertex-buffer 4096 -memory-mb 16

@echo off
setlocal
cd /d "%~dp0.."
if not exist samples\local\project.json (
  go run ./cmd/terrainpatch generate -dir samples\local -seed 42
)
if not exist data mkdir data
go run ./cmd/terrainpatch serve -addr :8080 -samples samples\local -db data\terrain.db -workers 2 -vertex-buffer 4096 -memory-mb 16

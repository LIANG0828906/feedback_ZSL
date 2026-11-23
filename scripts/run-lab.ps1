$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
if ($env:CGO_ENABLED -ne "1") { $env:CGO_ENABLED = "1" }
go run ./cmd/ocrlab -addr 127.0.0.1:8123 -samples samples -db data/workbench.db

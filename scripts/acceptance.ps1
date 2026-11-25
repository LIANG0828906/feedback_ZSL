$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

go version
go test ./... -count=1
go run ./cmd/mocapbench generate -out samples
go run ./cmd/mocapbench validate -samples samples

foreach ($clip in @("walk_cycle", "brisk_walk", "idle_shift")) {
  foreach ($target in @("target_hero_tall", "target_compact")) {
    go run ./cmd/mocapbench verify -samples samples -clip $clip -target $target -workers 3 -buffer 4 -out "reports/$clip`_$target.json"
  }
}

go build -o bin/mocapbench.exe ./cmd/mocapbench
Write-Host "ACCEPTANCE OK"

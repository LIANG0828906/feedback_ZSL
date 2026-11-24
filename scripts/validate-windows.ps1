$ErrorActionPreference = "Stop"

Write-Host "== go test =="
go test ./... -count=1

Write-Host "== go vet =="
go vet ./...

Write-Host "== pure-Go build =="
$env:CGO_ENABLED = "0"
go build ./...
go build -o sample-pipeline.exe ./cmd/sample-pipeline
go build -o verify-result.exe ./cmd/verify-result
go build -o gen-samples.exe ./cmd/gen-samples

Write-Host "== generate and verify local samples =="
.\gen-samples.exe -out samples -seed 20261125
foreach ($name in @("blocks420", "rampcolor444", "mixed422")) {
  .\sample-pipeline.exe run --project "samples\$name\project.json" --db "$name.db" --out "$name-out" --workers 4 --buffer 2 --chunk-size 8 --fresh
  .\verify-result.exe --project "samples\$name\project.json" --truth "samples\$name\truth.json" --db "$name.db"
}

Write-Host "WINDOWS_VALIDATE_OK"

$ErrorActionPreference = "Stop"

# modernc.org/sqlite is pure Go, so a broken or missing C compiler does not
# block `go build` or `go test`.
$env:CGO_ENABLED = "0"

Write-Host "== go env =="
go env GOVERSION CGO_ENABLED GOOS GOARCH

Write-Host "== go build =="
go build ./...

Write-Host "== go vet =="
go vet ./...

Write-Host "== go test =="
go test -count=1 ./...

Write-Host "== deterministic sample =="
go run ./cmd/terrain-occlusion generate -out samples/project.json

Write-Host "== three-path verification =="
go run ./cmd/terrain-occlusion verify -in samples/project.json -artifacts artifacts

Write-Host "OK: build, vet, tests, sample and three-path verification passed."

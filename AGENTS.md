# Repository Guidance

- Use only the Go standard library and files in this repository.
- Keep all retarget arithmetic after schema conversion integer-based; do not add floating-point accumulation.
- Preserve deterministic output for the fixed seed.
- Validate with `gofmt`, `go test ./...`, and `go run ./cmd/bonerig`.

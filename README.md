# terrain-bake

Deterministic terrain/geospatial render-batch workbench for open-world chunks.
It loads a local terrain project, validates layered material weights and
passability masks, bakes vertex/normal/material/scatter buffers, enforces seam
constraints, persists a versioned cache snapshot, incrementally rebakes a
precise dependency component, and serves a standard-library-only diagnostic
page.

No database, container, network service, or third-party Go module is used.

## Quick Start

Requires Go 1.22 or newer.

```bash
go version
go mod download

# Regenerate the deterministic local sample in data/.
go run ./cmd/gen-data -out data -force

# Build and test.
go build ./...
go test ./...
go test -race ./...
go vet ./...

# Full validation bake plus analytic truth checks.
go run ./cmd/terrain-bake bake -force -validate

# Incremental diagnostic server.
go run ./cmd/terrain-bake serve -addr 127.0.0.1:8090
```

Open `http://127.0.0.1:8090/`.

The checked-in sample has six blocks in two disconnected terrain islands. The
omitted grid column is an open-water gap. This intentionally demonstrates that
editing one island leaves the other island's cache artifacts untouched.

## Commands

```text
terrain-bake bake       [-force] [-validate] [-workers N] [-vertex-limit N]
terrain-bake serve      [-addr 127.0.0.1:8090] [-no-bake]
terrain-bake invalidate -block ID [-kind height|material]
terrain-bake edit       -block ID -kind height|material -col N -row N [-layer N] -value V
terrain-bake retry
terrain-bake compare
terrain-bake status
```

All commands also accept `-data`, `-cache`, and `-out`. Defaults are `data`,
`cache`, and `build`.

## Outputs

- `cache/snapshot.json`: atomic, versioned cache snapshot containing every
  committed block artifact, cache key, parameter version/hash, dependency set,
  and checksum.
- `build/blocks/<block>.json`: canonical per-block render batch files.
- `build/manifest.json`: global render manifest with bounds, seam statistics,
  buffer counts, cache-hit state, dependencies, timing, and artifact hashes.
- `data/edits.json`: atomic edit overlay written before an incremental rebake;
  absent for the original generated sample.

The cache snapshot is authoritative after a hard process termination. Atomic
snapshot replacement prevents mixed old/new entries. Per-block `build/` files
are derived outputs; a process killed between individual output renames can
leave an incomplete build directory, but the next startup only restores blocks
from a fully valid version-matched snapshot.

## Error Codes

- `E_SCHEMA`: missing/invalid field, unsupported schema, layer mismatch.
- `E_SIZE`: block/resource dimension or mask alignment mismatch.
- `E_WEIGHT`: material weight outside the quantization range or bad sum.
- `E_DECODE`: malformed JSON, truncated/non-finite binary project data, or a
  corrupt edit overlay.
- `E_SEAM`: seam normal/material constraints still violate after the declared
  iteration limit.
- `E_DIFF`: incremental artifacts differ from a fresh full bake.
- `E_RESOURCE`: per-thread vertex buffer limit exceeded.
- `E_CANCEL`: a bake batch was canceled before commit.
- `E_CACHE`: snapshot/output persistence failure.
- `E_TRUTH`: declared analytic truth failed validation.

Errors carry `code`, `block`, `field`, `message`, and optional structured
details. `E_SEAM` reports both seam block IDs and the measured angle/threshold;
`E_DIFF` reports the first differing block and artifact type.

## Documentation

- [Technical design](docs/TECHNICAL.md)
- [Data and cache format](docs/FORMAT.md)
- [Actual verification record](docs/VERIFICATION.md)

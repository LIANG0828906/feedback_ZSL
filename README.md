# StrokeLab

StrokeLab is a deterministic Go domain component for reconstructing a mask from
declared brush construction records. It loads three historical project versions,
normalizes them into one integer-only internal model, resolves a unique stroke
order, rasterizes and accumulates per-stroke coverage, verifies connected
topology against truth, attributes pixel differences to strokes, reuses
unaffected blocks after edits, and resumes from committed stroke checkpoints.

The repository also ships `strokediag`, a local diagnostic command that writes a
directory-local HTML report with PNG evidence. It uses no network, container,
external account, CGO driver, or database service.

## Quick Start

Requirements: Go 1.22+ on Linux/WSL. The module uses only the Go standard
library.

```bash
go build ./...
go test ./...

go run ./cmd/strokediag generate -seed 424242 -out artifacts/final-demo
go run ./cmd/strokediag verify   -seed 424242 -out artifacts/final-acceptance
```

Open the generated reports directly from the local filesystem:

```text
artifacts/final-acceptance/reports/full/report.html
artifacts/final-acceptance/reports/resume/report.html
artifacts/final-acceptance/reports/edited/report.html
```

The report shows candidate/composite/reference/difference masks, per-stroke
contributions, topology domains, pixel attribution, invalidation propagation,
checkpoint state, block reuse/recompute status, and scheduler queue records.

## Commands

```bash
# Generate 64x64/v1, 96x96/v2, and 128x128/v3 samples from one seed.
go run ./cmd/strokediag generate -seed 424242 -out artifacts/demo

# One-shot full recomputation.
go run ./cmd/strokediag run \
  -dir artifacts/demo/v3-grouped-128 -mode full -out artifacts/diag-full

# Incremental run with fixed checkpoint path.
go run ./cmd/strokediag run \
  -dir artifacts/demo/v3-grouped-128 -mode incremental \
  -checkpoint artifacts/run.ckpt -out artifacts/diag-incremental

# Resume only after committed, version-matching records.
go run ./cmd/strokediag run \
  -dir artifacts/demo/v3-grouped-128 -mode resume \
  -checkpoint artifacts/run.ckpt -out artifacts/diag-resume

# Simulate abrupt process termination after three commits.
go run ./cmd/strokediag run \
  -dir artifacts/demo/v3-grouped-128 -mode incremental \
  -checkpoint artifacts/kill.ckpt -hard-abort-after 3

# Then restart and resume.
go run ./cmd/strokediag run \
  -dir artifacts/demo/v3-grouped-128 -mode resume \
  -checkpoint artifacts/kill.ckpt -out artifacts/diag-restarted

# Edit one stroke, reuse the prior block cache, and show invalidation.
go run ./cmd/strokediag run \
  -dir artifacts/demo/v3-grouped-128 -mode full \
  -reuse-cache artifacts/final-acceptance/cache/blocks.json \
  -edit-stroke body-top -edit-weight 7800 \
  -cache-out artifacts/edited-cache.json -out artifacts/diag-edited
```

`-hard-abort-after` calls `os.Exit(17)` after the selected number of fsynced
commits and intentionally produces no exit report. A built binary therefore
returns status 17; `go run` wraps that status as 1.

## Persistence Decision

The task permits a standard-library substitute when the SQLite driver is
external. StrokeLab deliberately uses that substitute:

- `checkpoint.go` is an append-only journal with a fixed header, one
  base64-encoded JSON record per line, a CRC32 trailer, one serialized write per
  stroke, and `fsync` before commit acknowledgement.
- Transaction IDs are strictly contiguous. Recovery ignores an incomplete or
  bad final record, truncates back to the last valid record boundary, then
  continues with new transactions. Corruption before the tail returns
  `E_CKPT`.
- Every stroke record contains the per-block component, component digest,
  block cache key, cumulative content hash, conversion residual digest, order
  version, stage/seq, and algorithm version.
- `-reuse-cache` / `-cache-out` persist complete block outputs separately using
  an atomic temp-file + fsync + rename replacement.

No `database/sql` driver, local SQLite file, shared database, or external
service is needed.

## Versions

| Version | Stroke field | Coordinates | Weight | Hardness | Sequence | Groups |
|---|---|---|---|---|---|---|
| `mask-work/v1` | `operations` | absolute `from_px` / `to_px` | per-mille | absent, default 1000 | zero-based, optional | synthesized from stage |
| `mask-work/v2` | `brush_ops` | offset from previous endpoint, quantum 2 | per-mille, optional default 1000 | absent, default 1000 | one-based, optional | per-stroke `group` |
| `mask-work/v3` | grouped `constructs` | absolute `from` / `to` | pp10000 | present | one-based | grouped with stage |

The internal model uses absolute integer pixels, one-based sequence, hardness
in permille, and weight in pp10000. Every field mapping/default is recorded in
`Project.ConversionTrace`. Unknown versions and missing required fields return
`E_VER`.

Strict export rejects values a target cannot represent with `E_LOSSY` and a
structured loss list. Successful exports are loaded back immediately and
compared field-by-field; a mismatch returns `E_RT`. Integer scale and quantum
overflows return `E_OVF` with the field and operands. Generated v1/v2 samples
declare values that survive downgrade and upgrade exactly.

## Core Semantics

- Composition order is `(stage priority, sequence)` ascending. Duplicate
  priority/sequence pairs are ambiguous and return `E_ORD`.
- Stroke coverage is rasterized with integer fixed-point geometry, hardness
  falloff, and pp10000 weight. Candidate coverage and stroke alpha are combined
  with integer source-over rounding; no float accumulation exists in the
  composition path.
- Each block records canonical participants, winner basis, suppressed strokes,
  and local order version. Pixel results are independent of declaration order,
  worker count, queue pressure, and completion order.
- Topology uses row-major 4-connected foreground domains, one-to-one IoU
  matching, area/centroid/bbox/hole/ring checks, and `E_TOP` evidence containing
  the domain ID and measured values. Ring width is
  `2000 * area / boundaryPixels` milli-pixels.
- Attribution assigns every differing pixel to the largest recorded stroke
  component, with both first-applied and primary stroke order, plus contribution
  share in pp10000.
- Cache keys combine block input content, participating stroke content, local
  composition order, conversion parameter version, and algorithm version.
  Editing one stroke invalidates only old/new footprint blocks or blocks whose
  local canonical order actually flips, then marks topology/attribution
  downstream.
- Scheduler budgets bound worker count, pixels per worker, pending queue depth,
  and retries. Every wait/failure/retry is recorded. A failed stroke is never
  committed; successful sibling blocks remain uncommitted until all blocks of
  that stroke succeed.

## Error Codes

| Code | Meaning |
|---|---|
| `E_VER` | unknown version or invalid/missing required field |
| `E_LOSSY` | strict downgrade loss list |
| `E_OVF` | checked integer conversion overflow |
| `E_RT` | export/load-back mismatch |
| `E_ORD` | ambiguous composition order |
| `E_TOP` | topology or pixel tolerance failure |
| `E_BUDGET` | scheduler budget violation |
| `E_CKPT` | stale/corrupt/mismatched checkpoint or cache |
| `E_EXEC` | block failed after retries |
| `E_CANCELED` | context cancellation |

`errors.As` with `*strokelab.CodedError` exposes `Code`, `Message`, and
structured `Details`.

## Layout

```text
model.go          internal model and deterministic ordering
generator.go      seeded icons, candidate perturbations, strokes, truth
version.go        v1/v2/v3 loaders, strict exporter, RLE, trace/loss rules
integer.go        checked integer conversion and alpha rules
raster.go         fixed-point stroke rasterization and accumulation
topology.go       domains, holes, ring width, one-to-one comparison
attribution.go    per-pixel responsibility and culprit summary
engine.go         common full/incremental/resume engine and path comparison
scheduler.go      bounded worker/queue/pixel scheduler and retries
cache.go          block keys, block cache, order traces
invalidation.go   edits and exact local invalidation planning
checkpoint.go     CRC32 append-only transaction journal
diagnostic.go     PNG/HTML/JSON local diagnostics
verification.go   fixed-seed acceptance pipeline
deps.go           explicit stroke/order/block/topology dependency graph
cmd/strokediag    local command
```

See `docs/ARCHITECTURE.md` for design details and `docs/VERIFICATION.md` for the
recorded test and acceptance results.

# soundlab

`soundlab` is a self-contained Go 1.22+ domain component and local demo for
deterministic audio fingerprinting and similar-clip retrieval. It targets game
and interactive-media sound libraries and uses only generated mono PCM, the Go
standard library for analysis/retrieval, and pure-Go SQLite
(`modernc.org/sqlite`) for versioned checkpoints.

## What is included

- `audiofp`: the reusable domain package.
  - Configurable framed FFT analysis, band-energy quantization, bit-packed
    fingerprints, and explicit zero frames.
  - Deterministic frame index construction and L1 similarity search.
  - Explicit dependency graph, precise invalidation reports, incremental rebuild
    session, and first-difference comparison.
  - Versioned SQLite shard cache/snapshot store, cancellation/resume,
    serialized atomic commits, and independent truth verifier.
  - Typed errors: `E_SCHEMA`, `E_PARA`, `E_DECODE`, `E_ORDER`, `E_STAGE`,
    `E_DIFF`.
- `samplegen`: fixed-seed fixture and truth generator.
  - Synthesizes multi-batch PCM from band profiles and marker transients.
  - Applies replace/trim/recipe edits.
  - Derives reference frame fingerprints with a generator-local FFT, not the
    production extractor, and computes expected query rankings from those truth
    frames.
- `cmd/soundlab`: `generate`, `demo`, `inject`, and `verify` commands.
- Tests for determinism, zero frames, parameter/order errors, search purity,
  path parity, checkpoint cancellation/resume, version invalidation, selective
  cache reuse, stage retry, loader validation, and verifier tamper detection.

## Requirements

- Go 1.22 or newer.
- Windows 10+ (the same commands work on Linux/macOS).
- No external audio, accounts, services, Docker, or container runtime.
- `modernc.org/sqlite` is downloaded by `go mod download` and uses no CGO.

## Quick start

Run from the repository root.

```powershell
go mod download
go build ./...
go vet ./...
go test ./...

go run ./cmd/soundlab generate --seed 20261125 --out ./samples --batches 4 --assets 3
go run ./cmd/soundlab demo --samples ./samples --artifacts ./artifacts/demo --workers 4 --buffer-samples 8192 --shard-frames 24 --resume-after 2
go run ./cmd/soundlab inject --samples ./samples --artifacts ./artifacts/faults --workers 4 --buffer-samples 8192
go run ./cmd/soundlab verify --manifest ./samples/batch-000/project.json --db ./artifacts/demo/batch-000/full.db --project-id batch-000
```

The demo writes `artifacts/demo/report.json` and prints the same JSON report.
Generation is byte-for-byte reproducible for a fixed seed.

## Fixture format

Each `samples/batch-NNN/project.json` contains:

- `assets`: initial raw `s16le` mono PCM descriptors (`asset_id`, relative
  `file`, `sample_rate`, `total_samples`, `channels`, `sample_format`,
  `sha256`).
- `recipe`: frame length, frame shift, band count, quantization width,
  similarity threshold, and candidate limit.
- `edits`: monotonic logical-time operations:
  - `trim`: remove `[start_sample,end_sample)`.
  - `replace`: replace the range with a referenced PCM segment.
  - `set_param`: update a recipe field.
- `truth`: final recipe, per-asset final content hash, per-frame expected
  fingerprint (levels, packed hex, zero flag, summary), embedded marker sample
  positions, and query files with expected ranked neighbors.

Truth fingerprints are derived by the generator's own synthesis/reference
transform. The production extractor is never called while generating truth.

## Analysis and retrieval

For each full frame:

1. Copy `frame_len` samples into a zero-padded radix-2 FFT.
2. Divide all positive FFT bins into contiguous bands and sum band power.
3. Normalize band power per frame and quantize each band to
   `0..2^quant_bits-1`.
4. Pack band levels MSB-first into a stable hex fingerprint and summarize the
   start sample, dimensions, packed bytes, and zero flag.

Silent frames are retained with `zero=true` and all-zero levels.

Search scans every contiguous query-length frame window. Similarity is
`1 - sum(|query_level-candidate_level|) / (frames*bands*maxLevel)`. Ranking is
similarity descending, then asset ID, then start sample, so it is independent of
insertion order and scheduler timing. Query results use recipe threshold and
candidate limit; invalid limits return `E_PARA`.

## Incremental invalidation

The dependency graph contains:

```text
asset content -> fingerprint(frame key) -> index entry
recipe --------> fingerprint
all index entries -> query result
```

Frame keys include the asset content hash, frame number/start, recipe values,
and an upstream recipe key. Index keys include the frame key. Shard cache keys
include content hash, recipe version, frame range, and algorithm version.

- Replace/trim invalidates that asset's fingerprint/index range and all query
  results.
- Frame-affecting recipe fields invalidate all fingerprint/index ranges and all
  query results.
- Threshold/limit-only changes invalidate query results without recomputing
  fingerprints.
- The incremental session recomputes affected assets and reuses untouched
  analyses. The persistent store reuses exact cache-key matches.

Invalidation reports include asset ID, start/end frame range, and reason.

## SQLite checkpoints and resume

SQLite contains:

- `snapshots`: project hash, recipe version, algorithm version, layout version.
- `cache_shards`: fingerprint/index payload, content hash, recipe/algorithm
  versions, range, and payload digests.
- `snapshot_shards`: stable per-project commit sequence and cache references.

Each shard is written in one transaction containing both its cache payload and
snapshot commit row. The store uses a single serialized writer and canonical
asset/shard order.

On resume:

- Matching snapshot versions reuse only persisted shards with matching cache
  keys/digests.
- A changed content hash, recipe version, algorithm version, layout, or corrupt
  shard invalidates the whole snapshot and forces a zero-reuse rebuild.
- Cancellation returns after a deterministic number of new shard commits; a
  restart reuses exactly those matching committed shards.

## Independent verification

`verify` never invokes PCM extraction. It reads SQLite shard payloads plus the
manifest truth, then:

- rebuilds packed fingerprints and summaries from persisted levels;
- rebuilds index entries and cache keys from persisted frame data and recipe;
- recomputes query rankings from truth query frames against persisted frames;
- reports the first mismatch as `E_DIFF` with asset ID, frame, and field.

## Fault injection

`inject` and the demo exercise:

| Fault | Expected code | Detail |
|---|---|---|
| PCM shorter than declared | `E_DECODE` | asset ID and `total_samples` |
| `frame_shift > frame_len` | `E_PARA` | field `frame_shift` |
| injected frame timing inversion | `E_ORDER` | asset, shard, frame |
| injected single-shard failure | `E_STAGE` | asset, shard, frame |

The stage-failure demo commits other shards, then resumes without the injected
failure and compares recovered output with a fresh full build.

## Actual validation results

Validated on 2026-11-25 with Go 1.22.12:

```text
go build ./...                  PASS
go vet ./...                    PASS
go test ./...                   PASS
go test -race ./...             PASS
CGO_ENABLED=0 go build ./...    PASS
CGO_ENABLED=0 go test ./...     PASS
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...  PASS
```

Generated fixture set: 4 batches, 3 assets/batch, 8 queries, 1,763 truth
frames, about 1.1 MB of PCM. A separate two-run hash comparison for seed 12345
produced byte-identical trees (`13` files).

Final `demo` result:

| Batch | Frames | Reused/Recomputed | Cache hit/new | Resume committed/reused | Version reset | DAG/topo nodes | Verified | Queue waits |
|---|---:|---:|---:|---:|---|---|---:|---:|
| batch-000 | 285 | 100 / 185 | 5 / 8 | 2 / 2 | true / 0 reuse | 575 / 575 | true | 10 |
| batch-001 | 585 | 0 / 585 | 0 / 26 | 2 / 2 | true / 0 reuse | 1,175 / 1,175 | true | 6 |
| batch-002 | 587 | 0 / 587 | 0 / 26 | 2 / 2 | true / 0 reuse | 1,179 / 1,179 | true | 14 |
| batch-003 | 306 | 0 / 306 | 0 / 15 | 2 / 2 | true / 0 reuse | 617 / 617 | true | 16 |

Incremental, full, and resumed analyses/indexes/commit sequences and query
rankings compared equal for every batch. The batch-000 independent verifier
reported:

```json
{
  "project_id": "batch-000",
  "algorithm_version": "afp-v1",
  "assets_checked": 3,
  "frames_checked": 285,
  "index_checked": 285,
  "queries_checked": 2,
  "ok": true
}
```

Standalone injection result:

```json
{
  "faults": [
    {"kind":"truncate","expected_code":"E_DECODE","actual_code":"E_DECODE","asset_id":"b000-a0","ok":true},
    {"kind":"recipe_contradiction","expected_code":"E_PARA","actual_code":"E_PARA","ok":true},
    {"kind":"frame_order","expected_code":"E_ORDER","actual_code":"E_ORDER","asset_id":"b000-a0","shard":1,"frame":2,"ok":true},
    {"kind":"stage_failure_retry","expected_code":"E_STAGE","actual_code":"E_STAGE","asset_id":"b000-a0","committed_before_retry":12,"reused_after_retry":12,"ok":true}
  ],
  "ok": true
}
```

## Notes and boundaries

- Raw PCM has no embedded sample-rate header. The manifest declares the sample
  rate; query sample rates are checked against their source assets and all
  numeric fields are range-validated.
- The FFT uses a rectangular window and deterministic contiguous bin bands.
  This keeps truth construction and repeatable verification simple.
- Demo fixtures are intentionally small. The same code paths accept larger
  assets; worker concurrency is capped at 32 and the configured sample-buffer
  budget can queue work while recording the reason.
- Generated `samples/`, demo databases, and reports are local artifacts and are
  ignored by Git.

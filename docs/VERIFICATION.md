# Verification Record

Recorded on 2026-11-24 in the supplied workspace.

## Environment

- Host command: `go version`
- Result: `go version go1.22.12 linux/amd64`
- Go directive: `go 1.22`
- Module: `terrain-bake`
- External modules: none
- Database/container/network service: none

The current execution host is Linux. Windows compatibility was checked by
cross-compiling the complete module:

```bash
GOOS=windows GOARCH=amd64 go build ./...
```

This completed successfully. The implementation uses only portable Go standard
library APIs and explicit little-endian binary decoding.

## Build and Test

Executed successfully:

```bash
go build ./...
go test ./...
go test -race ./...
go vet ./...
GOOS=windows GOARCH=amd64 go build ./...
```

Test coverage includes:

- missing-field, size, layer, weight-range, weight-sum and decode errors with
  expected block/field attribution;
- deterministic repeated bakes and call-order independence;
- all analytic height/material/count/seam truth checks;
- seam failure `E_SEAM` including both block IDs and measured angle;
- per-thread vertex resource limit;
- exact invalidation scope, bidirectional edges, dependency paths, stable order;
- initial bake, snapshot restart, all-cache-hit restart;
- precise incremental invalidation and full-bake byte equality;
- snapshot global-hash and parameter-version invalidation;
- corrupt snapshot whole-snapshot rejection;
- concurrent snapshot writers producing one whole valid snapshot;
- block panic/failure isolation and retry without neighbor artifact changes;
- canceled batch committing no partial artifacts;
- parallel-vs-serial final artifact bytes, snapshot hash and commit order;
- global render manifest/artifact files;
- first-block/first-product-type `E_DIFF` localization;
- HTTP page, PNG preview, status, invalidation, edit persistence and compare APIs.

## Deterministic Sample

```bash
go run ./cmd/gen-data -out data -force
```

Observed:

```text
generated data: 6 blocks, 5 seams, 53 truth checks, seed=20261124
```

```bash
go run ./cmd/terrain-bake bake -force -validate
```

Observed:

- generation `1`;
- six committed blocks, no failures;
- stable commit order:
  `B_0_0, B_1_0, B_0_1, B_1_1, B_3_0, B_3_1`;
- all 53 analytic truth checks passed;
- every block reported zero seam violations, maximum normal angle `0`, and
  material boundary delta `0`.

## Incremental and Full Equality

An actual sample edit was performed:

```bash
go run ./cmd/terrain-bake edit \
  -block B_0_0 -kind height -col 10 -row 10 -value 2.5
```

Observed scope:

- affected: `B_0_0, B_1_0, B_0_1, B_1_1`;
- cache hits: `B_3_0, B_3_1`;
- commit order matched the affected order.

Then:

```bash
go run ./cmd/terrain-bake compare
```

Observed:

```json
{
  "equal": true,
  "compared": 6,
  "message": "artifacts are byte-identical"
}
```

A material invalidation query for `B_3_0` observed only
`B_3_0, B_3_1` affected and the four block-0/1 island blocks as cache hits.

After restarting the CLI, `bake` observed no affected blocks and six cache hits,
confirming snapshot recovery.

The workspace was then regenerated to a clean base sample, followed by a final
`bake -force -validate`; final cache generation is `1` with no `data/edits.json`.

## HTTP Diagnostics

Served with:

```bash
go run ./cmd/terrain-bake serve -addr 127.0.0.1:18090
```

Observed:

- `GET /` returned HTTP 200 with the workbench page, block previews, invalidation
  graph, cache/timing table, algorithm version, and edit controls;
- `GET /api/status` returned six artifacts and valid cache;
- `GET /preview/B_0_0.png` returned a valid `240x160` PNG;
- all blocks showed cache hits on the restarted server.

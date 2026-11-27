# Volume Fog Chunk Integration Kernel

## Scope

This module implements an independently testable transparent-volume fog kernel
plus a local diagnostic workbench. Everything is generated from a fixed seed;
there is no network, GPU, Docker or external sample dependency. The only Go
module is `volumefog`, and `go list -m all` reports no third-party modules.

## Packages and entry points

- `volumefog` (repository root): scene model, generator, fixed-point geometry,
  overlap compositing, integration, dependency graph, scheduler, checkpoints,
  preview/exact comparison and fault diagnostics.
- `volumefog/store`: checkpoint boundary. With cgo it opens the system
  `libsqlite3.so.0` ABI directly through `dlopen`; each chunk is committed in
  one `BEGIN IMMEDIATE ... COMMIT` transaction under a Go write mutex. With
  `CGO_ENABLED=0` or an unavailable SQLite ABI, it uses an atomic
  standard-library file snapshot fallback.
- `cmd/workbench`: embedded local HTML diagnostic workbench.
- `cmd/validate`: deterministic acceptance runner producing JSON and text
  reports under `artifacts/validation`.

## Determinism and sample scene

`GenerateScene` hashes `(seed, medium salt, x, y, z)` into each integer density
voxel. The default seed is `0x5EED2026` (`1592598566`). The scene contains:

- three AABBs with Q10 corners: `fog_a`, `fog_b`, `fog_c`;
- densities in `[0,255]` encoded as little-endian int32 plus SHA-256 checksum;
- three camera samples at `0, 500000, 1000000` microseconds, strictly ordered;
- an orthographic frustum trajectory whose center rays are +Z and therefore
  analytically integrable;
- recipe steps `128`, preview steps `8`, HG phase, shadow samples `64`,
  preview shadow samples `8`, shadow budget `64`, chunk size `8`.

The truth generator uses closed-form exponential cell integration. It does not
call the numerical step-group kernel. Truth includes per-sample-ray atomic
segments, exit transmittance, cumulative scattered radiance, and every chunk
center pixel color/transmittance.

## Fixed-point intervals and overlap rule

- World positions and ray parameters use Q10 (`1/1024` unit) int64 endpoints.
- AABB slab intersections use integer floor/ceil division. Residuals are
  recorded per interval endpoint and per atomic segment.
- Every interval endpoint becomes an atomic boundary. In each atomic interval,
  all active media are sorted by ID and composited once:
  `sigma_t = sum_i sigma_t_i`, `sigma_s = sum_i sigma_s_i`, and phase
  anisotropy is scattering-weighted. The kernel does not add whole-medium
  integrals independently.
- For +Z camera/sample rays, exact voxel-Z boundaries are inserted. This keeps
  transmittance independent of the numerical step budget.

## Integration and budgets

Within each integration group:

- optical depth is summed exactly over the group's voxel cells, then transmittance
  is multiplied by `exp(-tau)`;
- the step budget controls how many radiance quadrature groups are formed;
- each shadow call reserves its complete recipe sample budget for the call
  lifetime; actual sampled cells may be fewer. Fewer shadow samples merge
  shadow cells; sufficient samples use one sample per exact shadow cell.

Consequences:

- tightening the step budget never increases transmittance (it is exactly
  preserved);
- the declared error bound is
  `0.5/steps + 0.5/shadowSamples`, which decreases monotonically as either
  budget is tightened;
- full-budget sample/chunk-center results match analytic truth within `2e-7`.

The chunk scheduler enforces:

- maximum parallel workers (`max_parallel`);
- per-worker ray batch limit (`ray_buffer`);
- total concurrent shadow-sample budget (`shadow_budget`).

The shadow budget is a condition-variable semaphore. A chunk that cannot obtain
samples waits, records the reason, needed/current/capacity values and wait
duration, then is resumed. No chunk task is dropped.

## Preview / exact consistency

Preview uses 8 steps and 8 shadow samples by default; exact uses 128 and 64.
Both frames carry the same media/camera/recipe snapshot hashes. After exact
rendering, the workbench compares chunk-center RGB and transmittance against the
last preview. The declared limits are `0.02` color and `0.01` transmittance.
The first violation is `E_DIVERGE` with chunk coordinates, both measured values
and deviation.

## Dependency graph, cache keys and exact invalidation

The graph is:

```
density/absorption/scattering/anisotropy/camera/recipe
        -> ray segment node -> ray node -> chunk node
```

A segment cache key chains:

```
hash(source values, ray-segment id, parameter versions, previous segment key)
```

A chunk key contains content hash, all ray keys, camera version, recipe version
and algorithm version. Cache validation also checks each segment content hash
and key chain. Editing one voxel changes only chunks whose segment dependencies
include that voxel; other chunks reuse persistent SQLite records.

Invalidation output contains the medium ID, chunk coordinate, reason and source
IDs. The graph tracks actual sampled primary and shadow voxels, not just medium
bounds.

## SQLite checkpoints

The cgo shim declares the needed SQLite C ABI functions and loads
`libsqlite3.so.0` at runtime, avoiding a third-party Go module and avoiding a
compile-time `sqlite3.h` requirement.

Schema:

- `chunks`: coordinates, stable commit sequence, ray digest, cache/content
  hashes, recipe/algorithm/camera/scene versions, center transmittance and
  scattering, dependency blob, gzip JSON payload.
- `commits`: stable row-major `seq -> chunk` journal.
- `meta`: backend version.

Each `PutChunk` does both `chunks` upsert and `commits` upsert in one serialized
transaction. The scheduler commits only in row-major chunk order. On restart,
missing/changed content or versions invalidate the whole chunk and force a
recompute.

Three paths are compared:

1. one-shot full render (no cache);
2. realtime render with constrained workers/ray buffer;
3. resume after truncating the commit suffix, closing and reopening SQLite.

They are compared pixel-by-pixel and commit-sequence-by-commit-sequence; the
first mismatch is located by chunk/pixel/field.

## Error codes

| Code | Meaning | Location payload |
| --- | --- | --- |
| `E_SCHEMA` | missing/invalid field, fixed-point or compatibility violation | medium ID + field |
| `E_DECODE` | density payload/checksum corruption | medium ID + `density_checksum` |
| `E_ORDER` | camera sample times not strictly increasing | camera ID + `time_us` |
| `E_DENS` | integer density outside `[0,255]` | medium ID + `density[index]` |
| `E_INT` | integration truth/tolerance or budget violation | ray ID + deviation |
| `E_BLOCK` | one chunk failed | chunk coordinate |
| `E_DIVERGE` | preview/exact or path comparison mismatch | first chunk + measured values |

A single chunk failure (or injected `E_BLOCK`) is not committed, leaves other
chunks and their transactions intact, and can be retried; the retry reuses the
seven successful checkpoints and only computes the failed chunk.

## Workbench HTTP API

`go run ./cmd/workbench` serves `http://127.0.0.1:8099`.

- `GET /api/state`: scene, density arrays, chunk grid, cache/budget/invalidation
  state and dependency graph counts.
- `POST /api/render/preview|exact|resume`
- `POST /api/edit`: `{"kind":"voxel"|"scattering", ...}`
- `POST /api/fault`: `{"kind":"decode"|"order"|"dens"|"block"}`
- `POST /api/truth`
- `POST /api/compare`

The embedded page draws max-Z density projections, colorized chunk centers,
progress/cache counters, budget wait details, invalidation lists and edit/fault
controls. It uses only inline HTML/CSS/JS.

## Verification

Run:

```bash
go test ./...
CGO_ENABLED=0 go test ./...
go test -race ./...
go vet ./...
go run ./cmd/validate -out artifacts/validation
```

`artifacts/validation/validation-summary.txt` and
`artifacts/validation/validation-report.json` contain the recorded local run.

# Technical Design

## Packages

- `project`: manifest/resource loading, pointer-wire schema validation, edit
  overlay replay, content hashing, and error codes.
- `bake`: deterministic block baking, seam projection and statistics,
  dependency graph, invalidation queries, snapshot store, worker pool, manager,
  render-manifest writer, and full-vs-incremental comparison.
- `server`: standard-library `net/http` diagnostics, JSON APIs, and PNG block
  previews.
- `cmd/gen-data`: deterministic terrain sample generator.
- `cmd/terrain-bake`: CLI and local server entry point.
- `internal/atomicfs`: fsync-then-rename atomic file replacement.
- `internal/testutil`: test-only project copying/JSON helpers.

## Loading and Validation

`project.Load` reads `manifest.json`, all block height/material/mask binaries,
`truth.json`, and optional `edits.json`.

Validation order:

1. JSON syntax and field types. Syntax corruption is `E_DECODE`; a missing or
   incorrectly typed schema field is `E_SCHEMA`.
2. Manifest semantics: schema version, positive grid dimensions, material
   layer IDs/colors, bake parameters, and resource paths that stay inside the
   data root.
3. Block layout: block grid equals the manifest block grid (`E_SIZE`), world
   coordinates are in range, resource metadata matches block dimensions
   (`E_SIZE`), and material count matches the global layer count (`E_SCHEMA`).
4. Binary decoding: exact byte count, finite height samples, and endian-safe
   `f64le`/`u16le`/`u8` decoding. Truncation or non-finite data is `E_DECODE`.
5. Material quantization: every weight is in `[0, quantMax]` and each cell sums
   exactly to `quantMax`; violations are `E_WEIGHT` with block and field.
6. Seam topology: known blocks, opposite edges, adjacent grid positions,
   matching sample counts, unique IDs, and unique block pairs.
7. Truth annotations and optional edit overlay. Every edit is replayed in order;
   material edits rebalance the other layers deterministically while preserving
   the quantization sum.

The canonical content hash covers the canonical manifest plus the final height,
weight, and mask arrays. Truth annotations and edit history are metadata and do
not alter render bytes, so changing only annotation values does not invalidate a
render snapshot.

## Deterministic Block Bake

For a block with `C x R` source samples and subdivision `S`, the output is a
`((C-1)S+1) x ((R-1)S+1)` vertex grid.

- Heights are bilinearly resampled from source samples.
- Local normals are finite-difference derivatives of the fine height grid,
  with one-sided derivatives at non-seam boundaries.
- Material weights are bilinearly resampled and quantized with deterministic
  largest-remainder rounding.
- Triangles use stable row-major winding.
- Scatter candidates are generated per coarse cell from a fixed SplitMix64
  stream seeded by block seed/position. Candidates in cells below the
  passability threshold are culled; deterministic instance cap truncates.
- The AABB includes terrain vertices and instance positions.

No timing, worker ID, map iteration, or call order is embedded in an artifact.
Canonical JSON is the artifact byte representation used by cache, comparison,
and output files.

Per-block output contains positions, normals, triangle indices, per-vertex
quantized material weights, per-layer coverage ratios, instance buffer, AABB,
and seam statistics.

## Seam Treatment

At a seam, both blocks share coincident fine samples. The bake computes a
world-space height sampler that averages bilinear heights from every block
containing the point. A shared seam normal comes from central differences of
that sampler, so both sides receive bit-identical normals. This is a direct
manifold projection and converges in one relaxation pass.

Material treatment:

- Exact seam vertices receive the quantized world-sampled material average, so
  both sides have identical boundary weights.
- A configurable fine transition band blends interior vertices toward projected
  seam weights.
- Statistics compare every seam sample for both blocks.

`seamMaxIterations == 0` disables projection; measured independent one-sided
normals then expose seam error. If any seam still exceeds the angle threshold
or has nonzero material boundary delta, bake returns `E_SEAM` with seam, block
IDs, measured angle, threshold, and material delta. Successful artifacts report
zero violations, zero maximum normal angle, and zero maximum material delta.

## Dependency Graph and Invalidation

Each manifest seam becomes two invalidation edges. Both carry normal and
material dependency kinds. Invalidation is breadth-first over this bidirectional
constraint graph:

- The edited block is included with reason `edit:<kind>`.
- Every reached block has the shortest dependency path from the source.
- Unreachable blocks are returned as cache hits.
- A height edit labels paths `seam-normal:<seam>`; a material edit labels them
  `seam-material:<seam>`.

Because real seam constraints are symmetric, connected terrain components are
strongly connected graphs. Bake ordering is therefore a stable topological
order of the SCC condensation: independent components are topologically sorted,
and blocks inside a cyclic seam component are ranked by manifest order. This
provides deterministic commit order without pretending a cycle has a linear
topological order.

## Cache Snapshot and Recovery

Snapshot fields include:

- schema version;
- global content hash;
- bake algorithm version;
- project parameter version and artifact-affecting parameter hash;
- generation and stable commit order;
- per-block cache key, sorted dependency set, artifact type, SHA-256, and
  canonical artifact bytes.

Writes use a same-directory temp file, `fsync`, close, `rename`, and directory
sync. A store mutex serializes writes. Loading validates the global hash, code
version, parameter version/hash, every cache key, every dependency set, every
artifact checksum, canonical re-encoding, and block identity. Any mismatch
invalidates the entire snapshot; no block entry is mixed with entries from a
different version.

The manager holds an operation mutex while applying an edit and rebuilding its
component. Successful artifacts are committed only after all workers finish, in
stable order, and one complete snapshot is then atomically replaced. If the
batch is canceled before commit, no new snapshot is written.

## Scheduler and Bounds

- `maxWorkers` is a semaphore limiting concurrent block bakes.
- `maxVerticesPerThread` is checked before allocation. A block whose output
  grid exceeds the limit fails with `E_RESOURCE`.
- Every worker owns its block buffers; no mutable project arrays are modified.
- Results are collected and committed in deterministic dependency order.
- Parallel and serial runs therefore produce identical artifact bytes,
  snapshot bytes, and commit order.
- Snapshot writes are serialized by the store and by manager operation order.

The cache model does not promise a complete `build/` directory after a forced
kill. It promises that restart only restores blocks from the last fully written,
version-matched snapshot, then rebakes everything else.

## Failure Isolation

A block bake error marks only that block failed, removes its stale cached/output
artifact, and leaves healthy neighbors/artifacts alone. The global manifest
records the failure. `retry` bakes only failed blocks and does not rewrite
committed neighbor bytes. If a process has edits, the edit overlay is persisted
atomically before rebaking; an interrupted incremental run is detected by the
snapshot content-hash mismatch on restart.

## HTTP Diagnostics

`net/http` routes:

- `GET /` server-rendered diagnostic page.
- `GET /preview/<block>.png` material/height-shaded 240x160 PNG preview.
- `GET /api/status` manager/cache/block state.
- `GET /api/invalidate?block=ID&kind=height|material` exact invalidation paths.
- `POST /api/edit` explicit height/material edit and incremental rebake.
- `POST /api/retry` retry failed blocks.
- `POST /api/cancel` cancel the active batch before commit.
- `POST /api/compare` incremental-vs-full byte comparison.
- `GET /healthz` liveness.

The page shows block previews, seam violations/angles, invalidation SVG/table,
cache hits, rebake timings, artifact hashes, and the last bake report. There are
no external assets or network dependencies.

# Architecture Notes

## Determinism boundary

The engine never imports the hidden truth manifest. `workspace` and `Wails` expose only observations and reconstructed outputs. The truth manifest is consumed by:

- generator tests,
- the acceptance CLI, for numeric comparison,
- sample JSON export.

All nondeterministic-looking operations are content-addressed:

- candidate ordering uses sorted grid coordinates,
- reconstruction uses SplitMix64 seeded by SHA-256 of region content, zone parameters, estimates and seed,
- worker completion order cannot influence result ordering,
- checkpoint commits use a stable ordinal sequence independent of worker completion.

## Cache keys

| Key | Inputs |
| --- | --- |
| `SamplingKey` | zone ID/geometry/ordinal, relevant exclusions, sampling config, valid observation records, seed |
| `ParamsVersion` | zone geometry/params, relevant exclusions, config hash, terrain raster hash |
| `EstimateKey` | sampling key, zone params, params version, quantization step |
| `PointsKey` | estimate key, seed, zone params |
| `CacheKey` | all three component keys |
| `ContentHash` | component keys + samples + estimate + reconstructed points + heat summary |

`ContentHash` is recomputed from canonical fields after the heat summary is known. It is idempotent and therefore can be compared across incremental runs without a generation counter.

## Heatmap pixel reuse

The heat raster is partitioned into 8×8 tiles. On invalidation:

1. A pixel is dirty when its current or previous owning zone is affected.
2. A tile is dirty when it contains at least one dirty pixel.
3. Dirty tiles are scheduled as bounded work units.
4. Non-dirty pixels in a dirty tile are copied from the previous raster bit-for-bit.
5. Completely non-dirty tiles are not recomputed.

This is why a tile can have `partial` reuse state. The manifest reports both tile and pixel counts.

## Checkpoint transaction order

For every run:

1. `sessions` row is inserted with `running`.
2. Zone states are recomputed/reused.
3. Heatmap is finalized and each zone receives a heat summary.
4. Each zone is committed in ordinal order in a separate SQLite transaction.
5. The session becomes `completed`, `cancelled` or `failed`.

A process kill before step 5 leaves `running`, which is the forced-termination marker. A watched cancellation executes step 5 with `cancelled`.

On load:

1. The newest session for the exact scenario hash is selected.
2. Session algorithm version and zone order are checked.
3. Rows must start at ordinal 0 and be contiguous.
4. Every row's zone parameter version and payload SHA-256 are checked.
5. Any failure deletes all rows for the session and marks it `invalid`.
6. A valid prefix is injected into a fresh engine and missing zones are recomputed in ordinal order.

This prevents mixing old and new zone results.

## Error contract

| Code | Meaning |
| --- | --- |
| `E_SCHEMA` | missing/invalid scalar or configuration field |
| `E_GEOM` | invalid geometry/overlap/out-of-bounds, includes zone/vertex |
| `E_DIFF` | incremental vs full mismatch, includes point index or pixel coordinates and zone |
| `E_CANCEL` | active cancellation, committed checkpoints retained |
| `E_IO` | filesystem/SQLite/export failure |

## Concurrency

`workerPool` is a fixed worker set with a bounded queue. Submission uses a non-blocking send first; if the queue is full it increments `BufferBlockEvents`, stores a human-readable reason, then blocks until capacity or context cancellation. Work is never silently dropped.

Heatmap tasks respect:

```text
HeatmapTileSize² <= PerWorkerBufferSize
```

This is validated at scenario load.

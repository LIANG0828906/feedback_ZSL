# Technical Notes

## Pipeline

1. `parseFont` validates the JSON contract and resolves each raw glyph into a `NormalizedGlyph`.
2. Bezier segments are adaptively flattened in font units. Quadratic/cubic subdivision uses a deterministic tolerance (`0.2` units by default) and depth cap.
3. Composite references are expanded recursively with affine transforms. A DFS stack detects cycles; all diagnostics retain the top-level glyph and component/segment index.
4. Declared contour direction is checked against signed area and normalized by reversal when necessary.
5. `placeGlyph` maps font coordinates to a device bitmap using `targetPixelSize / unitsPerEm`, one-pixel margins and fractional `subpixelX/Y`.
6. Every coverage sample uses absolute bitmap coordinates:

   `fontX = bounds.x0 + (deviceX - originDeviceX) / scale`

   `fontY = bounds.y1 - (deviceY - originDeviceY) / scale`

   This is the key seam invariant: a pixel inside a tile and the same pixel in the whole-glyph truth pass see exactly the same sample positions.
7. Winding is accumulated over all closed normalized contours. Nonzero fills `winding != 0`; even-odd fills `(winding & 1) != 0`.
8. Subpixel hits are converted to a byte and quantized with nearest-level rounding. Full coverage stays 255.

## Budget model

For a block of `P` pixels and `S = samplesPerAxis^2`:

```text
samples       = P * S
coverage      = P
scanline      = width * 32
transient     = min(samples, sampleBatchSize) * 12 + 256
estimated     = coverage + scanline + transient
taskLimit     = min(perTaskBudgetBytes, globalBudgetBytes)
```

Planning happens before any scan conversion. If a base tile is above `taskLimit`, operations are applied in `degradeSequence` order:

- `halve_batch`: repeatedly reduce the current sample batch down to one.
- `split_tile`: recursively split a tile into `ceil(width/2) x ceil(height/2)` quadrants.

If a one-pixel block still exceeds the budget, the glyph fails with `E_BUDGET_EXCEEDED` before a chunk is started. This is what guarantees that budget rejection cannot leave a half-written glyph.

At runtime, at most `parallelism` workers exist. Workers take the smallest pending submission index. A worker waits when `active + retained + nextEstimate` exceeds the global budget. Completed out-of-order tiles remain retained until the main thread can commit them in coordinate order.

## Stable composition

Each chunk owns a disjoint rectangle. The main thread commits only `nextCommit`; results for later chunks may finish first but cannot alter output order. Composition writes each pixel exactly once.

After all tiles commit, the engine rasterizes the full glyph as one region with the same placement and sample grid. A pixel difference above `seamTolerance` yields:

- glyph name,
- own chunk index,
- adjacent chunk index when the pixel touches a tile edge,
- pixel X/Y,
- tiled and truth bytes.

## Snapshot format

`state/<glyph>/snapshot.json` contains:

- format version,
- full raster/config fingerprint (font, glyph, fill rule, quantization, sampling, tiling, budgets, subpixel phase, bitmap size),
- deterministic chunk table,
- completed chunk count and contiguous completed flags,
- each committed chunk's SHA-256 content hash,
- a SHA-256 hash chain over the config hash and committed chunk records.

Commit sequence:

1. Worker computes a chunk in memory.
2. Main thread writes `chunks/<index>.cov` through `*.tmp` + `fsync` + atomic rename.
3. Main thread updates the snapshot and atomically replaces `snapshot.json`.
4. Only then does the chunk count as committed.

A hard kill between steps 2 and 3 may leave an orphan `.cov`; it is ignored because the snapshot does not list it. A kill after step 3 reuses it on resume. A missing, resized or hash-mismatched committed chunk, malformed version or broken hash chain returns `E_SNAPSHOT_MISMATCH`.

The CLI test hook `--hard-exit-after N` calls `std::_Exit(77)` after the Nth newly committed chunk has been durably snapshotted. It exists only to verify real forced-termination behavior.

## Atlas

Successful glyphs are packed deterministically in font order with a shelf allocator. Layout is validated entirely in memory before any atlas file is written. `atlas.json` reports:

- `offsetX`/`offsetY` (`atlasX`/`atlasY`),
- `step` (`advance`),
- `bearingX`/`bearingY`,
- source-unit `bbox`,
- per-glyph coverage content hash.

The image is a binary PGM (`P5`) alpha/coverage atlas. The Qt preview uses the in-memory grayscale coverage directly.

## GUI threading

The GUI owns one `FontEngine` per run and executes rasterization on a `std::thread`. Progress crosses to the UI through queued `QMetaObject::invokeMethod` calls. Cancel sets an atomic checked by every tile before and during scanline work. Atlas publication happens only after all requested glyphs complete; cancellation or failure leaves the previous `atlas.pgm`/`atlas.json` untouched.

## Determinism

No timestamps or wall-clock values enter output or trace. JSON object keys are serialized in sorted order. Chunk content hashes, trace records and atlas shelf placement are derived only from normalized geometry and the declared configuration. The reported `peakMemoryBytes` is a deterministic concurrency envelope (`sum of the largest <parallelism> chunk estimates`, capped by global budget); live observed active/peak memory is shown in GUI progress events.


# Rendering And Scheduling Design

## Pipeline

1. Decode and validate the binary scene.
2. Load and strictly validate JSON configuration.
3. Partition the screen into row-major tiles with stable submission sequence.
4. Plan every tile in submission order.
5. Select the first declared degradation stage whose exact sample plan fits the
   per-tile budget, otherwise place the tile in the waiting queue.
6. If all admitted, render tile tasks concurrently with one worker per tile.
7. Stitch in submission sequence and verify boundaries against a whole-screen
   single-block truth render.
8. Build deterministic summary/trace data and atomically replace output files.

## Sampling And Budget Control

The plan phase intersects camera rays with all media, builds ordered active-media
segments and performs a deterministic density-adaptive march:

- the first interval uses the configured `base_step * stage.step_multiplier`;
- the next step is `max(min_step, base_step*multiplier / (1 + 3*density))`;
- the phase inside each interval comes only from the global pixel coordinate;
- samples are visited in increasing segment/t order.

The plan stores the actual density, scattering, absorption and interval length
for each primary sample. Admission therefore knows the exact sample count before
accumulation starts. Shadow visibility samples are counted as
`primary_samples * stage.shadow_samples`.

`PlanBudgetExceeded` records the measured lower bound and moves to the next
declared stage. If every stage exceeds `sample_budget`, the tile is `WAITING` and
the render fails with `E_BUDGET_EXCEEDED` before any tile worker is launched.

## Parallelism And Ordering

Tiles can run in a `ThreadPoolExecutor` with exactly
`min(max_parallelism, admitted_tiles)` workers. `max_parallelism` is never raised
at runtime. Within a tile, pixels are visited in local row-major order and each
pixel's samples in increasing parameter order. Results are always stitched in
submission sequence, independent of worker completion order.

Every schedule record is emitted in submission order and includes all stage
trials, measured samples, budget, selected stage and budget ratio. Committed tile
records include tile id, grid bounds, stage, step multiplier, budget, sample
count and content hash.

## Scattering And Visibility

Active media are evaluated in sorted medium-id order. Their density-scaled
scattering/absorption coefficients are summed. Primary accumulation updates:

```text
L += T * sigma_s * phase * light * visibility * dt
T *= exp(-(sigma_s + sigma_a) * dt)
```

The preview uses the first active medium's Henyey-Greenstein phase function.
Shadow rays sample the scene bounding interval with the stage's fixed shadow
sample count and estimate transmittance from accumulated optical depth.
Preview PPM/PNG files apply a fixed exposure of `8.0` before Reinhard tone
mapping; floating-point scatter and transmittance buffers are unchanged.

## Seams

The sample phase function depends only on full-screen pixel coordinates, never
on tile offsets or workers:

```text
phase = 0.05 + 0.90 * ((x*73856093 XOR y*19349663 XOR 0x9E3779B9) & 0xFFFFFF) / 2^24
```

After tiled rendering, a separate whole-screen tile is rendered with
`seam.truth_stage`. Every interior boundary pixel is compared against the truth
for both scatter and transmittance. A mismatch above `seam.tolerance` raises
`E_SEAM_UNRESOLVED` with the tile pair, pixel coordinate, metric and both deltas.
The truth render has its own explicit `seam.truth_budget`; no production tile
budget is silently increased.

## Precision Paths

- `float` accumulates in `float64`.
- `fixed` uses Q20.20 integer accumulation for radiance and transmittance
  (`FIXED_SCALE = 2^20`).

When comparison is requested, both paths render the same scene and the renderer
produces per-pixel absolute deltas plus max/mean/RMSE/P95/per-channel statistics.
The analytical upper bound is based on the maximum per-pixel sample count and the
fixed-point quantum. It is stored in `summary.json` even without comparison.

## Incremental Rendering

Each tile stores:

- camera and light content hashes;
- parameter hashes of all active media;
- hashes of all density chunks touched by primary or shadow interpolation.

`IncrementalRenderer` compares old/new scenes and computes an affected tile set:

- camera or light changes invalidate all tiles;
- material-only changes invalidate tiles whose dependencies include the medium;
- geometry changes add the screen projection of old and new medium bounds;
- density chunk changes add the chunk's screen projection and all dependent
  tiles;
- topology changes, unknown changes or failed projections conservatively
  invalidate all tiles and record the reason.

Only affected tiles are rendered. Unaffected `TileResult` objects, arrays and
content hashes are reused. A new complete image and versioned `Snapshot` are
published only after every affected tile and optional seam check succeeds.
Concurrent readers receive either the previous published snapshot or the new
one, never a partially merged tile map. The snapshot stores a deep copy of the
scene so later in-place caller mutations cannot rewrite published history.

## Fault Isolation

`fault_injection.py` applies mutations after raw binary decode and before
semantic validation. Configuration injection is performed in the configuration
loader. Renderer, sampler and scheduler modules do not import injection code.
CLI rendering writes artifacts only after complete success.

## Error Summary And Hashes

The pixel hash covers scatter, transmittance and per-pixel sample counts.
Per-tile content hashes cover the same tile-local arrays. The summary hash is the
SHA-256 of canonical sorted JSON. Summaries and traces contain no timestamps,
thread IDs or completion-order data, so identical parameters produce identical
bytes.

# Technical Notes

## 1. Rendering pipeline

Each path owns one image resolution `(W, H)`. Every stage returns arrays on exactly that grid:

1. **Ray-cast base shading** — camera basis, per-pixel primary rays, nearest intersection against spheres, planes and axis-aligned boxes. Exact shading adds hard shadow rays, Blinn-Phong specular and rim light. Preview shading uses the same diffuse/shadow model at lower resolution but omits specular/rim and applies a fixed 1.07 exposure compensation to hit pixels.
2. **Tone posterization** — linear luminance maps to `floor(luminance * levels)` with deterministic clipping. Poster color keeps source hue while snapping to a per-level luminance/tint palette.
3. **Outline and strokes** — a level-boundary 4-neighbor mask is extracted from the graded map. A fixed normalized stroke guide produced at `96×72` drives seed selection, direction, length, bend, color and winding.

The final style is `poster color` composited with the brush RGBA layer, then dark ink applied to outline pixels.

Preview defaults: `120×90`. Exact defaults: `360×270` (exactly 3×). All three stage tensors are checked for identical shape through `NprFrame.validate_shapes()`.

## 2. Camera and clipping

Both paths read camera position, target, up vector, vertical FOV, near plane and far plane from the same immutable `Snapshot`. Preview cannot accept a non-latest version. Exact validates that the requested snapshot is still the latest at start, then keeps the immutable object for the entire tile run even if the UI increments the store version during rendering.

## 3. Stroke determinism

For guide cell `(gx, gy)` the global index is:

```text
stroke_id = gy * guide_width + gx
```

The presence, anchor jitter, angle, length perturbation, bend sign/amplitude, width perturbation, tint and winding order are all derived by SHA-512 over:

```text
npr-seed-v1|<scene_content_hash>|<stroke_id>|channels
```

The per-stroke fingerprint is a second hash over content hash, stroke index and the quantized channels. `verify_plan_seed_contract()` recomputes every retained descriptor. Thread IDs, clocks, PIDs and global RNG are never read. Cache keys are only `(content_hash, stroke_id)`.

The stroke guide is generated from scene + camera at a resolution independent of preview/exact output. Thus both paths see the same stroke descriptors. Preview only changes path sampling (`sample_factor=2`) and output resolution; exact samples every pixel-length step.

## 4. Brush rasterization

Each stroke is a quadratic-bend polyline. All selected descriptors in a cache-sized batch are converted to point arrays with NumPy, then rasterized with `bincount`.

Per-point brush weight is `clamp(width_norm * output_width, 0.05, 1.0)`. Accumulated weight becomes alpha, so a sub-pixel hairline at 2×/3× resolution represents the same normalized coverage as at 1×. This is why cross-resolution brush coverage is compared as alpha density rather than binary occupancy.

The cache budget uses an explicit condition-variable token ledger. A tile processes stroke batches no larger than the budget; acquisition blocks until enough cache slots exist, and release notifies all waiters. No stroke or tile is dropped.

## 5. Tiling and parallel schedule

Exact rendering uses row-major fixed tiles. Each tile shades a one-pixel halo, posterizes it, derives outline on the halo and crops. Image-border neighbors are suppressed, matching a full-image outline. Because ray casting, posterization and outline are pixel-local, tile output is independent of worker assignment.

At most `2 * max_workers` futures are in flight, so submission blocks instead of growing an unbounded queue. Completing futures are consumed in tile-number order by the coordinator and committed serially. Consequently single-threaded and multi-threaded runs produce identical commit order and pixels. The tests compare both.

Resource guards:

- `max_workers`: ThreadPool size.
- `thread_pixel_budget`: every padded tile area must fit.
- `stroke_cache_budget`: global number of cached stroke entries.

## 6. Checkpoint format

Session directory:

```text
persist/checkpoints/<scene-session>/session/
  manifest.json
  tiles/tile_00000.npz
```

Each tile NPZ contains:

- base RGB
- posterized RGB
- level map
- outline mask
- brush RGBA
- coverage mask
- styled RGB
- stroke IDs
- content hash and parameter version
- tile ID and origin

The coordinator writes the tile to a temporary file, flushes and fsyncs it, installs by `os.replace`, computes SHA-256, then atomically rewrites the manifest. A crash can leave an unreferenced tile, but never a manifest pointing at partial content.

Manifest identity includes scene content hash, parameter version, camera pose hash, full params hash, output size and tile size. Any mismatch during load invalidates the whole session. Missing or checksum-failing block payloads also invalidate the whole session.

Commit order is stored explicitly. Normal runs commit `0,1,2,...`; a retry after a failed tile appends the retried block after the unaffected committed blocks.

## 7. Consistency checks

Preview vs exact metrics are evaluated on the preview grid:

- **Level histogram**: total variation distance between normalized level histograms.
- **Outline coverage**: absolute difference of outline-pixel fractions after area max-pooling.
- **Stroke coverage**: absolute difference of mean brush alpha after area averaging.
- **Outline position**: IoU after one-pixel dilation of both masks.

Each report also records the first mismatching comparison pixel mapped back to exact-image coordinates and its tile ID.

Three-path verification compares full-resolution exact arrays:

- interactive exact,
- paused / restarted resumed exact,
- one-shot exact.

Styled RGB, level map, outline and coverage must be exactly equal. Mismatch raises `E_PATH` and records the first pixel and tile.

## 8. UI interaction

The Tk UI renders preview synchronously on drag/slider events; measured warm p95 is under 16ms. The first deterministic stroke plan is built during startup warmup. A 260ms debounce schedules exact rendering in a single background executor. Results arrive through a queue polled by Tk, avoiding cross-thread Tk calls.

UI state stores scene, current version, camera and params. On restart the same version is restored so a matching checkpoint can resume. The Resume button is shown when a matching manifest exists, a render was paused/canceled, or failed tiles remain.

## 9. Deliberate limitations

- The renderer is CPU-only NumPy for reproducibility; PyOpenGL is not used.
- Preview/exact cross-resolution equality is semantic/metric, not pixel identity. Exact three-path equality is pixel identity.
- A forced process kill can leave only previously committed blocks; no complete-image guarantee is made.
- Outline is defined from posterized level boundaries, as required by the task, rather than depth/silhouette edges.

# Architecture

## Data Flow

```text
sample generator
  ├─ foreground-composite/v1 layer document
  ├─ foreground-composite/v2 layer document
  ├─ foreground-composite/v3 layer document
  ├─ per-frame instructions
  └─ sampled truth + visibility endpoints + rational timestamps
          │
          ▼
version loaders ── E_VER / E_SCHEMA / E_DECODE / E_LAYER
          │
          ▼
canonical Model (ordered bottom→top stack, rational time base, source versions)
          │
          ├──────────────► deterministic version export + degradation traces
          │
          ▼
window state machine (pending → compositing → committed | failed)
          │
          ├─ bounded worker batches
          ├─ linear-light alpha compositing
          ├─ atomic zlib-compressed .vspw frame-window packet
          └─ serialized SQLite checkpoint transaction
```

## Canonical Model

- `Canvas`: width, height, channels and working color space.
- `TimeBase`: reduced rational frame period, source unit and optional tick rate.
- `Layers`: ID, kind, source version, boundaries, visibility, opacity, alpha,
  color encoding, premultiplication convention and procedural mask.
- `Stack`: explicit bottom-to-top render order.
- `Instructions`: sparse per-frame overrides for opacity, alpha, color or
  visibility.
- `SourceVersions` / `Sources`: source-format provenance retained through loads.

Layers are sorted by ID only in canonical snapshots; render order always comes
from `Stack`. This makes hashes and exports independent of map iteration and input
ordering.

## Composition

For every frame and every stack entry:

1. Apply lifecycle and sparse per-frame override.
2. If source alpha is premultiplied, unpremultiply by source alpha.
3. If source color is sRGB, apply the exact piecewise sRGB EOTF.
4. Scale source alpha by layer opacity and mask coverage.
5. Composite in premultiplied linear space:
   `out = src + dst * (1 - src.a)`.
6. Convert the result to straight linear alpha for the internal frame.

The output timestamp is `frameIndex × reduced frame period`; no floating-point
timestamp accumulation is used.

## Window State Machine

```text
pending ──begin──► compositing ──commit──► committed
   ▲                    │                     │
   │                    └──fail──► failed      │
   │                               │            │
   └──── explicit invalidate ◄─────┘            │
                    retry ──────────────────────┘
```

Allowed transitions are centralized in `legalTransition`. An illegal transition
returns `E_STATE` without modifying the window. A committed snapshot includes:

- copied layer parameter snapshot,
- source versions/time base/canvas snapshot,
- model/source/algorithm hashes,
- all per-frame summaries and the atomic window packet hash.

## Incremental Invalidation

For a layer edit, affected frames are the union of the layer's old and new
lifecycles plus explicit per-frame instruction frames. Only windows intersecting
that set are invalidated. The returned `InvalidationList` contains window number,
layer IDs, reason, downstream stage, and the committed windows that were reused.

Time-base or source-version changes alter the immutable source fingerprint and
therefore invalidate/purge every checkpoint before recomputation.

Before recomputation the engine persists the invalidation intent, then the edited
model. After a crash, a new model can never silently trust old snapshots: either
the pending invalidation remains, or the fingerprint mismatch purges the cache.

## SQLite Checkpoints

`window_snapshots` stores one row per committed window:

```text
window_index, start_frame, end_frame, frame_hash,
summary_json, frame_summaries_json, layers_json, source_json,
model_hash, source_hash, algorithm_version, frame_path,
committed_sequence, committed_at
```

Each row is written in one transaction through one SQLite connection and a writer
mutex. Recovery runs before any window enters committed state:

1. compare database metadata to the current algorithm/source fingerprint,
2. load all rows in stable window-index order,
3. compare bounds, source hash and algorithm version,
4. compare the persisted compressed-file SHA-256,
5. decompress and compare the canonical packet content hash,
6. only then restore committed windows.

Any failure clears both tables and frame packets. The caller starts from an empty
cache and recomputes all windows.

## Concurrency and Resource Bounds

- `MaxWorkers` must be in `[1,64]`.
- Computation proceeds in bounded batches of at most `MaxWorkers` windows; a
  thread never owns more than one active frame composition at a time.
- `MaxFrameBytes` rejects a frame before allocation if channel/dimension size is
  over the configured per-frame buffer limit.
- SQLite writes are serialized independently of worker scheduling.
- A failed/canceled window does not clear other committed windows.

## Export

Target versions 1, 2 and 3 are all supported. The exporter:

- preserves stack order and visibility state,
- normalizes source colors when the target has a different global alpha convention,
- flattens hierarchy only for v1 and records an `E_LOSSY` trace,
- converts rational time bases exactly to seconds or integer ticks,
- emits stable indented JSON with a SHA-256 content hash.

Round-trip tests reload the target document through the matching loader and compare
stack order plus per-layer visibility.

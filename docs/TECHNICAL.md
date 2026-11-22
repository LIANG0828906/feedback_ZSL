# Technical Design

## 1. Pipeline Shape

The implementation is a deterministic local pipeline:

```text
vpfc/1 + vpfc/2 + vpfc/3 layer documents
                 |
                 v
      version adapters + validation
                 |
                 v
 internal SceneModel (linear straight RGBA, integer frames, rational fps)
                 |
                 v
 fixed 3-frame windows -> state machine -> ordered commit
                 |
                 +--> atomic checkpoint store
                 +--> incremental/full pixel comparison
                 +--> version-specific deterministic export
                 +--> PySide6 operator UI
```

The production package is `vp_pipeline`. The deterministic sample producer is
`scripts/generate_samples.py`; it contains an independent reference
compositor for sample truth.

## 2. Canonical Internal Model

`SceneModel` contains:

- `scene_id`, source version, canvas width/height/channels
- integer frame count, fixed window size, rational frame rate
- source time unit and original time-basis representation
- stack-ordered `Layer` values
- the complete source-field mapping trace

Each layer stores:

- stable layer id, kind, name, stack index, optional parent id
- straight linear RGB in `[0,1]`
- straight alpha and opacity
- original alpha convention and source transfer
- visibility boolean and half-open frame interval
- integer pixel bounds
- source version

Parent links are organizational. Effective visibility is the logical AND of
the layer and all ancestors. Stack order remains an independent global order,
which is why flattening a hierarchy cannot silently reorder draw calls.

## 3. Color, Alpha and Frame Encoding

The compositor stores light in linear space and composites from bottom to top:

```text
source_premultiplied = source_linear_straight * (alpha * opacity)
out_rgb = source_premultiplied + dst_rgb * (1 - source_alpha)
out_alpha = source_alpha + dst_alpha * (1 - source_alpha)
```

Input conversion rules:

- straight transfer-encoded RGB is decoded to linear RGB;
- premultiplied transfer-encoded RGB is decoded per channel and divided by
  alpha to recover straight linear RGB;
- a transparent premultiplied sample must have zero RGB;
- premultiplied RGB above alpha is rejected as inconsistent rather than
  repaired.

For the `srgb` transfer, premultiplication is interpreted as encoded after
linear premultiplication. That convention is implemented symmetrically by the
loader and exporter.

Output frames are RGBA8. Linear straight RGB is encoded to sRGB, clipped, then
quantized with `floor(x * 255 + 0.5)`. This makes repeated renders byte-equal.

## 4. Format Differences

| Concern | `vpfc/1` | `vpfc/2` | `vpfc/3` |
| --- | --- | --- | --- |
| Version field | `format.version` | `schema.revision` | `document.revision` |
| Naming | snake_case | camelCase | lower camel with nested objects |
| Time | frame count + rational fps | ticks/sec + ticks/frame | seconds per frame |
| Alpha | one global premultiplied convention | per-layer `straight`/`premultiplied` | per-layer alpha object |
| Transfer | global sRGB | per-layer `srgb`/`linear` | per-layer color object |
| Stacking | explicit `order` | `zIndex` | `stackIndex` |
| Hierarchy | absent | parent references | nested children |
| Visibility | boolean + frame interval | tick interval | frame interval |

Unknown versions return `E_VER`. Missing or semantically undecidable fields
return `E_SCHEMA` at the exact source path. JSON decode failures return
`E_DECODE`. The loader never chooses a fallback alpha convention.

## 5. Window State Machine

Allowed transitions:

```text
pending    -> computing
computing  -> committed | failed
failed     -> computing
committed  -> pending   (explicit invalidation only)
failed     -> pending   (explicit invalidation only)
```

Any other transition returns `E_STATE` and leaves the state unchanged.

A commit stores the relevant layer snapshot, source version, frame hashes and
checkpoint hash. An edit does not overwrite a committed frame. It marks the
window pending and preserves the previous committed snapshot and frame data
until a new compute is explicitly run and committed.

## 6. Incremental Invalidation

For every window, the session builds a deterministic snapshot containing:

- scene identity, canvas, timeline and source version;
- layers that are effectively visible in any frame of the window;
- ancestors needed to express inherited visibility.

An edit is applied to a frozen candidate `SceneModel`, validated, and compared
against the old model. A window is invalidated only when its snapshot changes.
This handles alpha, color, opacity, bounds, visibility, parent and time-base
changes without guessing ranges.

An edit always invalidates the downstream export stage. The invalidation
record contains window number, changed layer ids, reason and stage.

Unchanged committed windows are reused. The exact comparison utility reports
the first row-major mismatch with frame, window, pixel coordinate, channel and
topmost covering layer, and raises `E_DIFF`.

## 7. Checkpoint and Recovery

Each committed window is written as:

1. a raw contiguous RGBA frame file, atomically replaced;
2. a manifest with frame summaries, pixel hashes, content hashes, layer
   snapshot, source version and algorithm version, atomically replaced.

An inter-process file lock plus an in-process lock serializes all checkpoint
I/O. Commits from parallel workers are applied in ascending window order.

Recovery is all-or-nothing. The manifest content hash, algorithm version,
source version and scene id must match. Every frame and window hash is
rechecked, and every restored window snapshot is compared with the current
model. Any mismatch discards the whole snapshot; mixed old and new windows are
forbidden. A process that stopped after two durable commits can recover only
those two windows because the atomic manifest contains only those records.

The base model fingerprint is retained for audit but is not a replacement for
snapshot validation. If an edited model has been incrementally recomputed so
that every window snapshot matches the current model, all windows can be
recovered after restart. If any dirty window was never recomputed, that
snapshot mismatch discards the entire cache.

## 8. Concurrency and Resource Bounds

- worker count is bounded to `1..8`;
- working frame buffers per thread are bounded to `1..4`;
- each worker renders one frame working buffer at a time;
- checkpoint writes are serialized;
- commits are emitted in stable ascending window order, regardless of worker
  completion order;
- a worker exception marks only its window failed; other windows continue and
  can commit;
- failed windows can be retried.

Rendering each frame is independent, uses `float64` arithmetic, and writes a
new buffer. Worker identity and completion order therefore cannot affect frame
bytes.

## 9. Cross-Version Export

Export writes:

- `scene.json`: a target-version document readable by the target loader;
- `frames.bin`: sorted raw RGBA frames;
- `frames_meta.json`: offsets, sizes, pixel hashes and frame statistics;
- `export_report.json`: warning list and degradation trace.

JSON uses sorted keys, fixed indentation, UTF-8 and no timestamps. Fixed file
names and sortable frame order make repeated export byte-stable.

`vpfc/1` cannot preserve hierarchy, per-layer straight alpha, or linear source
storage. Those cases produce `E_LOSSY` warnings and explicit traces describing
the conversion or flattening. Effective visibility is intersected across
ancestors when hierarchy is flattened. Layer draw order and rational frame rate
remain unchanged.

`vpfc/2` can express per-layer alpha, transfer and parent references. `vpfc/3`
can express nested hierarchy, per-layer alpha/transfer and independent stack
order.

## 10. Error Codes

| Code | Meaning |
| --- | --- |
| `E_VER` | Unknown source or target format version |
| `E_SCHEMA` | Missing/inconsistent required schema or undecidable semantics |
| `E_PARA` | Out-of-range API/UI parameter, with field path |
| `E_STATE` | Illegal window transition or invalid operation for current state |
| `E_LOSSY` | Export warning code, not a fatal exception |
| `E_DIFF` | Pixel comparison failure with localized first mismatch |
| `E_DECODE` | Corrupt/unreadable local JSON or frame pack |
| `E_IO` | Atomic checkpoint or export write failure |
| `E_INTERNAL` | Isolated unexpected worker failure payload |

## 11. Verification Coverage

The unit suite covers:

- fixed-seed generator reproducibility and manifest hashes;
- three-version loading, truth mappings and reference pixels;
- malformed version/schema/alpha/JSON errors;
- cross-version frame equality and alpha/transfer conversion;
- deterministic repeated rendering;
- legal/illegal state transitions and snapshot preservation;
- incremental invalidation scope, reuse and pixel equality to full compute;
- first-diff localization and `E_DIFF`;
- restart recovery, partial durable recovery and whole-snapshot discard;
- serial/parallel equality, failure isolation and retry;
- 3x3 export interoperability, deterministic bytes and `E_LOSSY` traces;
- Qt offscreen UI workflow.

See `docs/TEST_REPORT.md` for the recorded run on this machine.

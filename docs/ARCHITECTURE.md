# Architecture

## 1. Deterministic data plane

`internal/generator` never reads analyzer output. For each `(seed, sampleIndex, channelIndex)` it derives an LCG stream and constructs:

```text
IR[n] = directImpulse[n] + sum(earlyReflections[n]) + exponentialTail[n]
```

The generated manifest stores all samples and their construction-time truth:

```json
{
  "sampleRate": 48000,
  "layout": {"name": "arena-5.1", "channels": ["FL", "FR", "C", "LFE", "SL", "SR"]},
  "irSamples": 4096,
  "channels": [{
    "samples": [0.0],
    "truth": {
      "directArrival": 64,
      "directEnergy": 0.5625,
      "reflections": [{"arrival": 136, "energy": 0.03}],
      "drrDb": 5.589,
      "itdSamples": 0
    }
  }],
  "dry": {"description": "...", "samples": [0.0]}
}
```

`internal/loader` parses JSON through a raw-object pass so absent fields are distinguishable from zero values. Validation covers required fields, channel uniqueness, layout/channel equality, point counts, finite samples, sample-rate interval, and strict arrival monotonicity.

## 2. Framing and estimation

Given `frameLength`, `hop`, `windowLength`, and `quantStep`:

```text
frameCount = ceil(len(dry.Samples) / hop)
analysisSpan(f) = [f*hop, min(f*hop + frameLength, irSamples))
```

For every channel the estimator:

1. Finds the maximum absolute sample on the global grid `n % quantStep == 0` inside the analysis span.
2. Records the absolute direct arrival and frame-relative offset.
3. Integrates squared samples in `[direct+1, min(direct+1+windowLength, frameEnd))`.
4. Clips the integration width at the frame end; no zero padding is inserted.

The function is pure with respect to `(sample, params, frame)`. Parallel completion order cannot affect results because channels are emitted in layout order and all arithmetic is local.

`E_PARA` carries a specific field name for invalid `frameLength`, `hop`, `windowLength`, `quantStep`, `interpOrder`, worker count, buffer size, or tolerance.

## 3. Reconstruction

For channel `c` and frame `f`:

```text
ITD[c] = directArrival[c] - directArrival[reference]
DRR[c] = 10*log10(max(directEnergy[c], eps) / max(earlyEnergy[c], eps))
smoothedEarly[c] = w*previousSmoothedEarly + (1-w)*earlyEnergy[c]
```

The previous state is the immediately preceding frame's committed reconstruction state. This is the cross-frame numeric coupling. State also stores cumulative direct/early energy and cumulative error bounds.

The direction diagram uses:

```text
angle = asin(clamp(ITD / windowLength, -1, 1))
```

## 4. Error-bound propagation

Bounds are functions of observed parameters only:

| Item | Current bound |
|---|---|
| Direct quantization | `quantStep / 2` samples |
| Early-energy truncation | `(windowLength - integratedWidth) / windowLength` |
| ITD | `quantStep` samples |
| Convolution interpolation | `0.5^(interpOrder+1)` amplitude |

Each item has:

- `current`: the current frame's worst-case bound;
- `cumulative`: previous cumulative + current.

Both current and cumulative values are compared to `params.ErrorTolerance`. Failure returns `E_ERR` with `frame` and `item`. The bound calculation never receives truth fields or output samples.

## 5. Rendering

For each frame:

1. Select the dry chunk `[frame*hop, min(frame*hop+hop, len(dry)))`.
2. Build a local IR from reconstructed direct delay, direct energy, smoothed early energy, and DRR-derived tail energy.
3. Place fractional early-reflection taps through Lagrange interpolation of order `interpOrder`.
4. Perform causal convolution and evaluate only output indices `0..len(dryChunk)-1`.
5. Route each source channel to the configured output index.

The output for every channel is exactly `len(dry.Samples)` samples after concatenating frame chunks. The convolution's causal tail outside a chunk is intentionally not emitted, so no boundary zero padding enters the stream.

Invalid routing returns `E_LAYOUT` with the source channel ID. Effective gain outside `[0,1]` returns `E_CLIP` with frame and channel.

## 6. Frame state machine

States:

- `pending`
- `estimated`
- `committed`
- `failed`

Legal transitions:

```text
pending   -> estimated | failed
estimated -> committed | pending | failed
failed    -> pending
committed -> (terminal)
```

`CommitFrame` requires the next sequential index and `estimated` state. A failed commit leaves the frame in its prior state. A committed frame is represented by an immutable snapshot. `ReestimateUncommitted` cannot cross the committed boundary.

Parameter changes are constrained by checkpoint generation. After any frame is committed, a different parameter hash would create a mixed-generation checkpoint, so it is rejected with `E_FROZEN`; adjustments of the same generation may reset only uncommitted frames. This keeps “committed frames frozen” and “no partial checkpoint mixing” simultaneously true.

## 7. SQLite generation and invalidation

Schema:

```text
checkpoint_meta(
  id=1,
  input_hash,
  params_hash,
  algorithm_version,
  layout_hash,
  created_at
)

committed_frames(
  frame_index PRIMARY KEY,
  seq UNIQUE,
  input_hash,
  params_hash,
  algorithm_version,
  layout_hash,
  params_json,
  result_json,
  render_blob,
  committed_at
)
```

All writes are serialized by:

- one `database/sql` connection (`SetMaxOpenConns(1)`);
- a package-level mutex;
- one SQLite transaction per committed frame;
- `seq` and `frame_index` continuity checks inside the transaction.

`render_blob` uses a versioned little-endian float64 layout so persisted samples round-trip bit-exactly.

On load, metadata and every row are checked against the active generation. Any mismatch triggers one transaction that deletes all committed frames and metadata. The caller receives `Invalidated=true` and the reason, then starts a full recompute from frame 0.

## 8. Three-path verification

`internal/verify.RunThreePaths` executes:

1. Sequential realtime processing with checkpoint writes.
2. Partial processing, close/reopen, resume from the same SQLite file.
3. Load persisted parameters, estimate in parallel batches, then reconstruct/render/commit sequentially.

Comparison checks:

- commit sequence;
- per-frame parameter/input/algorithm hashes;
- direct arrival/energy;
- reconstructed delay/ITD/DRR/direction;
- current and cumulative bounds;
- cumulative state;
- every rendered float64 bit.

`E_DIFF` reports frame, channel, field, and left/right values.

## 9. Concurrency and buffers

`ProcessAndCommitBatch` can estimate up to N frames concurrently:

- at most `maxWorkers` goroutines hold worker tokens;
- each task acquires a sample-buffer weight of `analysisSpan * channelCount`;
- total budget = `maxWorkers * bufferSamplesPerWorker`;
- a request larger than the per-worker limit fails with `E_FRAME`;
- when the budget is exhausted, the task blocks and records a timestamped `sample-buffer-exhausted` event with request/used/capacity.

Estimation is parallel; reconstruction and commit remain sequential to preserve the committed-prefix state chain.

## 10. Diagnostics and desktop

The Fyne panel uses the same `session` API as the CLI:

- frame table: state, sequence, parameters, delays, energy, ITD, DRR, cumulative bounds, error;
- ITD-derived direction diagram;
- per-channel early-energy and direct-delay curves;
- waveform overlay for realtime/resume/recompute;
- parameter, routing, anomaly, retry, audition, and pause controls.

Windows audition uses `winmm.dll` MCI commands (`open`, `play`, `pause`, `close`). Linux/macOS fallback uses `ffplay` or `xdg-open`; its “pause” action stops the external process.

The Fyne code is isolated with `//go:build fyne || windows`. Windows builds launch it by default; Linux/CI builds opt in with `-tags fyne`. A headless Fyne test app constructs the panel, loads a generated sample, commits a frame, and verifies the plot/table objects without needing a display server.

## 11. Intentional boundary decisions

- The direct-arrival estimate is frame-local. Later reverberant frames may contain no global direct impulse, so their peak is the current frame's strongest quantized path candidate. This is required to keep per-frame diagnostics independent.
- Rendering emits exactly the dry-frame span and drops the causal tail beyond that span. This is the only way to satisfy both “general convolution” and “output length strictly equals dry length” without out-of-range zero padding.
- Checkpoint generation changes after a commit are rejected rather than mixing old and new parameter versions. Stale generations on restart are invalidated as a whole.

# biomemaster

`biomemaster` is a deterministic offline synthesizer and mastering renderer for
biome-scene projects. It reads a JSON project, loops and crossfades layer WAVs,
mixes biome buses, applies soft limiting, performs ITU-R BS.1770-4 loudness
normalization and 4x oversampled true-peak protection, then writes a 24-bit PCM
WAV master plus a fixed-precision QC report.

The render is block based and resumable. A render interrupted by Ctrl+C exits
with code 0 after checkpointing the current block. A forced kill can be resumed
from the last durable checkpoint. The resulting WAV is byte-identical to an
uninterrupted render.

## Requirements

- Python 3.10+
- `numpy>=1.24`
- `scipy>=1.10`
- `soundfile>=0.12`

Install locally:

```bash
python -m venv .venv
source .venv/bin/activate        # Windows: .venv\Scripts\activate
python -m pip install -r requirements.txt
python -m pip install -e .
```

No external samples or network resources are needed. The included generator
creates all layer WAVs and the project JSON deterministically.

## Quick Start

```bash
python tools/generate_sample.py --output artifacts/demo --duration 6

biomemaster render artifacts/demo/project.json \
  --output artifacts/demo/master.wav \
  --report artifacts/demo/qc.json \
  --workers 4

biomemaster verify \
  --report artifacts/demo/qc.json \
  --wav artifacts/demo/master.wav
```

Without installation, prefix commands with `PYTHONPATH=src` and use
`python -m biomemaster`.

The default checkpoint directory is `<output>.work`. It is removed after a
successful render. Use `--work-dir` to place it elsewhere.

## Project Schema

Downward-compatible fields are intentionally strict: malformed projects are
rejected rather than repaired.

```json
{
  "schema_version": 1,
  "sample_rate": 48000,
  "duration_seconds": 12.5,
  "output_channels": "stereo",
  "target_loudness_lufs": -23.0,
  "biomes": [
    {
      "name": "forest",
      "weight": 1.0,
      "layers": [
        {
          "name": "wind",
          "path": "layers/forest_wind.wav",
          "loop_start": 0,
          "loop_end": 72017,
          "base_gain_db": -1.0,
          "curve": [
            {"time": 0.0, "gain_db": 0.0},
            {"time": 4.0, "gain_db": -3.0},
            {"time": 8.0, "gain_db": -12.0}
          ]
        }
      ]
    }
  ]
}
```

- `output_channels` accepts `"stereo"` or `2`.
- `path` is resolved relative to the project JSON.
- Layer files must be mono or stereo and match the project sample rate.
- `loop_start` and `loop_end` are sample indices with
  `0 <= loop_start < loop_end <= file frames`.
- The loop interval must be longer than the 20 ms crossfade.
- Curve nodes use seconds and dB. Times must be non-decreasing; gains must be
  in `[-96, 12]`. Equal-time nodes keep the last value.
- Layer gain is
  `10 ** ((base_gain_db + interpolated_curve_db) / 20)`, so base and dynamic
  gain are added in the dB domain before conversion.
- `weight` is a linear bus weight. `weight_db` may be used instead and is
  converted with `10 ** (weight_db / 20)`. Do not specify both.

## Deterministic Rendering

Rendering advances in fixed 8192-sample chunks. Within a chunk:

1. Every biome renders its layers in project order. Layers inside a biome are
   strictly serial.
2. Biome workers run in parallel when `--workers > 1`. Futures are collected
   and mixed in project order, so thread scheduling cannot reorder audio.
3. The mixed bus is soft limited with a -3 dBFS threshold and 6 dB knee. The
   knee starts at -6 dBFS, reaches -3 dBFS at 0 dBFS input, and is capped at
   -3 dBFS above that point.
4. The float32 shard representation is treated as the canonical intermediate.
   It is written and fsynced, then the loudness ledger is advanced from those
   exact float32 values.

### Loop Crossfades

For source loop length `L` and crossfade length `C` (20 ms), adjacent loop
segments are overlap-added with period `L - C`. The tail contributes
`cos`-shaped weight and the next head contributes `sin`-shaped weight, so the
squared weights sum to one. This handles partial final cycles without padding
or truncating the requested output length. The output has exactly
`round(duration_seconds * sample_rate)` samples, i.e. within half a sample of
the requested duration.

### Loudness

The meter implements ITU-R BS.1770-4:

- K-weighting high shelf plus RLB high-pass;
- 400 ms blocks with 100 ms hop (75% overlap);
- -70 LUFS absolute gate and -10 LU relative gate.

Only full 400 ms blocks participate. For projects shorter than 400 ms, all
available energy is treated as one block so normalization remains defined.
Loudness is measured after the fixed soft limiter. The final gain is corrected
iteratively after rendering the candidate master, because the true-peak safety
limiter can change loudness slightly.

### True Peak

True peak is measured by 4x polyphase oversampling with overlapping padded
blocks. The final sample-domain limiter uses a smooth `tanh` transfer starting
at a knee below its ceiling. If measurement still exceeds -1.0 dBTP, the
ceiling is lowered and the candidate is regenerated. Finalization succeeds only
when both conditions hold:

- integrated loudness is within 0.3 LU of the target;
- 4x true peak is at or below -1.0 dBTP.

## Checkpoint and Resume Protocol

The work directory contains:

- `shard_XXXXXXXX.f32`: interleaved little-endian stereo float32 blocks;
- `loudness_ledger.bin`: 16-byte little-endian float64 hop-energy records;
- `checkpoint.json`: project hashes, completed blocks, shard-chain digest,
  latest-shard digest, ledger digest, filter state, and loudness intermediate.

After each block, the shard and ledger are fsynced, then the checkpoint is
replaced atomically. The checkpoint stores the loudness state both before and
after the latest shard. On resume:

1. All committed shards are rehashed and the chain must match.
2. The latest shard is reprocessed from the prior filter state.
3. The resulting filter state, hop counters, ledger digest, and intermediate
   loudness must exactly equal the checkpoint.
4. A mismatch is rejected with `E_CHECKPOINT_CORRUPT` and exit code 2.
5. Uncommitted shards and uncommitted ledger tails are removed.
6. A syntactically malformed checkpoint or a project/content hash mismatch
   causes the work directory artifacts to be discarded and synthesis to restart.

SIGINT/SIGTERM/SIGBREAK set a cooperative stop flag. The active 8192-sample
block finishes, its checkpoint is persisted, and the process exits with code 0.
Forced termination needs no cleanup: recovery only trusts persisted state.

## Memory Control

The global working-memory budget is 512 MB. Before loading loop audio, the
renderer estimates loop storage plus fixed and per-worker working buffers. An
estimate above the budget is rejected before rendering with
`E_MEMORY_LIMIT` and exit code 2.

## Errors

Business errors are one-line JSON objects on stderr:

```json
{"ok":false,"code":"E_PROJECT_INVALID","message":"...","details":{}}
```

- `E_PROJECT_INVALID`: malformed JSON, invalid schema/curve/loop, missing file,
  sample-rate mismatch, unusable audio. Exit code 2.
- `E_MEMORY_LIMIT`: estimated working set exceeds 512 MB. Exit code 2.
- `E_CHECKPOINT_CORRUPT`: committed shard, ledger, or loudness intermediate
  does not match the checkpoint. Exit code 2.
- `E_RENDER_FAILED`: internal or target-convergence failure. Exit code 1.

## QC Report

The report is deterministic JSON with sorted object keys. Every floating-point
value is emitted with exactly six decimal places. It includes:

- project and referenced-content SHA-256 values;
- WAV SHA-256, byte size, format, subtype, sample rate, and channels;
- total samples, duration, biome layer counts, and total layers;
- soft/final limiter trigger samples and frames;
- crossfade count, shard count, and checkpoint resume count;
- final integrated loudness, true peak, deviation, and normalization gain.

`biomemaster verify` hashes the WAV, checks its stream metadata, remeasures
loudness and true peak, and confirms the report values and true-peak ceiling.

## Tests

```bash
PYTHONPATH=src python -m unittest discover -s tests -v
```

The suite covers validation errors, BS.1770 relative-level behavior, soft and
true-peak limiting, loop-length preservation, worker-order determinism, SIGINT
resume, SIGKILL recovery, malformed-checkpoint restart, loudness-mismatch
rejection, fixed-precision QC, and report/WAV verification. See
`VERIFICATION.md` for the recorded run.

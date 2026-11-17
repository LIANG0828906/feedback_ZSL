# Festival Documentary Stereo Master

Deterministic offline mastering for six mono festival-stage sources. The package
synthesizes the source WAVs, validates a JSON cue sheet, aligns the channels,
renders an analytic ITD/ILD binaural image, mixes the three program groups,
meets a BS.1770 integrated-loudness target and true-peak ceiling, and commits
resume progress block by block.

No network access or external media are used. Python 3.10+, NumPy, and SciPy are
the only runtime dependencies.

## Setup

```bash
uv venv --python 3.10 .venv
uv pip install --python .venv/bin/python -r requirements.txt pytest
```

The current workspace already has a Python 3.14 environment in `.venv/`.

## End-to-End Run

```bash
.venv/bin/python -m festival_master demo \
  --workspace deliverable \
  --duration 7.0 \
  --block-samples 4096
```

This creates:

- `deliverable/manifest.json`
- `deliverable/audio/*.wav` (six deterministic 24-bit mono sources)
- `deliverable/master.wav` (deliverable stereo master)
- `deliverable/master_report.json` (channel and master verification)
- `deliverable/generation_report.json` (source hashes, azimuths, offsets)
- `deliverable/work/snapshot.json` and committed block files

The separate commands are equivalent:

```bash
.venv/bin/python -m festival_master generate --manifest manifest.json
.venv/bin/python -m festival_master render \
  --manifest manifest.json \
  --output master.wav \
  --report master_report.json \
  --work-dir .master.work
```

`render` deliberately does **not** generate missing sources. It returns
`E_MISSING_FILE`; use `generate` first. `render --fresh` discards committed
progress.

## Manifest

See `examples/manifest.json` for a complete file. Required values are:

- top-level integer `seed`
- top-level or per-channel `sample_rate`, `bit_depth`, and `num_channels`
  (defaults: 48000, 24, 1)
- top-level `target_loudness_lufs` (alias: `target_lufs`, `target_loudness`)
- optional `true_peak_limit_dbtp` (default `-1.0`)
- optional `block_samples` (default 2048)
- `stage` bounds in metres using `x`, `y`, `z` range pairs
- `listener` position (default `[0, -0.5, 1.7]`)
- exactly six `channels`, one each for `主唱`, `吉他`, `贝斯`, `鼓组`, `环境`,
  `观众` (English aliases are accepted)
- per-channel `id`, `instrument`, `path`, `anchor`, and a start offset in
  integer samples (`start_offset_samples`) or milliseconds
  (`start_offset_ms`)
- optional per-channel `end_offset_samples`, `downmix_weight`, and `group`

`groups` may contain `人声组`, `乐器组`, and `环境组`; each accepts `gain_db`.
`group_gains_db` is also accepted. A missing group gain defaults to `0 dB`.

The source coordinate convention is X right, Y forward, Z up. Azimuth is
`atan2(x - listener_x, y - listener_y)`. The source WAVs contain their musical
content from the beginning; the cue-sheet start offset is the position of that
content on the unified master timeline. Aligning therefore prepends zeros by
`declared_start_offset - detected_onset`.

## Signal Path

1. The cue sheet is parsed without depending on formatting; its canonical JSON
   SHA-256 is the manifest summary.
2. WAV headers and PCM payloads are inspected directly. Sample rate, bit depth,
   channel count, path presence, and frame alignment are checked.
3. Six sources are generated with independent deterministic PCG64 streams:
   vocal notes, guitar plucks, bass notes, drum transients, broadband stage
   ambience, and crowd applause noise.
4. Each source onset is detected and placed at its declared sample offset. The
   master timeline is the longest aligned channel; channels are zero-padded to
   that length without changing relative timing.
5. A static analytic binaural model uses equal-power ILD and a smooth
   `0.65 ms * sin(azimuth)` ITD. The far ear receives a fractional causal delay;
   no hard left/right switch is introduced.
6. Group gains and per-channel downmix weights are applied before the stereo
   sum. Progress is written in fixed-size sample blocks.
7. The mix is normalized with iterative BS.1770 gating and a look-ahead limiter.
   The limiter uses future gain without adding output latency, so the sample
   count is unchanged. A final 24-bit (or manifest-selected) PCM encoding is
   measured again before the report is committed.

## Structured Errors

Errors are emitted as one JSON object on stdout and exit non-zero. Every issue
has a `code`, `channel`, and `message`.

| Code | Exit | Meaning |
| --- | ---: | --- |
| `E_MANIFEST_JSON` | 2 | unreadable or invalid JSON |
| `E_MANIFEST_SCHEMA` | 2 | missing/invalid structural field |
| `E_SEED` | 2 | seed is not an integer |
| `E_INSTRUMENT_SET` | 2 | not exactly the required six instruments |
| `E_CHANNEL_ID` | 2 | duplicate channel id |
| `E_OFFSET` | 2 | invalid or unreachable time offset |
| `E_GROUP` | 2 | invalid group or downmix weight |
| `E_MISSING_FILE` | 3 | manifest file or channel WAV missing |
| `E_AUDIO_READ` | 4 | corrupt/unsupported WAV |
| `E_SAMPLE_RATE` | 4 | WAV sample rate differs from the cue sheet |
| `E_BIT_DEPTH` | 4 | WAV bit depth differs from the cue sheet |
| `E_CHANNELS` | 4 | WAV channel count differs from the cue sheet |
| `E_DUPLICATE_INSTRUMENT` | 5 | repeated instrument role |
| `E_ANCHOR_MISSING` | 6 | missing/invalid anchor |
| `E_ANCHOR_RANGE` | 6 | anchor outside stage bounds |
| `E_TARGET_LOUDNESS` | 7 | target loudness missing/invalid |
| `E_TRUE_PEAK_LIMIT` | 7 | invalid true-peak ceiling |
| `E_SILENT_MASTER` | 8 | no measurable program loudness |
| `E_LOUDNESS_UNACHIEVABLE` | 8 | target and ceiling cannot both be met |
| `E_OUTPUT` | 9 | output write failure |

## Resume Semantics

Each completed block follows this transaction order:

1. the block `.npy` payload is fsynced and atomically replaced;
2. `snapshot.json` is fsynced and atomically replaced with the complete ledger.

The snapshot contains the completed block count, manifest SHA-256, all
alignment offsets, all analytic pan parameters, source hashes, block hashes,
block size, and total sample count. A stale block after a forced kill is ignored.

On restart the current manifest summary and recomputed parameters are compared
with the snapshot. Any mismatch removes the old snapshot and block ledger and
starts at block zero with business code `E_RESUME_MISMATCH`. A corrupt or
unreadable snapshot uses `E_RESUME_CORRUPT`.

`SIGINT` is handled as a cooperative interrupt: the active block is completed,
the snapshot is committed, `master_report.json` is written with
`status: "interrupted"` and `E_INTERRUPTED`, and the process exits `0`. SIGTERM
or SIGKILL is not converted into a clean shutdown; restart depends only on the
last committed snapshot.

Reports contain no timestamps or absolute paths. Master and channel hashes make
the delivered bytes auditable.

## Verification

Run:

```bash
make test
```

The tests cover deterministic generation, source re-hashing, one-sample
alignment, continuous pan mapping, BS.1770/true-peak compliance, stable report
field order, invalid-input business codes, byte-identical repeated rendering,
graceful SIGINT recovery, SIGKILL recovery, and `E_RESUME_MISMATCH`.

### Recorded Acceptance Result

- `pytest -q`: `11 passed`.
- Deliverable fixture: six 48 kHz/24-bit mono sources, 336,000 samples each.
- Master: stereo 48 kHz/24-bit PCM, 336,911 samples, 2,021,510 bytes.
- Master SHA-256:
  `c10e6c5e3689f566e5822075d0e613d05dac71724ae46d5324a8df36f160e0c1`.
- Declared starts `816, 288, 624, 480, 96, 912` samples are reproduced exactly
  after alignment (onset detection was sample 1 for every synthesized source).
- Integrated loudness: `-16.000001 LUFS`; deviation `-0.000001 LU`.
- Maximum true peak: `-7.428 dBTP` against the `-1.0 dBTP` ceiling.
- Limiter triggered samples: `0` for this fixture; the look-ahead path is
  exercised by the hotter-target tests.
- A second full render in a separate work directory produced the same 2,021,510
  bytes and SHA-256.
- Ctrl+C test: active block committed, snapshot/recovery report written, exit
  code `0`; resumed output matched a fresh render byte for byte.
- SIGKILL test: no exit report; restart from the committed snapshot matched a
  fresh render byte for byte.
- Manifest-change test: old ledger discarded, `E_RESUME_MISMATCH` recorded,
  block progress restarted at zero.

Per channel: source path and SHA-256, detected onset, alignment shift, declared
and actual aligned offset, aligned duration, anchor, azimuth/elevation, pan
gains, ITD/ILD, left/right energy percentages, maximum interaural difference,
maximum ILD step, stability threshold, and ear peaks.

Per group: gain and contribution percentage. Master: file name, format, sample
count, duration, byte count, SHA-256, and ear peaks. Loudness: integrated LUFS,
target deviation, maximum true peak, and limiter trigger/pass/look-ahead data.

# Virtual Studio Acoustic Calibration Workbench

Local deterministic processing workbench for multichannel swept-sine impulse-response calibration. It covers PCM16 WAV ingestion, direct-sound alignment, segmented FFT band analysis, smoothing, linear-phase FIR compensation, error-budget propagation, multichannel consistency diagnostics, incremental recomputation, and a PySide6 desktop UI.

## Setup

The verified environment is Python 3.11.18, NumPy 2.4.6, SciPy 1.17.1, and PySide6 6.12.0.

```bash
./scripts/setup.sh
source .venv/bin/activate
```

The setup script uses `uv` when available and otherwise requires `python3.11`. It creates `.venv`, installs the package with test dependencies, and creates `samples`, `cache`, and `reports`.

## End-to-end run

```bash
python -m studio_calib doctor
python -m studio_calib generate --out samples --seed 20261120
python -m studio_calib run \
  --wav samples/surround51.wav \
  --cache cache \
  --report reports/diagnostic.json \
  --trace reports/trace.jsonl \
  --coefficients reports/filter_coefficients.json
```

Launch the workbench:

```bash
python -m studio_calib gui --wav samples/surround51.wav --cache cache
```

On a machine without `DISPLAY` or `WAYLAND_DISPLAY`, the GUI selects Qt's offscreen platform and software raster rendering. The doctor command verifies an offscreen `QImage`/`QPainter` software render without requiring a GPU.

## Workbench behavior

- Alignment uses an adaptive per-channel threshold derived from the signal: pre-arrival MAD noise sigma, an absolute floor, and a fraction of the channel peak. Arrival is the first sample above that threshold. Reference-channel selection, gain normalization, and integer shifts are all explicit in the trace.
- Analysis uses overlapping, windowed segments of the aligned impulse response. Band aggregation is octave or one-third octave. Coherent gain, segment count, segment variance, and PCM quantization step are retained as intermediate quantities.
- Smoothing is a normalized Gaussian kernel in log2 frequency. Each affected band records its contributing bands, weights, and smoothing-coupling bound.
- FIR design uses a zero-phase magnitude grid followed by a centered symmetric truncation. Odd tap counts guarantee exact linear phase and a reported `(taps - 1) / 2` group delay.
- The error ledger propagates alignment, quantization, segmentation, smoothing-coupling, and FIR-truncation terms. Each band publishes both dB terms and power fractions, so the total can be recomputed independently.
- Consistency checks report relative gain mismatch, phase mismatch/inversion, and adjacent-band gain jumps. Low-frequency jumps name the dominant modeled source among alignment offset, smoothing coupling, and filter truncation and include the source values.
- Jobs run in a background thread. A second submission returns `E_BUSY`. Cancellation or failure leaves the last committed result untouched. A successful job publishes a complete new version and a structured old/new diff.

## Incremental cache

Every node has a SHA-256 key derived only from stage identity, upstream content hashes, and relevant configuration. Cache entries contain the output payload and its content hash. Nodes cover:

```text
load
  -> align:<channel>
  -> bands:<channel>
  -> smooth:<channel>:<band>
  -> correction:<channel>:<band>
  -> filter_segment:<channel>:<segment>
  -> filter:<channel>
  -> error:<channel>:<band>
  -> consistency
```

Unchanged nodes are loaded from `cache/`; trace entries mark `cache_hit` and carry the reused content hash. Changing only smoothing or target settings leaves alignment and FFT-band nodes untouched. Target changes reuse smoothing, while changed correction bands invalidate their corresponding filter segments and assembled FIR.

## Determinism

The sample generator uses `PCG64(seed)` and writes deterministic little-endian PCM16 WAV files. Reports and JSONL traces contain no timestamps or file-size substitutes. With the same seed, input bytes, configuration, and starting cache state, filter coefficients and traces are byte-identical. Use `--no-cache` for a cache-independent canonical trace.

`./scripts/verify.sh` regenerates samples, runs the tests, executes diagnostics, and compares two clean traces and coefficient files with `cmp`.

## Error codes

- `E_BUSY`: another processing job is already running.
- `E_CANCELLED`: worker observed a cancellation request.
- `E_FILTER_INVALID`: invalid tap count, target curve, boost/cut limit, or non-finite desired response. `parameter` identifies the field.
- `E_ERROR_BUDGET`: a band's total propagated upper bound exceeds the configured threshold. This is a band status, not a process failure.
- `E_ALIGNMENT`, `E_CONFIG_INVALID`, `E_WAV_INVALID`, `E_INTERNAL`: input, configuration, WAV, and defensive worker errors.

## Verification

The final local validation is recorded in `reports/validation_summary.json`. Key results:

- All six detected arrivals match the synthetic ground truth exactly: `FL=233`, `FR=241`, `FC=229`, `LFE=247`, `SL=237`, `SR=243`.
- An unchanged repeat reuses all `722/722` cache nodes, reports no affected nodes, and preserves the publication hash.
- A target-curve change reuses all alignment, FFT-band, and smoothing nodes and recomputes correction/filter downstream nodes.
- A smoothing change reuses all alignment and FFT-band nodes.
- Two clean no-cache runs produce byte-identical JSONL traces and filter-coefficient files.
- The full test suite passes with 22 tests.
- The default sample contains identified SR low-frequency gain jumps attributed to `filter_truncation`, with competing alignment and smoothing terms retained in evidence.

See `docs/ARCHITECTURE.md` for formulas, propagation definitions, and design constraints.


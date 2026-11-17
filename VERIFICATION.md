# Verification Record

Date: 2026-11-18

Environment used for the recorded run:

- WSL2 Linux x86_64, Python 3.14.4
- numpy 2.5.5, scipy 1.18.1, soundfile 0.14.0
- The implementation uses only portable Python, NumPy/SciPy, and libsndfile
  APIs; no Linux-only behavior is required by the renderer.

## Automated Suite

Command:

```bash
PYTHONPATH=src .venv/bin/python -m unittest discover -s tests -v
```

Result: **16 tests passed in 9.813 s**.

Covered:

- deterministic biome-parallel output;
- Ctrl+C checkpoint, exit code 0, and byte-identical resume;
- SIGKILL recovery and byte-identical resume;
- `E_CHECKPOINT_CORRUPT` for an altered loudness intermediate;
- restart after a syntactically broken checkpoint;
- fixed-six-decimal QC and report/WAV verification;
- `E_PROJECT_INVALID` and exit code 2 for invalid JSON, gain range, curve order,
  and sample-rate mismatch;
- `E_MEMORY_LIMIT`;
- BS.1770 relative level, loop OLA length, and both limiters.

## Demonstration Master

Commands:

```bash
python tools/generate_sample.py --output artifacts/demo --duration 6
PYTHONPATH=src python -m biomemaster render \
  artifacts/demo/project.json \
  --output artifacts/demo/master.wav \
  --report artifacts/demo/qc.json \
  --workers 4
PYTHONPATH=src python -m biomemaster verify \
  --report artifacts/demo/qc.json \
  --wav artifacts/demo/master.wav
```

Observed result:

- Duration: 6.000000 s, 288,000 samples, 48 kHz stereo.
- Project SHA-256:
  `8177cf391a6b0ff4ceccf623251ca1ce9c978e1ffc2c92b4f6c4cbab30ea436d`.
- WAV SHA-256:
  `d5938aeffaa44a335e36788e33e71e479ea057b3d071045e73b977246168725c`.
- Integrated loudness: -23.000000 LUFS.
- True peak: -13.677566 dBTP (below -1.0 dBTP).
- Soft-limiter trigger samples: 1,534.
- Crossfades: 21.
- Shards: 36.
- WAV format: 24-bit PCM, 1,728,044 bytes.
- `biomemaster verify`: all report/WAV checks true.

An additional stress render with target -3 LUFS completed in 3 finalization
iterations at -3.174030 LUFS and -1.056574 dBTP, demonstrating convergence when
the true-peak limiter is active.

## Interruption Comparison

- Ctrl+C run: stopped after a persisted checkpoint at 3 completed chunks,
  returned code 0, then resumed with a different worker count.
- SIGKILL run: stopped after a persisted checkpoint at 4 completed chunks,
  returned code -9, then resumed after checkpoint verification.
- Both resumed WAVs had the same SHA-256 as the uninterrupted reference:
  `d5938aeffaa44a335e36788e33e71e479ea057b3d071045e73b977246168725c`.
- Workers 1 and 4 also produced the same WAV SHA-256.

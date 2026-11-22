# Verification Record

Date: 2026-11-23  
Execution environment: WSL2 Linux, Python 3.11.18, NumPy 1.26.4, Tk 9.0. The implementation is cross-platform and Windows commands are documented in `README.md`; this run was performed in the available WSL2 desktop environment.

## 1. Deterministic samples and truth

Command:

```bash
.venv/bin/python -m npr_workbench.cli generate-samples --out data
.venv/bin/python -m npr_workbench.cli verify-truth --data data --out artifacts/truth_verification.json
```

Result:

```json
{
  "passed": true,
  "scenes": [
    {"scene": "orb_and_crate", "checked_samples": 110, "passed": true},
    {"scene": "twin_signal", "checked_samples": 110, "passed": true},
    {"scene": "signal_garden", "checked_samples": 110, "passed": true}
  ]
}
```

All 330 sampled pixels matched expected level, outline flag, 7×7 brush alpha coverage and reference style SHA-256.

## 2. Preview latency and exact render

Command:

```bash
.venv/bin/python -m npr_workbench.cli benchmark \
  --data data --scene orb_and_crate --repeat 60 --warmup 6 \
  --out artifacts/benchmark
```

Measured:

| Metric | Result |
| --- | ---: |
| Preview resolution | 120 × 90 |
| Exact resolution | 360 × 270 |
| Preview median | 12.07 ms |
| Preview p95 | 13.60 ms |
| Preview max | 15.74 ms |
| Budget | 16.00 ms |
| Exact render | 932.08 ms |
| Exact tiles committed | 0..62 in order |

Cold plan construction was approximately 110–140ms. The desktop UI performs the warmup before interaction is enabled, so subsequent drag/slider previews use the measured warm path.

## 3. Three-path consistency

Command:

```bash
.venv/bin/python -m npr_workbench.cli verify \
  --data data --scene orb_and_crate --pause-after 4 \
  --out artifacts/verification
```

Result:

- `passed = true`
- interactive exact commit sequence: `0..62`
- initial paused run: committed `0..3`
- resumed final commit sequence: `0..62`
- one-shot commit sequence: `0..62`
- all three styled image SHA-256 values:

```text
905de790f9e909b54cb24bb37f500d544df77ae5d6d5e5b345d2d1f0917d7dd1
```

Path report:

```json
{
  "passed": true,
  "pixel_exact": true,
  "mismatches": []
}
```

## 4. Preview/exact automatic gate

Final default-scene metrics were below all declared tolerances:

| Metric | Observed | Tolerance |
| --- | ---: | ---: |
| Level histogram TV | 0.0050 | 0.035 |
| Outline coverage delta | 0.0248 | 0.040 |
| Stroke alpha coverage delta | 0.0531 | 0.120 |
| Outline IoU after dilation | 0.8485 | ≥ 0.42 |

The first differing comparison pixel is still recorded (for example `(7, 1)`), but the gate is based on the four declared image-level metrics. Any over-tolerance case raises `E_DIVERGE` and writes preview/exact NPZ tensors, PPM images, heatmap and JSON report.

## 5. Automated tests

Command:

```bash
.venv/bin/python -m unittest discover -s tests -t . -v
```

Result: **20 tests passed** in approximately 11.6 seconds.

Coverage includes:

- parameter bounds and field-specific `E_PARA`
- stable scene hash across save/load
- `E_DECODE` for malformed JSON and unsupported geometry
- thread/RNG-independent stroke seeds
- `E_SEED` with offending stroke index
- preview/exact tolerance gate
- `E_VER` with conflicting versions
- `E_DIVERGE` diagnostic artifact retention
- parallel vs serial exact pixel and commit-order identity
- pause/resume vs one-shot pixel identity
- new engine instance restart from a partial committed window
- checkpoint identity mismatch → whole-session invalidation
- single failed tile retry without modifying other block payloads
- unexpected per-tile runtime exception isolation and retry
- `E_PATH` first pixel and tile location
- generated truth reconstruction
- warm preview p95 ≤ 16ms
- Tk desktop startup, image display and EXACT/preview frame arrival

## 6. UI screenshot

`docs/workbench.png` is a capture of the actual Tk window after an exact render. It shows the six visualization tabs, camera/pose selectors, level/density/length controls, render-control buttons, local fault injection, telemetry, exact-status badge and diagnostic heatmap tab.

## 7. Failure semantics checked

- A failed tile leaves other committed blocks intact and does not alter their SHA-256 payloads.
- Retry renders only missing/failed blocks and reaches the same pixels as one-shot rendering.
- Parameter-version change after a paused render invalidates the whole checkpoint; the manifest is recreated at the new version and no old block is mixed.
- Cancel discards uncommitted in-memory results; resume can only assemble from the persisted manifest.

The repository keeps the JSON verification summaries and the UI screenshot. Large PPM/NPZ render dumps and checkpoint tile payloads are intentionally omitted from the committed artifact set; the exact commands above regenerate them under `artifacts/verification/`.

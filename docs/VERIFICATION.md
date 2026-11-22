# Verification Record

## Environment actually used

| Item | Value |
|---|---|
| Date | 2026-11-23, Asia/Shanghai |
| Host | WSL2, Ubuntu 26.04, x86_64 |
| Python | 3.14.4 |
| PySide6 | 6.12.0 |
| NumPy | 2.3.5 |
| Display for smoke capture | `QT_QPA_PLATFORM=offscreen` |
| Transport | UDP `127.0.0.1` ephemeral port only |

Windows was not available inside this execution environment. Windows launch/setup scripts (`setup_windows.ps1`, `run_workbench.bat`) are included but were not executed here. The application uses cross-platform Python, PySide6, NumPy, socketserver, and filesystem APIs.

## 1. Environment and sample generation

Commands:

```bash
uv venv --python 3.14 .venv
uv pip install --python .venv/bin/python PySide6==6.12.0 numpy==2.3.5
.venv/bin/python -m scworkbench generate --out samples
.venv/bin/python -m scworkbench validate --sample samples/sample_session.json
```

Observed:

- sample frames: `40`
- input events: `28`
- phase actions: `5`
- content hash: `02753c13e1451963fc9f170c63486bb2fccbeb512e1c3813c6447a45052ecebe`
- negotiated codec: `hevc-main`
- negotiated resolution / FPS: `640×360 @ 12`
- capabilities hash: `977466547a99b46cd6ac36ee4dd99509d86995d37e0f2a3abd43699ad0e579cd`
- input backchannel: supported

## 2. Automated tests

Command:

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -s tests -v
```

Result: **48 tests, 0 failures, 0 errors**, elapsed about 26 seconds.

Coverage includes:

- valid sample, stable content hash, file load
- `E_SCHEMA`, `E_DECODE`, `E_NEGO`
- paused/mirroring/illegal transitions and preserved counters
- media packet roundtrip, out-of-order reassembly, `E_LOSS`, `E_SESSION`, `E_CORRUPT`
- coordinate scaling, clipping traces, drag mutation, `E_DUP` idempotence
- checkpoint atomicity under 12 concurrent writers
- whole-snapshot invalidation for content/capability/protocol mismatch and tampering
- paused-checkpoint resume and three-path equality
- intentional frame/input `E_DIFF` localization
- `E_LOSS / E_ORDER / E_CAP / E_CORRUPT / E_SESSION / E_DUP / E_STATE` chains
- loopback media/input echo
- runtime tier/retry changes only affecting future frames
- isolated one-shot retry for a single-frame renderer exception
- bounded worker buffer
- complete vs partial replay logs
- offscreen GUI render, completion, and export

Full log: `artifacts/generated/test-results.txt`.
Machine-readable summary: `artifacts/generated/verification_summary.json`.

## 3. Three-path acceptance demo

Command:

```bash
.venv/bin/python -m scworkbench demo \
  --sample samples/sample_session.json \
  --data-dir artifacts \
  --anomaly clean \
  --recovery-after 18
```

Result: **PASS**.

| Path | Frames | Committed | Inputs | Replayed | Missing | Final phase | Desktop revision |
|---|---:|---:|---:|---:|---:|---|---:|
| Realtime over UDP loopback | 40 | 40 | 28 results | 27 | 0 | disconnected | 27 |
| Checkpoint recovery after frame 18 | 40 | 40 | 28 results | 27 | 0 | disconnected | 27 |
| Full replay | 40 | 40 | 28 results | 27 | 0 | disconnected | 27 |

The one non-replayed input is the deliberate probe at logical `1035ms` while paused. It correctly returns `E_STATE`; resume then continues without resetting frame sequence or input progress.

Comparison:

- frame `frame_sha256` sequence: identical
- per-input `desktop_digest` sequence: identical
- phase event sequence: identical
- final desktop state: identical
- comparison result: `ok=true`, no `E_DIFF`

Artifacts:

- `artifacts/generated/demo_run.json`
- `artifacts/generated/anomaly_matrix.json`
- `artifacts/generated/realtime_replay.ndjson`
- `artifacts/generated/recovery_replay.ndjson`
- `artifacts/checkpoints/three_path_demo/checkpoint.json`

## 4. Loopback relay

Command:

```bash
.venv/bin/python -m scworkbench relay-test --sample samples/sample_session.json
```

Observed:

- bind address: `127.0.0.1:<ephemeral>`
- media packets received: non-zero (48 in the recorded run)
- input envelopes received: non-zero
- invalid requests: `0`
- session set contains `cast-demo-uid-001`
- ping over UDP: `true`

No external network target is contacted.

## 5. Anomaly code matrix

| Preset | Observed codes | Important result |
|---|---|---|
| clean | `E_STATE` | paused-stage probe rejected; 40 frames committed |
| loss | `E_LOSS`, `E_ORDER`, `E_STATE` | frame 7 recovered by retransmission; frame 18 persists as missing marker |
| reorder | `E_ORDER`, `E_STATE` | reversed fragments still reassemble byte-exact |
| capability | `E_CAP`, `E_STATE` | runtime downgrade rejected; negotiated baseline retained |
| corrupt | `E_CORRUPT`, `E_LOSS`, `E_ORDER`, `E_STATE` | corrupted fragment detected by CRC, retransmitted, recovered |
| mixed | all seven codes including `E_SESSION`, `E_DUP` | foreign-session packets discarded; duplicate input leaves desktop unchanged |

`E_SCHEMA`, `E_DECODE`, `E_NEGO`, and intentional `E_DIFF` are covered by tests because they are loader/negotiation/comparator paths rather than network injections.

## 6. GUI smoke and screenshots

Command:

```bash
.venv/bin/python scripts/capture_workbench.py
```

Outputs:

- `artifacts/generated/workbench_clean.png`
- `artifacts/generated/workbench_faults.png`

The clean capture shows mid-session reconnect state with frame 25. The mixed-fault capture shows missing frames, retransmission requests, order events, corruption/session counters, clipping trace, desktop mutation, and continuation after reconnect.

`tests/test_gui_smoke.py` additionally drives the offscreen workbench to normal disconnect, verifies 40 frame rows, creates a PNG larger than 50 KB, exports the log, and verifies the complete footer.

## 7. Reproduce from scratch

```bash
rm -rf .venv artifacts/checkpoints artifacts/generated
uv venv --python 3.14 .venv
uv pip install --python .venv/bin/python -r requirements.txt
.venv/bin/python -m scworkbench generate --out samples
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -s tests -v
.venv/bin/python -m scworkbench demo --sample samples/sample_session.json --data-dir artifacts
.venv/bin/python scripts/capture_workbench.py
```

Expected: all tests pass, demo prints `"result": "PASS"`, and `failed_presets` is empty.

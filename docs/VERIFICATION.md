# Verification Record

Recorded on 2026-11-22 in the task workspace. No container runtime, network
service, or remote target was used.

## Environment

| Component | Value |
|---|---|
| Python | 3.14.4 |
| NumPy | 2.5.4 |
| PyOpenGL | 3.1.10 |
| GLFW | 2.11.0 |
| OpenGL context | 4.5 core, Mesa 26.0.8 |
| Renderer | llvmpipe, LLVM 21.1.8 |

## Automated Tests

Command:

```bash
PYTHONPATH=src python -m unittest discover -s tests -t . -v
```

Result: `Ran 36 tests ... OK` (2.400 s).

Coverage includes:

- binary round trip and exact file-size validation;
- first out-of-bounds/NaN point index;
- rectangle/sphere selection and stale-version rejection;
- purity and invalid-parameter/empty-selection behavior for all four operators;
- exact added/removed/color-change diffs;
- monotonic commits and manifest hashes;
- injected manifest failure with previous-version preservation and temp cleanup;
- forced process termination before manifest commit (orphan discarded);
- forced process termination after manifest commit (commit recovered);
- render-side old snapshot visibility during a background edit;
- cooperative cancellation with no version creation;
- commit failure leaving the published version unchanged;
- cache payload corruption and later-version cache rejection;
- headless OpenGL/software snapshot rendering;
- byte-identical samples, committed objects, manifests, traces, and summaries
  across two independent demo runs.

## Reproducibility

Command:

```bash
PYTHONPATH=src python scripts/verify_demo.py --seed 20261122
```

Result:

```json
{
  "byte_identical": true,
  "deterministic_files_compared": 13,
  "excluded_intentionally": ["runtime_metrics.jsonl"],
  "mismatches": [],
  "seed": 20261122
}
```

The generated baseline manifest is
`artifacts/samples/baselines.json` with SHA-256
`189a37650fbc6017c4688241f2fb1706bfc6716be3c0637c17df9ead55acf3be`.

Sample hashes:

| Sample | Points | SHA-256 |
|---|---:|---|
| `small-noisy.pcwb` | 2,400 | `6385531eda21a96803e62cdf68f219c36e797f33d0fbce3b7938f1b9d9605dd4` |
| `medium-hole.pcwb` | 12,000 | `4e2f5607dadc3e6a9706271f387a0d92fb7bf52741f21b4c07ab5b43acde3693` |
| `large-sparse.pcwb` | 40,000 | `457c173f97eb902d40eaa5fb0a6fccee382548a6dbc759cd43ba1b9232726c4e` |

## Scripted Edit/Versioning Demo

Artifact root: `artifacts/demo`.

```text
v1 import            parent=0  points=12000
v2 smooth            parent=1  points=12000
v3 downsample        parent=2  points= 9388
v4 color_overwrite   parent=3  points= 9388
v5 crop              parent=4  points= 4762
v6 color_overwrite   parent=2  points=12000
```

The demo rolled back from v5 to v2, then committed v6 with v2 as its parent.
This confirms that later history was retained while the active edit dependency
was recalculated from the historical version.

Final current version is 6 with hash
`1f6a3c865353ee8ad527bcb88f89fe43a58aa35c704839ce5c6163b0fb8d017a` and
12,000 points. The deterministic trace SHA-256 is
`ce18b71e59725960d163f422a4b5f0e91a3f6ebe1372a9a2f26bdd52ae993263`.

## Background/Render Isolation

`artifacts/demo/runtime_metrics.jsonl` records the first background smooth job
as:

- 356 display-probe frames while the edit was active;
- `old_snapshot_isolated: true`;
- the displayed hash stayed at the v1 base until publication;
- progress phases observed at approximately 61 ms, 188 ms, and 333 ms;
- zero recorded display frames exceeded the 16.667 ms budget;
- 0.447 s worker duration.

Every edit metric has `old_snapshot_isolated: true`. The display reader acquired
one published snapshot reference per frame and never observed a partial array.

## Offscreen OpenGL and Interaction Metrics

Command:

```bash
PYTHONPATH=src python -m pcwb gui \
  --repo artifacts/workbench-final-20261122c/repo \
  --sample artifacts/samples/medium-hole.pcwb \
  --headless --frames 60 --auto-smooth --scripted-input \
  --threshold-ms 50 \
  --metrics artifacts/workbench-final-20261122c/metrics.jsonl
```

Result:

- renderer path: `opengl` (hidden GLFW window, not software fallback);
- final published version: 2;
- final hash: `9daba3f43d37118a7e38d773aa05f52a333284d105d51d6e83a8c235b27958b6`;
- background smooth duration: 0.817 s;
- 143 measured frames during/after the edit;
- frame average: 5.708 ms;
- frame p95: 6.907 ms, p99: 13.444 ms;
- frame max: 25.376 ms (one publication/upload frame);
- 1 of 143 frames above the 16.667 ms budget;
- 143 scripted orbit input events;
- input latency average: 0.0239 ms, p95: 0.0319 ms, max: 0.0809 ms;
- zero input events above budget;
- progress became visible after the 50 ms threshold and the edit loop continued
  rendering until commit.
- 12,815 of 16,384 sampled center pixels were non-background, confirming the
  point cloud was actually rasterized rather than merely clearing the buffer.

The single over-budget frame is bounded and corresponds to publishing/uploading
the complete new 12,000-point snapshot; the p95 and p99 interaction budgets
remain below 16.667 ms.

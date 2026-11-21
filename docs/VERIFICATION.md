# Verification

Environment used for the recorded run:

- Linux/WSL execution host, Python 3.14.4.
- Target runtime remains Windows 10/11, Python 3.11, per `requirements.txt`.
- Geometry, CLI, tests, rasterization, and project recovery are pure Python.
- PySide6 is required only for the desktop window.

## Commands

```bash
python scripts/generate_samples.py --root samples
python -m unittest discover -v
python -m vpipe acceptance --samples samples --artifacts artifacts
python -m vpipe run \
  --a samples/v1/base_a.json \
  --b samples/v1/base_b.json \
  --operation union \
  --tolerance 0.01 \
  --out-result artifacts/run_union.v2.json \
  --project artifacts/run_union.vproj \
  --preview artifacts/run_union.ppm \
  --log artifacts/run_trace.jsonl
```

## Recorded results

`python -m unittest discover -v`:

```
Ran 24 tests in 9.342s
OK
```

`python -m vpipe acceptance --samples samples --artifacts artifacts`:

- v1 and v2 parse into geometrically consistent normalized contours for
  `base_a`, `base_b`, `nested`, `near_a`, `near_b`, `degenerate`, `shared`.
- v1 hole direction is inferred/corrected from nesting; v2 downgrade reports
  `E_VERSION_LOSS` with all affected fields and retained tolerance.
- Near-parallel test changes `disjoint -> coincident` when tolerance changes
  `0.001 -> 0.01`; difference summary reports one added hit.
- Boolean tolerance test changes the near-gap union from two loops at tolerance
  `0.001` to one merged loop at tolerance `0.01`.
- All four boolean operations on v1 and v2 samples close, have no output
  self-intersections, are not degraded at threshold `1.0`, and have zero
  raster-cell area symmetric difference.
- Preview raster is byte-identical across repeated runs.
- Project atomically saves and restores the last committed result hash.
- A modified project payload returns `E_PROJECT_MISMATCH`.
- An unknown preview binding returns `E_PREVIEW_STALE`.

Actual operation areas (v1 and v2 identical):

| operation | loops | signed area | area rel. error | boundary upper | degraded |
|---|---:|---:|---:|---:|---|
| difference | 2 | 4561.9659332599 | 0 | 0.5067 | false |
| intersection | 1 | 2157.9120011960 | 0 | 0.4947 | false |
| union | 3 | 10807.6751617755 | 0 | 0.4308 | false |
| xor | 4 | 8649.7631605796 | 0 | 0.3781 | false |

Artifacts:

- `artifacts/trace.jsonl`: deterministic 23-event acceptance trace.
- `artifacts/acceptance_report.json`: full machine-readable report.
- `artifacts/acceptance.vproj`: atomically written committed project.
- `artifacts/tampered.vproj`: rejected hash-mismatch fixture.
- `artifacts/preview.ppm`: committed preview persisted without alpha.
- `artifacts/run_union.v2.json`, `artifacts/run_union.vproj`,
  `artifacts/run_union.ppm`, `artifacts/run_trace.jsonl`: scripted `run` output.
- `artifacts/intersection.v1.json`, `artifacts/export_trace.jsonl`: v1 downgrade
  export containing `E_VERSION_LOSS` with six affected fields.

Reproducibility check: the acceptance command was run twice and the following
artifact SHA-256 values were identical before and after:

```
e0356d0fc0d012ea6060aa07ba36a70296b4b7c916ca8e327aa71a11e3f748a2  acceptance_report.json
14101e221c1aadb91e2ca95cbf50dc3f42b73341ea600e975ef23470072fc188  trace.jsonl
801b7c047ae4511368c44d6ac89912b2b94c75c7dd1335bc9cb4aa59ca91b00c  preview.ppm
c42ae04597e347735115e22751fde5932af70a0ab2c5b6a028682119ed023134  acceptance.vproj
e40461e368fa6a9b74ef339748d804af8d4fff99bd6bdce6ec0fb17bd578b3b3  tampered.vproj
```


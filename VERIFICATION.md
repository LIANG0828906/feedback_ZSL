# Verification Record

Date: 2026-11-22

## Environment

- OS: Ubuntu 26.04.1 LTS (Linux)
- Python: 3.14.4
- NumPy: 2.5.5
- PyQt5: importable, offscreen platform plugin verified
- Working directory: `/home/liang/feedback/uid190c93df-default-70`
- No Docker, external datasets or network inputs were used.

Windows was not available in this execution environment. The implementation uses
cross-platform NumPy and Qt APIs; a Windows/OpenGL GUI smoke run remains an
environment-specific acceptance step.

## Automated Tests

Command:

```bash
python3 -m unittest discover -v -s tests -t .
```

Result:

```text
Ran 32 tests in 7.606s
OK
```

Full verbose output: `artifacts/verification/unit_tests.log`.

Coverage includes:

- binary roundtrip, matrix/geometry/nesting/density validation;
- five injection classes with distinct codes and non-negative offsets;
- stable row-major partition, budget degradation, waiting queue and configured
  parallelism;
- volume output, fixed/float error bound, seam truth and seam error location;
- byte-identical repeated image/summary/trace;
- incremental material and chunk invalidation, conservative invalidation,
  reused hashes, published-snapshot immutability under in-place scene mutation
  and atomic rollback after budget rejection;
- deterministic sample generation and offscreen PyQt construction.

## Generated Samples And Baselines

Command:

```bash
python3 -m volrender samples --outdir samples
```

Generated scenes:

| Sample | Resolution | Media | Density fields |
| --- | ---: | ---: | --- |
| `gradient_smoke_48x32` | 48x32 | 1 box | gaussian gradient |
| `smoke_in_glass_64x44` | 64x44 | glass sphere + nested smoke box | dual blob + uniform glass |
| `dual_plume_glass_72x48` | 72x48 | glass sphere + two nested boxes | layered + dual blob + uniform |

All baselines used a whole-screen single-block seam truth with tolerance `0.0`.

| Sample | Pixel/scatter hash | Max scatter seam delta | Max transmittance seam delta |
| --- | --- | ---: | ---: |
| `gradient_smoke_48x32` | `f00638978a9adbf5eb743df7f443f764868338f35d76dab548b780fd3264ac6d` | 0.0 | 0.0 |
| `smoke_in_glass_64x44` | `075704b663a5f540fadf9b673303a06ca86685c8be74b740188649f75136d544` | 0.0 | 0.0 |
| `dual_plume_glass_72x48` | `8ae599a13f269fe8241c60d7f5f9de040ff25855531e367e57c7e98d13b16fef` | 0.0 | 0.0 |

Reproduction command:

```bash
python3 -m volrender samples --outdir samples --verify
```

Result:

```json
{
  "checked": 3,
  "failures": [],
  "ok": true
}
```

Saved as `artifacts/verification/samples_verify.json`.

This rerender confirms that scene bytes, pixel buffers, image outputs and
deterministic summary hashes reproduce under the same seed and operation
sequence.

## Budget Degradation And Rejection

`artifacts/verification/budget_config.json` lowers the per-tile budget from
`2,000,000` to `8,000` samples for the 48x32 sample.

Render result:

```text
tiles: 6
stage S0_FULL: 5
stage S1_STEP_2X: 1
total consumed: 80,253 samples
pixel hash: 33eec62ec9403e3008fe73e647c8b6195ae546d1f21b420c84c8dfaa96829499
```

The degraded tile is `t00001-16_0_16_16`, recorded with
`sample_count=4979`, `effective_base_step=0.2` and its own content hash in
`artifacts/verification/budget_degrade/trace.jsonl`.

With `sample_budget=1`, all six tiles passed through every declared stage into
the waiting queue and the CLI returned `E_BUDGET_EXCEEDED`. The target output
directory was not created (`reject_output_exists=false`); the result is saved in
`artifacts/verification/budget_reject.json`.

## Precision Comparison

The 48x32 sample rendered in fixed mode with float comparison:

```text
max absolute deviation: 7.301962656991803e-06
mean absolute deviation: 8.445799114547125e-07
RMSE: 1.2610946899580286e-06
P95: 2.7317457208790553e-06
reported upper bound: 4.3487548828125e-04
fixed quantum: 9.5367431640625e-07
```

The per-pixel difference buffer is
`artifacts/verification/precision_compare/precision_delta.npy`; aggregate
statistics and the bound are in `summary.json`.

## Determinism

Two independent CLI renders of the same scene and configuration produced
identical files:

```text
image.ppm   1e69634ac04995ce88afdee03e25ad7eb43ff70a3a96a27d2ecaeb0ce243edff
image.png   4d0311998d9f8fb688c635c8d1cbb0afef3389cab4336259a88d529d7e391b4b
summary.json 193ba0a8eeb2fbb419f2a1faebd45ca3289dbe04dc0530d8f20b57264b1f74f5
trace.jsonl  6e91800c885799e9e630e62496691381a09676e60189fca71ea5e54ef49aa50d
```

Both runs reported pixel hash
`f00638978a9adbf5eb743df7f443f764868338f35d76dab548b780fd3264ac6d`.

## Incremental Update And Atomic Rollback

`scripts/incremental_demo.py` uses a 40x30 two-medium scene split into 12 tiles.
Changing only medium `10` material parameters produced:

```text
rendered tiles: 6
reused tiles:   6
reason: medium_material_changed
reused content hashes preserved: true
```

The demo then attempts an update with `sample_budget=1`. It returns
`E_BUDGET_EXCEEDED`, the snapshot version stays at `2`, and
`snapshot_unchanged=true`. The complete report is
`artifacts/verification/incremental_demo.json`.

## Injection Channel Results

CLI tests ran each injection against a valid scene/configuration and asserted
that no `image.ppm` or `summary.json` existed afterward:

| Injection | Business code | Location |
| --- | --- | --- |
| `chunk_index` | `E_CHUNK_INDEX` | corrupted chunk index record offset `440` |
| `missing_media` | `E_MEDIA_MISSING` | `media[0]`, byte offset `276` |
| `camera_nonfinite` | `E_CAMERA_NONFINITE` | camera matrix, offset `76` |
| `density_truncated` | `E_DENSITY_TRUNCATED` | density payload offset `9132` |
| `config_budget` | `E_CONFIG_BUDGET` | JSON `sample_budget` offset `603` |

Each CLI result in `artifacts/verification/inject_*.json` includes the code,
field and offset. None of the five runs created an output directory.

## GUI Verification

An offscreen PyQt5 smoke test instantiated the main window and both custom
widgets successfully. The preview provides:

- rendered image;
- tile grid;
- orange degradation-stage labels for non-default tiles;
- per-tile budget occupancy line/bar curve;
- tile size, parallelism, budget and precision controls;
- cooperative cancel in planning and during sample accumulation.

# PCWB Point-Cloud Workbench

A local, deterministic point-cloud editing workbench. It reads and writes a
custom binary point-cloud format, provides rectangle and sphere selection,
executes four edit operators on a background worker, and commits immutable
versions through atomic file replacement.

The render path only reads the controller's currently published
`PointCloudSnapshot`. A worker computes into new arrays and publishes a new
snapshot with one reference replacement after the version commit succeeds.
Consequently the renderer never observes a half-edited cloud.

## Quick Start

Python 3.10+ is required. For a fully offline installation, provide a directory
containing local `numpy`, `PyOpenGL`, `glfw`, and optional `pytest` wheels:

```bash
scripts/setup_local.sh /path/to/local/wheels
.venv/bin/python -m pcwb --help
```

If the environment already provides those packages, only `PYTHONPATH=src` is
needed. This workspace was validated with Python 3.14.4, NumPy 2.5.4,
PyOpenGL 3.1.10, and GLFW 2.11.0.

Generate deterministic samples and baseline hashes:

```bash
.venv/bin/python scripts/generate_samples.py --out artifacts/samples
```

Run the scripted editing/versioning/background-isolation demo:

```bash
.venv/bin/python scripts/demo.py --out artifacts/demo
```

Verify byte reproducibility of two independent runs:

```bash
.venv/bin/python scripts/verify_demo.py --seed 20261122
```

Run the OpenGL workbench:

```bash
.venv/bin/python -m pcwb gui \
  --repo artifacts/workbench/repo \
  --sample artifacts/samples/medium-hole.pcwb
```

For an offscreen/headless check:

```bash
.venv/bin/python -m pcwb gui \
  --repo artifacts/workbench/repo \
  --sample artifacts/samples/medium-hole.pcwb \
  --headless --frames 120 --auto-smooth \
  --scripted-input \
  --threshold-ms 50 --metrics artifacts/workbench/metrics.jsonl
```

The offscreen path creates a hidden GLFW OpenGL 3.3 core context and renders
through the same shaders/VBO path as the visible window. If context creation is
not possible, it falls back to a software display loop that still reads exactly
one published snapshot per frame. The report identifies which path was used.

## Binary Format

All integers and floats are little endian.

| Field | Type | Bytes |
|---|---:|---:|
| Magic/version | `8s`, value `PCWB\0\0\0\1` | 8 |
| Point count | `uint64` | 8 |
| Bounding-box minimum | `float32[3]` | 12 |
| Bounding-box maximum | `float32[3]` | 12 |
| Point record, repeated | `float32[3]` XYZ + `uint8[3]` RGB | 15 each |

Header size is 40 bytes. A valid file is exactly
`40 + 15 * point_count` bytes. Validation rejects invalid magic, length/count
mismatch, non-finite coordinates, reversed bounds, and points outside the
header bounds. `E_PC_INVALID` includes the first offending point index in
`point_index`.

The content hash is SHA-256 over the complete canonical file bytes, including
the header. Writes use a same-directory temporary file, `fsync`, and atomic
rename.

## Architecture

- `pcwb.format`: strict PCWB parse/serialize and atomic standalone writes.
- `pcwb.selection`: immutable index selections bound to a cloud content hash;
  screen rectangle, world rectangle, and world sphere selection.
- `pcwb.operators`: pure `crop`, `smooth`, `downsample`, and
  `color_overwrite` implementations plus normalized operation records.
- `pcwb.repository`: monotonic versions, immutable objects, manifest commits,
  rollback pointer, and crash recovery.
- `pcwb.cache`: content/version-addressed operator cache with payload hash and
  lineage validation.
- `pcwb.async_edit`: one-worker background editing, progress, cancellation, and
  atomic published-snapshot replacement.
- `pcwb.workbench` and `pcwb.gl_renderer`: GLFW/OpenGL interaction, highlight
  rendering, diff overlays, metrics, and offscreen/software fallback.
- `pcwb.demo`, `pcwb.samplegen`, `pcwb.trace`: deterministic reproducible
  artifacts and scripted acceptance flow.

Detailed invariants and commit ordering are in
`docs/ARCHITECTURE.md`.

## Versioning and Crash Recovery

The manifest is the source of truth. A commit performs this order:

1. Serialize and hash the complete output cloud.
2. Write an immutable object to a same-directory temporary file and `fsync`.
3. Atomically rename the staged object to its immutable version filename
   without replacing an existing target.
4. Atomically replace the manifest that lists the new version and updates
   `current_version`.

If the manifest write fails, the new object is removed and the previous version
remains current. Version objects are never overwritten. On startup, `recover()`
removes temporary files and objects not referenced by the durable manifest.
Therefore a forced process kill restores only the last committed manifest
state; uncommitted worker output is discarded.

`checkout` changes the active recovery pointer without deleting later history.
The next edit uses the historical version's hash as its input dependency and
gets a new monotonic version number. Cache keys include both version and content
hash, and entries from later versions are invalidated when rolling back.

## Interaction

Left drag orbits. Mouse wheel zooms. Selection and edit keys:

| Key | Action |
|---|---|
| `R` | Rectangle selection mode; drag with left mouse |
| `S` | Sphere selection mode; drag center-to-radius |
| `C` / `X` | Crop keep / crop remove selected points |
| `G` | Smooth selection |
| `V` | Voxel downsample selection |
| `O` | Color overwrite selection |
| `B` | Roll back to the current version's parent |
| `[` / `]` | Choose comparison version |
| `J` | Diff current version against comparison version |
| `Esc` | Cancel edit, then clear selection/mode |
| `Q` | Quit |

Selection count and bounds are printed and shown in the window title. Selected
points are yellow. Diff additions are green, color changes magenta, and removed
points are shown as red ghosts. Edit progress is shown in the title and console
once the declared threshold is exceeded.

## Deterministic Logs

Two logs are deliberate:

- `trace.jsonl` is the reproducible operation trace. It contains sample hashes,
  version numbers, operators, parameters, input/output hashes, counts,
  selections, rollback/diff events, and frame policy. It contains no wall-clock
  timestamps or measured durations.
- `runtime_metrics.jsonl` contains actual frame durations, input-response
  latency, edit durations, progress observations, and snapshot-isolation
  evidence. These values are inherently run-dependent.

This split satisfies both requirements: identical seeds and operation sequences
produce byte-identical point-cloud files and deterministic traces, while real
latency remains observable in the runtime sidecar. `scripts/verify_demo.py`
compares every sample, committed object, manifest, trace, and summary byte.

## Tests

```bash
.venv/bin/python -m unittest discover -s tests -t . -v
```

The suite covers binary validation and first-bad-index reporting, immutable
selection binding, all operator purity/validation paths, exact diffs,
manifest-failure rollback with cleanup, forced-kill orphan recovery,
post-commit forced-kill recovery, background old-snapshot visibility,
cancellation, cache corruption/lineage rejection, byte-reproducible demos, and
headless OpenGL/software snapshot rendering.

Actual validation results are recorded in `docs/VERIFICATION.md`.

## Business Codes

- `E_PC_INVALID`: malformed point-cloud file, length/count mismatch, non-finite
  or out-of-bounds coordinates.
- `E_OP_INVALID`: unknown operator, invalid parameters, or empty selection.
- `E_COMMIT_FAILED`: manifest/object commit failure or stale base version.
- `E_SELECTION_INVALID`: stale selection or invalid index set.
- `E_BUSY`: another background edit is already running.
- `E_CANCELLED`: cooperative cancellation completed.

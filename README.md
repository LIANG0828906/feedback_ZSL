# Volumetric Light Preview Renderer

Deterministic offline preview renderer for local binary scenes containing
smoke, transparent glass-like media, a directional light and chunked 3D density
fields. The renderer is implemented in Python with NumPy and an optional PyQt5
preview.

The implementation focuses on operational reproducibility:

- all inputs are validated before rendering;
- tile jobs may run in parallel, but each tile has a fixed row-major/sample-order
  traversal;
- budgets and degradation stages come from configuration and every decision is
  recorded;
- a rejected or failed update never publishes a partially composed image;
- incremental updates replace a versioned snapshot atomically;
- fixed-point and floating-point accumulation are both available with
  deterministic hashes and precision error statistics;
- fault injection lives at the loading/configuration boundary and is separate
  from renderer and scheduler code.

## Requirements

- Python 3.10 or newer
- NumPy
- PyQt5 for the graphical preview (the CLI and tests run headless)

```bash
python -m pip install -r requirements.txt
```

The current verified environment is Ubuntu 26.04 with Python 3.14.4, NumPy 2.5.5
and PyQt5. The code uses only cross-platform Python APIs. Windows verification
still needs a local run with an OpenGL-compatible Qt environment.

## Quick Start

Generate deterministic local scenes and baseline renders:

```bash
python -m volrender samples --outdir samples
python -m volrender samples --outdir samples --verify
```

Render one scene:

```bash
python -m volrender render \
  --scene samples/gradient_smoke_48x32.vscn \
  --config samples/render_config.json \
  --outdir artifacts/example_render \
  --compare-precision
```

Open the preview:

```bash
python -m volrender gui \
  --scene samples/gradient_smoke_48x32.vscn \
  --config samples/render_config.json
```

Run all automated tests:

```bash
python -m unittest discover -v -s tests -t .
```

Run the incremental update/rollback demonstration:

```bash
python scripts/incremental_demo.py --output artifacts/incremental_demo.json
```

## Outputs

A successful render writes:

- `image.ppm`: tone-mapped preview image;
- `image.png`: the same preview in PNG form;
- `scatter.npy`: floating-point scattering buffer;
- `transmittance.npy`: per-pixel transmittance;
- `precision_delta.npy`: per-pixel float/fixed difference when comparison is
  requested;
- `summary.json`: tile hierarchy, budget, degradation, step sizes, seam check,
  precision error summary and deterministic hashes;
- `trace.jsonl`: stable submission-order scheduler and tile trace;
- `manifest.json`: artifact sizes and SHA-256 hashes.

No image or summary is written when loading, validation, budget admission, seam
validation or cancellation fails.

## Business Codes

| Code | Meaning |
| --- | --- |
| `E_SCENE_INVALID` | general malformed, degenerate or non-nested scene input |
| `E_CHUNK_INDEX` | damaged/out-of-range/duplicate/missing density chunk index |
| `E_MEDIA_MISSING` | declared media entry missing |
| `E_CAMERA_NONFINITE` | non-finite camera matrix |
| `E_DENSITY_TRUNCATED` | truncated density metadata or payload |
| `E_CONFIG_INVALID` | malformed or non-monotonic configuration |
| `E_CONFIG_BUDGET` | sampling budget exceeds declared hard limits |
| `E_CONFIG_PARALLELISM` | parallelism exceeds declared hard limit |
| `E_BUDGET_EXCEEDED` | no declared degradation stage fits a tile |
| `E_SEAM_UNRESOLVED` | tiled output differs from whole-screen truth |
| `E_CANCELLED` | cooperative cancellation |

Every business error includes `field` and `offset` where an input location can
be identified. CLI failures are emitted as JSON and return a non-zero status.

## Fault Injection

Injection is available only through the CLI:

```bash
python -m volrender render ... --inject chunk_index
python -m volrender render ... --inject missing_media
python -m volrender render ... --inject camera_nonfinite
python -m volrender render ... --inject density_truncated
python -m volrender render ... --inject config_budget
```

Injection transforms decoded scene metadata or configuration before validation.
The renderer and scheduler never import `fault_injection.py`.

## Documentation

- [Binary format](docs/FORMAT.md)
- [Rendering and scheduling design](docs/ARCHITECTURE.md)
- [Actual verification record](VERIFICATION.md)

## Scope Notes

This is a deterministic preview renderer, not a production spectral path tracer.
Transparent boundaries are traversed without Fresnel reflection. The fixed-point
path uses Q20.20 integer accumulation while transcendental functions still use
NumPy/Python floating point, matching common deterministic fixed-point emulation.
The reported analytical error bound is conservative and is stored with every
fixed-point render.

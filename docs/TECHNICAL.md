# Technical Notes

## 1. Numerical Chain

For raw scalar `v`, the loader records:

```text
raw_min = min(v)
raw_max = max(v)
truth_source = file_minmax | volume_minmax | caller-supplied
```

Normalization and window mapping:

```text
n = clip((v - raw_min) / (raw_max - raw_min), 0, 1)
level_n = (level - raw_min) / (raw_max - raw_min)
width_n = width / (raw_max - raw_min)
w = clip((n - (level_n - width_n / 2)) / width_n, 0, 1)
```

`w` is the TF domain value passed to the renderer. Both `n` and `w` are kept.
Every call to `set_window()` or `set_spacing()` rebuilds `w`, increments a
mapping revision and returns `MappingChange` with:

- old/new revision,
- maximum absolute difference,
- mean absolute difference,
- changed voxel count,
- old/new SHA-256 mapping digests.

Changing spacing does not change scalar amplitudes, so the expected mapping
delta is exactly zero, but the rebuild still occurs and is observable. Spacing
changes the world extent and central-difference denominator.

## 2. Transfer Function and LUT

Control points are `(x, opacity, r, g, b)`. Validation enforces:

- at least two points,
- finite `x` in `[0, 1]`,
- strictly increasing `x`,
- finite opacity/color channels in `[0, 1]`.

Opacity and each color channel are piecewise-linear. Linear interpolation is
C0-continuous at every control point. A candidate TF and candidate LUT are
constructed before assignment. If validation fails,
`renderer.transfer` and `renderer.lut` still reference the previous version.

LUT domain size defaults to 65,536. Runtime lookup linearly interpolates
adjacent LUT entries.

Float mode keeps `float64` LUT values. Fixed mode quantizes to signed
non-negative Q12 by default:

```text
levels = 2^12 - 1
q = round(clip(value, 0, 1) * levels) / levels
opacity_quant_step = 1 / levels
color_quant_step   = 1 / levels
```

For each control interval the reported opacity error bound is:

```text
slope_error = |delta_opacity / delta_x| * (0.5 / (LUT_size - 1))
value_error = 0.5 * value_quant_step
upper_bound = min(1, slope_error + value_error)
```

Float and fixed paths share the same control points, LUT indexing and
interpolation. A mode switch never changes transfer-function semantics.

## 3. Camera and Sampling

A scripted camera is persisted as a 4×4 world-to-camera matrix using the OpenGL
convention (camera looks down `-Z`). A perspective ray grid is generated for
every output pixel, then intersected with the volume AABB.

`step` is measured in voxel units. World step length is:

```text
step_world = step * mean(spacing)
```

The first sample is half a step past AABB entry; subsequent samples are spaced
by exactly `step_world`. Trilinear sampling is done in array coordinates
`(x, y, z)`. Coordinates outside the volume are clamped to the nearest voxel
center. This keeps boundary gradients defined.

To make opacity physically consistent when the step is refined:

```text
alpha_step = 1 - (1 - opacity_lut)^step
```

Thus halving the step and compositing two samples preserves transmittance in
the limit of homogeneous sample neighborhoods.

## 4. Gradient Lighting

The gradient is a central difference in array coordinates, divided by physical
spacing:

```text
gx = (f(x+1,y,z) - f(x-1,y,z)) / (2*sx)
gy = (f(x,y+1,z) - f(x,y-1,z)) / (2*sy)
gz = (f(x,y,z+1) - f(x,y,z-1)) / (2*sz)
n  = g / ||g||
```

If `||g|| <= 1e-12`, the normal falls back to `(0, 0, 1)`. Boundary samples are
clamped, so edge voxels use a one-sided effective difference.

Shading uses a fixed world-space light direction:

```text
shade = 0.25 + 0.75 * max(dot(n, light), 0)
```

Shading and alpha accumulation are in the same `float64` ray-march loop.

### Lighting Drift Audit

`audit_lighting_drift()` renders the same pixel grid at `step` and `step / 2`.
It compares RGB channels for pixels whose opacity exceeds `0.01` in either
render. The declared default tolerance is `0.20` maximum channel difference.
If the maximum exceeds tolerance, it raises `E_LIGHT_DRIFT` with mean, p95,
maximum and tolerance in `details`.

On the 64³ sample at 96×96, step 1.0 versus 0.5 produced:

```text
max  = 0.16179806413678915
p95  = 0.023344161914528358
mean = 0.004886607894541992
```

## 5. Forward Alpha Compositing and Error Propagation

Samples are visited front-to-back. Let:

- `T_i` be transmittance before sample `i`,
- `alpha_i` be step-corrected opacity,
- `c_i` be shaded LUT color,
- `delta_alpha_i` be step-corrected opacity error,
- `delta_c` be LUT color quantization error.

The image contribution is:

```text
C = sum_i T_i * alpha_i * c_i
```

The conservative per-pixel error recursion is:

```text
E_C += (E_T * alpha_i + T_i * delta_alpha_i) * c_i
       + T_i * alpha_i * delta_c

E_T_next = E_T * (1 - alpha_i) + T_i * delta_alpha_i
T_next   = T_i * (1 - alpha_i)
```

`E_C` is accumulated as the maximum absolute channel error. The LUT opacity
error is selected from the interval upper bounds. For a step-corrected sample:

```text
delta_alpha_i = clip(delta_opacity * step * (1 - opacity)^(step - 1), 0, 1)
```

This directly includes transmittance and sample weights, so low-opacity pixels
with long rays accumulate more uncertainty.

If `error_map > threshold`:

- the pixel is marked in `untrusted`,
- final RGBA is set to zero,
- `error_summary.status` becomes `low_confidence`,
- `error_summary.converged` becomes false.

The raw diagnostic float RGB remains available, but the display pipeline uses
the masked RGBA buffer and the error heat map generated from the same
`error_map`.

## 6. Precision Comparison

`compare_precision()` and `set_precision()` render float and fixed bundles for
the exact same view, step, threshold, TF points and camera. They report:

- mean/RMS/max absolute pixel differences,
- changed-pixel fraction,
- maximum propagated error for each mode,
- untrusted pixel counts for each mode,
- `semantics_unchanged`.

`set_precision()` builds both results before committing mode/LUT state. An
invalid mode or an uninitialized renderer raises `E_MODE_INVALID`.

On the default 80×80 sample comparison:

| Metric | Float | Fixed Q12 |
| --- | ---: | ---: |
| max propagated error | `5.234059092353972e-05` | `0.003092477472439918` |
| untrusted pixels | `0` | `740 / 6400` |
| trusted fraction | `1.0` | `0.884375` |

`semantics_unchanged=true`; changed display pixels are largely those masked as
untrusted by the fixed-path error threshold.

## 7. Determinism and Hashing

Fixed view, step, threshold, TF, mapping and precision mode produce the same
RGBA buffer on every run. The result content hash is:

```text
SHA256(
  RGBA8 bytes
  || error_map as little-endian float32
  || packbits(untrusted)
)
```

Float operations use explicit NumPy elementwise operations without random
sampling or thread-dependent reductions. Cross-platform Linux/Windows tests
produced identical RGBA and state hashes for the same fixtures.

## 8. Snapshots

Snapshot JSON schema `volrender.snapshot` version 1 stores:

- full renderer state: view matrix, step, FOV, image size, TF points, precision
  mode, window, spacing, threshold, LUT settings and mapping fingerprint;
- error summary and mode;
- RGBA8, float32 error map and untrusted bit mask as base64;
- result content hash;
- state hash and whole-document integrity hash.

Loading verifies all hashes and required fields before returning a `Snapshot`.
Restore builds candidate TF, LUT, view and numeric settings before mutating the
renderer. Mapping fingerprint mismatch rolls back volume window/spacing.
Missing/corrupt snapshots raise `E_SNAPSHOT_INVALID`; inconsistent
view/step/LUT/mode/state versions raise `E_SNAPSHOT_STALE`.

After a successful restore, `resume_render()` reuses the already-compiled LUT
and mapping and produces the same content hash as the original render.

## 9. Concurrency and Recovery

`RenderSession` uses a non-blocking lock:

- if a render is active, another submission immediately raises `E_BUSY`;
- only one worker may mutate renderer state;
- any exception drops `_offscreen_result`;
- `last_good_result` and the UI’s last displayed frame remain recoverable;
- the session can be reused after an error.

The PyQt5 application performs rendering in `QThread` and only creates
`QImage`/pixmaps on the UI side. Error heat-map colors are a display-only
mapping of the core error array.

## 10. Precision Trade-off

The implementation uses `float64` for the chain to avoid cumulative rounding
noise. Display quantization to 8-bit is isolated at the end. Fixed Q12 LUTs
reduce storage bandwidth and expose measurable accumulated uncertainty, but
the UI can mark low-opacity regions untrusted instead of silently presenting
them as reliable color.

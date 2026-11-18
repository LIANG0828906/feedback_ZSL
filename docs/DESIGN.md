# Technical Design

## 1. Scene Generation

`elytron/scene.py` constructs a closed icosphere, subdivides it twice, scales and
deforms it with seeded trigonometric displacement, and orients every triangle
outward. The result has 162 vertices and 320 uniquely numbered faces.

The scene contains:

- Mesh vertices, triangle indices, contiguous face IDs, and per-face seeds.
- Film thickness, perturbation, gradient, refractive index, absorption,
  bounds, spectrum, and hue threshold.
- Point-light ID, position, intensity, and RGB color.
- Ordered camera samples with position, target, up vector, FOV, and resolution.
- Leaf plane, caustic sampling density, report grid, and penumbra parameters.
- Block dimensions and display exposure.

The scene is serialized with sorted JSON keys. Re-running `generate` with the
same seed produces the same bytes.

## 2. Validation

Validation is a pure function over the JSON object:

1. Check schema and seed.
2. Check vertex array shape/finiteness and exact integer triangle indices.
3. Check every index against the vertex count.
4. Check face IDs are contiguous, unique, and paired with one seed each.
5. Check triangle area against `degenerate_area_eps`.
6. Check film parameters against their declared bounds, including the complete
   perturbation envelope and sampled per-face thickness.
7. Check light fields and use the signed solid angle of the closed mesh to
   reject a light inside the shell.
8. Check camera basis/FOV/resolution, leaf parameters, and block settings.

All issues are accumulated. A `SceneValidationError` carries structured
warnings, and the CLI writes `validation_report.json` with `image_written:
false`. Final image writers are never called on this path.

## 3. Stateless Hash and Tables

`elytron/hashing.py` implements SplitMix64 finalization and tuple hashing:

```text
h = hash_u64(seed, face_id, face_seed, channel, sample_index...)
u = (h >> 11) / 2^53
```

This removes dependence on loop order, Python's randomized `hash()`, and
global RNG state.

### Thickness Field

For face `f`, barycentric coordinates `(b0,b1,b2)`, and surface point `p`:

```text
window = clamp(27*b0*b1*b2, 0, 1)
delta_f = perturbation * signed_hash(thickness_seed, f, seed_f)
spatial(p) = gradient * (0.55*sin(2.3*p.x+0.7) + 0.45*cos(2.0*p.z-0.4))
thickness = base + delta_f*window + spatial(p)
```

The window vanishes on triangle edges, so adjacent faces share the smooth
spatial field without thickness seams.

### Micro-Normal Table

Each face has a 9x9 table. A local tangent frame is built from the canonicalized
triangle geometry. Two stateless smooth value-noise channels tilt the face
normal:

```text
n(i,j) = normalize(N + slope_u*T + slope_v*B)
```

Fragments bilinearly interpolate table normals in `(u,v)` space. The table is
indexed by face ID, not traversal position.

## 4. Thin-Film Shading

For wavelength `lambda`, film thickness `d`, refractive index `n`, and air-side
cosine `cos_theta`, Snell's law gives `cos_theta_t`. The two-interface Airy
reflectance is:

```text
delta = 4*pi*n*d*cos_theta_t/lambda
r12 = (1-n)/(1+n)
r23 = (n-n_substrate)/(n+n_substrate)

R = (r12^2 + r23^2 + 2*r12*r23*cos(delta))
    / (1 + r12^2*r23^2 + 2*r12*r23*cos(delta))
    * exp(-absorption*d/cos_theta_t)
```

Spectral samples from 380 to 780 nm are integrated with CIE 1931 approximate
color matching functions and converted to linear sRGB. The surface combines the
resulting interference tint with a Fresnel-weighted microstructure specular
lobe from every point light. Display conversion is monotone and deterministic.

Hue continuity is measured on four stable, near-normal representative faces
over the seven cameras. Hues below 10% saturation are excluded because hue is
numerically unstable near gray. The declared scene threshold is `8°`; the
generated sequence measures `2.307692308°`.

## 5. Caustics

Each source independently samples every elytron face at the scene sampling
density. A surface sample is retained only if it is front-facing to the light,
not self-occluded, and has nonzero thin-film transmission amplitude.

Landing on the leaf plane `y=leaf_y` is:

```text
t = (leaf_y - source_y) / (surface_y - source_y)
landing = source + t*(surface-source)
```

Landings outside the declared leaf rectangle are excluded. For each valid
sample `j`, the report stores its source, face ID, surface point, landing point,
thickness, amplitude, tint, distances, and penumbra width. The formula is also
written into `report.json`, so a landing can be independently recomputed.

The soft shadow half-width is:

```text
d_leaf   = surface_y - leaf_y
d_source = |source - surface|
half_width = base_width
           + distance_gain * d_leaf*d_source/(d_leaf+d_source)
half_width = max(half_width, min_pixel_width*pixel_world_size)
```

The first term is strictly increasing in both distances. The component is a
normalized two-dimensional Gaussian footprint, not a hard polygon, and the
pixel-floor guarantees at least 1.25 pixels of blur in the generated view.

Per-light contribution is:

```text
C_i(p) = gain * sum_j(amplitude_j * tint_j * exp(-r_j^2/(2*sigma_j^2)))
                / sum_j(amplitude_j)
```

The renderer sums `C_i(p)` in sorted source-ID order. Because each source is
evaluated independently and the order is canonical, reversing JSON light order
does not change any tile bytes.

## 6. Energy Accounting

For each active leaf report sample:

```text
combined = fsum(C_L1, C_L2, C_L3)
individual_sum = fsum(C_L1, C_L2, C_L3)
ratio_i = C_i / combined
```

The last ratio stores the residual `1-sum(previous_ratios)` to avoid ratio
rounding drift. The report includes luminance contributions, ratios, ratio-sum
error, combined-minus-individual excess, and forward/reverse order delta. It
also audits every primary-camera leaf pixel (14,405 in the reference). In this
run the ratio-sum error is `0`, additive excess is `5.55e-17`, and order drift
is `2.78e-17`; all are below the `1e-12` floating-point acceptance tolerance.
Every ratio sum is within `1e-6` of one.

## 7. Tiles, Snapshot, and Resume

Blocks are enumerated per camera in row-major order. A snapshot payload
contains:

- Canonical scene digest and film signature.
- Canonical scene summary.
- Completed block IDs and `next_block_index`.
- For every completed block: extents, color/face hashes, color summary,
  thickness statistics, and zlib+base64 RGBA and face-ID tiles.

The envelope has a SHA-256 checksum over canonical payload bytes. Writes use a
same-directory temporary file, `fsync`, and `os.replace`, so a reader sees
either the old complete snapshot or the new complete snapshot.

Resume accepts a snapshot only if all of the following hold:

- Envelope format and checksum are valid.
- Scene digest, film signature, and scene summary match.
- Completed IDs equal the canonical block prefix.
- Every block extent matches the current scene.
- Tile shape/dtype and RGBA/face hashes match their summaries.

Any failure discards the entire snapshot. The stale file is removed before a
fresh render, and no block index or pixel offset is reused.

## 8. Interruption and Comparison

SIGINT/SIGTERM install a flag-only handler before snapshot loading/context
construction. NumPy work is never aborted in the middle of a tile. After the
current tile is rendered and committed, the CLI writes `interrupt_report.json`
and exits with code `0`. No final image files are written.

Forced termination cannot run cleanup; the last atomic snapshot remains valid.
The next `--resume` run restores that prefix.

With `--baseline`, every current RGBA and face-ID array is compared using exact
NumPy equality. Mismatches become records containing:

- camera ID and canonical block ID,
- full-image pixel coordinates,
- channel (`R`, `G`, `B`, `A`, or `face_id`),
- baseline and current values.

Caustic records are compared per source/sample/field. Any difference causes
exit code `3` and `difference_report.json`; an identical run records
`"status":"identical"` and exits `0`.

## 9. Stable Report

`report.json` deliberately contains no timestamps or elapsed times. It records:

- scene/film digests and final image digests,
- per-image visible face count and mean thickness,
- per-block visible face count, mean thickness, RGBA mean, and tile hashes,
- hue threshold, representative hues, and maximum adjacent jump,
- per-light caustic sample counts, landing lists, and penumbra widths,
- penumbra probe brightness/width pairs,
- per-light energy contributions and ratios at leaf samples.

Repeated runs therefore produce byte-identical reports. Elapsed time is printed
only to stdout by the CLI.

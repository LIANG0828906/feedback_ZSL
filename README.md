# Deterministic Layered Aquarium Reference Renderer

This repository is a self-contained CPU renderer for the layered glass-aquarium
reference scene. It builds every mesh, light, material, particle field and camera
sample deterministically in code. It does not use external models, textures,
images, GPUs, containers or network resources.

The sample scene contains:

- three coaxial arc-shell glass layers with per-layer thickness, transmission,
  IOR and tint;
- two annular water layers with dye absorption, scattering colour and seeded
  floating particles;
- four numbered fish quads;
- two point lights;
- a full pebble-ring receiver on the tank floor;
- a nine-angle camera arc.

## Quick Start

Python 3.10+ and NumPy 1.24+ are required.

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
. .venv/bin/activate
python -m pip install -r requirements.txt
python -m pip install -e .
```

Generate the deterministic sample, validate it, and render it:

```bash
aquarium-render sample --out scenes/sample_scene.json
aquarium-render validate --scene scenes/sample_scene.json
aquarium-render render --scene scenes/sample_scene.json --out artifacts/reference_run
```

Without installing the console entry point, use:

```bash
PYTHONPATH=src python -m aquarium_render render --scene scenes/sample_scene.json --out artifacts/reference_run
```

Run the tests:

```bash
PYTHONPATH=src python -m unittest discover -s tests -v
```

## Outputs

Each completed angle produces:

- `angle_XXXX_rgba.png`: 8-bit premultiplied RGBA technical image. RGB is
  sRGB-encoded and never exceeds alpha.
- `angle_XXXX_ownership.png`: per-pixel component ownership, colour-coded by
  `report.json.ownership_legend`; the same ids are also written to a `.npy`
  sidecar for exact audits.
- `angle_XXXX_preview.png`: opaque studio-background preview composed from the
  technical premultiplied RGBA. It is review-only and does not replace the
  technical image.
- `angle_XXXX_rgba.npy`, `angle_XXXX_ownership.npy`: lossless arrays used by
  deterministic comparison tooling.

Global outputs:

- `particle_tables/<water-id>.json`: particle distribution and absorption
  modulation tables.
- `shadow_<light-id>.png`: deterministic top-down receiver shadow map.
- `report.json`: layer transmission/scattering ratios, all projection landing
  coordinates, per-light per-fish soft-edge samples, ownership legend and hashes.
- `progress.snapshot.json`: atomically replaced complete progress snapshot.
- `recovery_report.json`: written only for Ctrl+C / SIGINT graceful stops.
- `run_manifest.json`: completion state, scene summary and output hashes.
- `reference_diff.json`: written only if `--reference` comparison fails.

## Validation and Business Codes

Validation runs before any renderer allocation or output directory work. Errors
are emitted as canonical JSON on stderr and the process exits with code `2`.
Every item contains a business code, component id, JSON path and message.

| Code | Meaning |
| --- | --- |
| `AQ-GEO-001` | triangle index outside the vertex array |
| `AQ-GEO-002` | degenerate triangle |
| `AQ-GEO-003` | malformed vertex/triangle data |
| `AQ-MAT-001` | glass thickness outside `(0, 0.5]` |
| `AQ-MAT-002` | transmission outside `[0, 1]` |
| `AQ-ID-001` | duplicate component, fish, light or receiver id |
| `AQ-WAT-001` | negative water absorption coefficient |
| `AQ-LGT-001` | point light inside a glass/water layer volume |
| `AQ-LAY-001` | missing/duplicate glass or water layer index |
| `AQ-LGT-002` | not exactly two lights |
| `AQ-CAM-001` | illegal camera arc/FOV/radius parameters |
| `AQ-REC-001` | illegal receiver ring or mesh |
| `AQ-FSH-001` | fish quad or layer reference error |
| `AQ-SCH-001` | schema/JSON type error |
| `AQ-RESUME-001` | resumed result differs from reference output |

Light-in-volume checks use a fixed non-axis-aligned parity ray, so boundary
classification does not depend on triangle order.

## Determinism Design

### Stable particles

`aquarium_render.prng.StableRandom` is a SplitMix64 stream keyed by:

```text
seed XOR FNV1a64(component_id) XOR FNV1a64(channel)
```

The particle table is a function of only the layer seed, component id and
particle resolution. Particle count is derived from that resolution. No NumPy
random state, process hash seed, wall clock or triangle traversal order enters
the table. Canonical sorted JSON gives byte-identical table files and hashes.

The absorption modulation grid is a sum of bounded Gaussian footprints. It is
sampled bilinearly at a layer point using normalized `(u, v)` coordinates in
the declared layer extent. This removes per-pixel random sparkles at layer
overlaps.

### Layered transparency

For each block, rays are intersected with every closed shell. Each shell yields
an ordered entry/exit interval. Transparent intervals are sorted by entry
distance, independent of scene list order.

- Glass uses effective optical path `thickness / max(|cos(theta)|, 0.20)` and
  applies `(transmission * tint)^(path / thickness)`. Fresnel reflection is
  bounded by the layer coverage contribution.
- Water uses the actual ray interval length and
  `exp(-sigma_eff * path)`, where
  `sigma_eff = absorption * (1 + particle_strength * modulation)`.
- Absorption and scattering colours differ per water layer, so the two water
  layers remain distinguishable in the composite.
- Every emitted contribution is bounded by its alpha contribution. A saturating
  tone curve and sRGB encoding avoid white-band clipping without lifting black
  pixels. Final 8-bit RGB is clamped to alpha, preserving premultiplication.

`report.json` gives both raw and normalized transmission/scattering
contributions per layer.

### Projection model

For a light `L` and fish vertex `V`, receiver landing points are:

```text
t = (receiver_z - L.z) / (V.z - L.z)
Q = L + t * (V - L)
```

A polygon contributes only when all four points are inside the receiver ring.
Otherwise it is reported as `outside` and shadow coverage is exactly zero.

Water-layer landings intersect the light-to-vertex ray with the adjacent water
layer's declared `projection_radius` cylinder. The nearest positive intersection
is used. The report carries the full point list and centroid, so coordinates can
be recomputed from the scene.

Receiver coverage uses a signed distance to the projected convex quad and a
smoothstep across the penumbra width. Points outside the receiver ring fade
smoothly to zero; no hard black border is generated.

The scene-authored penumbra response is:

```text
density_factor = 1 + 0.35 / sqrt(shadow_samples)
width = shadow_softness_base * density_factor
        * (1 + fish_to_receiver_distance / scale)
        * (1 + light_to_fish_distance / scale)
```

It is monotonic in both requested distances. Higher declared sampling density
also supersamples the grayscale shadow map. Same scene and density produce
byte-identical shadow maps.

## Snapshot, Signals and Resume

The renderer processes one image block at a time in angle-major, row-major order.
After every committed block it fsyncs and atomically replaces
`progress.snapshot.json`. A snapshot contains:

- scene summary and scene hash;
- particle seed/table fingerprint;
- image/block dimensions and angle list;
- every committed RGBA and ownership block;
- block colour summaries and per-block SHA-256 values;
- cumulative layer contribution statistics;
- a canonical SHA-256 checksum over the whole payload.

Only this snapshot is read on resume. Incomplete PNG/NPY files, temporary
snapshot files and old block offsets are ignored. The block sequence must be
contiguous from `(angle=0, block=0)`.

If the scene summary, camera layout or particle fingerprint changes, the
snapshot checksum/content is corrupt, or the block sequence is non-contiguous,
the snapshot and all derived image outputs are discarded and rendering restarts
at angle 0.

- `Ctrl+C` / `SIGINT`: a handler sets a cooperative flag. The current block is
  completed, the snapshot is committed, `recovery_report.json` is written and
  the process exits with integer code `0`.
- `SIGTERM` / forced termination: no special handling is installed. The last
  atomically committed snapshot remains usable; a partially written snapshot
  cannot replace it.

To prove resume equivalence, render a completed reference directory and compare:

```bash
aquarium-render render --scene scenes/sample_scene.json --out artifacts/resumed \
  --reference artifacts/reference_run
```

or compare two complete directories:

```bash
aquarium-render compare --scene scenes/sample_scene.json \
  --reference artifacts/reference_run --actual artifacts/resumed \
  --output artifacts/reference_diff.json
```

On mismatch, `AQ-RESUME-001` JSON includes angle index, block id, pixel
coordinates, channel and both values. The process exits with code `1`.

## Scene Schema Summary

The root object contains:

- `schema_version`: must be `"1.0"`;
- `layers`: exactly three `glass` and two `water` entries, each with unique
  `component_id`, unique `layer_index`, and a triangulated closed `mesh`;
- glass: `thickness`, `transmission`, `ior`, `tint`;
- water: `absorption_coefficient`, `scattering_color`, `particle_seed`,
  `particle_resolution`, `particle_strength`, `projection_radius`;
- `fish`: unique `fish_id`, `water_component_id`, colour/opacity and a four-point
  quad mesh;
- `lights`: exactly two point lights with position, colour, intensity and radius;
- `receiver`: pebble ring mesh plus plane/radial/angular bounds and palette;
- `camera`: orbit centre, radius, eye height, target, arc endpoints, sample count
  and FOV;
- `render`: image/block dimensions, exposure, shadow density, softness
  parameters, background, ambient and particle modulation scale.

See `scenes/sample_scene.json` for the complete generated example.

## Local Verification Performed

The checked-in reference was generated from `scenes/sample_scene.json` with
`128 x 96` pixels, `32 x 24` blocks and 9 camera angles. The automated suite
covers 17 cases:

- all required business-error categories;
- particle table byte identity and triangle-order independence;
- two fresh renders with identical PNG bytes and projection coordinates;
- premultiplied-alpha and no-white-clipping checks;
- receiver/water projection coordinates, two-light overlap and monotonic
  penumbra widths;
- deterministic grayscale shadow maps;
- actual `SIGTERM` + resume and actual `SIGINT` graceful-stop + resume;
- snapshot corruption and particle-seed reset;
- structured pixel diff fields.

The final local test result is stored in `artifacts/test_results.txt`; the
deterministic run summary is in `artifacts/acceptance_summary.json`.


# Paper Lantern Validation Renderer

NumPy-only offline validator for layered paper-lantern scenes. It generates a
deterministic five-layer scene and paper-fiber/cutout textures, validates the
scene, renders with tiled double-sided transmission and projected soft cutout
shadows, and can resume from an atomic block snapshot after interruption.

No external image, texture, network, or container runtime is used.

## Quick Start

Windows PowerShell:

```powershell
py -3.10 -m pip install -r requirements.txt
py -3.10 -m paper_lantern generate --output examples\scene.json
py -3.10 -m paper_lantern validate examples\scene.json --report examples\validation_report.json
py -3.10 -m paper_lantern render examples\scene.json -o render_out
```

The default generated scene is 192x144, has five layers, 32x24 blocks, and 16
soft-shadow samples. A pre-generated copy and a completed render are included:

- `examples/scene.json`
- `artifacts/demo/report.json`
- `artifacts/demo/preview.png`

The package can be used in place from the repository root, or installed with
`python -m pip install -e .`.

## Commands

```text
paper-lantern generate [--output scene.json] [--seed 206] [--width 192]
                       [--height 144] [--tile-width 32] [--tile-height 24]
                       [--layers 5] [--shadow-samples 16] [--softness 0.045]
                       [--emit-textures DIR]

paper-lantern validate SCENE [--report REPORT]

paper-lantern render SCENE -o OUTPUT [--fresh] [--stop-after-blocks N]
                         [--block-delay-ms MS]
```

`--stop-after-blocks` performs a deterministic graceful interruption after the
specified number of newly completed blocks. `SIGINT` and `SIGTERM` also stop
after the block currently being committed. `--block-delay-ms` is only a test
aid for externally killing the process; it does not affect rendered values.

Exit codes:

- `0`: completed, or recoverable graceful interruption.
- `1`: runtime/output failure.
- `2`: scene validation failure. No final image is written.
- `4`: resumed output differed from a fresh one-shot render. A structured
  `difference_report.json` is written.

## Scene Contract

The validator consumes one JSON object:

```json
{
  "schema_version": 1,
  "seed": 206,
  "image": {
    "width": 192,
    "height": 144,
    "tile_width": 32,
    "tile_height": 24,
    "background": [0.012, 0.010, 0.009]
  },
  "camera": {
    "position": [0.0, 0.0, 2.65],
    "target": [0.0, 0.0, 0.54],
    "up": [0.0, 1.0, 0.0],
    "fov_y_deg": 40.0,
    "near": 0.02,
    "far": 8.0
  },
  "light": {
    "position": [0.24, 0.11, -0.58],
    "color": [1.0, 0.78, 0.52],
    "intensity": 3.0
  },
  "shadow": {
    "samples": 16,
    "softness": 0.045,
    "probe_light_distances": [0.75, 1.25, 1.75, 2.25, 3.0]
  },
  "layers": [
    {
      "layer_index": 0,
      "transmittance": 0.46,
      "scatter_color": [0.96, 0.70, 0.42],
      "texture_resolution": 96,
      "mesh": {
        "vertices": [[-0.56, -0.39, 0.14], [0.56, -0.39, 0.14], [0.56, 0.39, 0.14]],
        "triangles": [[0, 1, 2]],
        "uvs": [[0.0, 0.0], [1.0, 0.0], [1.0, 1.0]]
      }
    }
  ]
}
```

`layer_index` is the authoritative inner-to-outer order. Input list order is
ignored after validation. `softness` is an angular source radius in radians.
For a receiver at distance `d`, the synthetic finite-source radius is
`tan(softness) * d`, giving the reported half-penumbra width
`tan(softness) * light_distance`.

Meshes may be planar or non-planar. The planar path maps a ray/plane hit to UV
and samples the inner cutout table directly. Non-planar meshes use a
vectorized ray/triangle fallback.

### Validation Codes

Validation writes every discovered issue with `code`, `layer_index`, and
`reason`. No image or snapshot is written on failure.

```text
SCENE_JSON_INVALID
SCENE_SCHEMA_INVALID
LAYER_INVALID
DUPLICATE_LAYER_INDEX
MESH_INVALID
MESH_VERTEX_INVALID
MESH_INDEX_OUT_OF_RANGE
TRIANGLE_DEGENERATE
MESH_UV_INVALID
TRANSMITTANCE_OUT_OF_RANGE
SCATTER_COLOR_OUT_OF_RANGE
TEXTURE_RESOLUTION_OUT_OF_RANGE
LIGHT_INSIDE_LAYER
```

`LIGHT_INSIDE_LAYER` covers a point on a triangle or strictly inside a
watertight mesh, detected by a winding/solid-angle test.

## Rendering Model

Each layer is rasterized twice-sided with perspective-correct UV interpolation:

- Incident factor uses `abs(dot(N, L))`, so front-lit and back-lit sides both
  contribute.
- Effective paper transmission is
  `clamp(layer.transmittance * texture_transmission_modulation, 0, 1)`.
- Scatter adds `scatter_color * texture_scatter_modulation`, also with
  two-sided incidence weighting.
- A deterministic inverse-square light falloff and `1 - exp(-radiance)` tone
  rolloff avoid hard white clipping.
- Layer opacity is `paper_mask * (1 - effective_transmission)`, stored with
  premultiplied RGB.

For every receiver fragment, `shadow.samples` low-discrepancy rays are traced
from a deterministic finite light disk. Each ray intersects all lower-index
layers. A cutout has transmission `1`; paper has its own material
transmission. Rays that miss a layer, or whose intersection falls outside
that layer's UV/mesh domain, are unoccluded. Averaging those rays produces a
soft edge without turning the outside of a paper edge into a black blocker.

Per-layer buffers are composited in explicit ascending `layer_index` order
using source-over. Per-pixel optical depth is the opacity-weighted camera-space
distance, so it is deterministic for the same pixels and layers.

### Deterministic Textures

`texture_resolution x texture_resolution x 3` little-endian float32 tables are
generated from `(seed, layer_index, resolution)` only:

1. `paper_mask`: `1` paper, `0` cutout, with bilinear edge blending.
2. `transmittance_modulation`: fiber modulation on paper.
3. `scatter_modulation`: independent fiber modulation on scattered light.

Fiber noise uses a local integer SplitMix64 hash, fixed sinusoid directions,
and explicitly seeded value noise. There is no unseeded RNG and no dependence
on layer traversal order. Texture manifests and bundles are emitted under
`textures/`.

## Outputs

Every successful run writes:

```text
image.rgba               uint8 RGBA, row-major, premultiplied
image.png                straight-alpha RGBA preview
preview.png              composited over scene background
depth.f32                little-endian float32 optical depth, row-major
layers/layer_NN.rgba     per-layer uint8 premultiplied RGBA
textures/layer_NN.f32    raw texture tables
textures/manifest.json   shape/channel/hash manifest
report.json              stable render statistics
```

Raw dimensions come from `report.json`. PNG files are convenience previews;
raw files are the byte-comparable artifacts. `report.json` contains no
timestamps or absolute paths. Repeated uninterrupted runs of the same scene
produce byte-identical raw images, depth, textures, block summaries, and
report fields.

The report includes:

- Every layer's index, visible/paper/cutout fragment counts, average effective
  transmittance, mean premultiplied color/alpha, file, and SHA-256.
- Soft-shadow sample count, sample rays, occluder tests, softness, and
  monotonic half-penumbra probes.
- Completed block count/order, each block's RGBA/depth hash and color means.
- Image/depth digests, recovery count, and snapshot status.

## Snapshot And Resume

After every completed block, the complete renderer state is written to
`snapshot.bin` as a temporary file, fsynced, and atomically replaced. The file
contains:

- magic/version and canonical header;
- scene digest and image/block layout;
- contiguous completed block IDs and per-block color summaries;
- recovery count, shadow counters, all floating accumulator arrays and stats;
- a SHA-256 payload checksum.

On restart, only `snapshot.bin` is used. It is accepted only when the scene
digest/layout match, the completed block list is a valid prefix, every block
summary recomputes exactly, payload bytes and checksums validate, and
uncommitted regions are still zero. Otherwise the entire snapshot is discarded
and rendering starts at block `0`; no old block offset or image is reused.

Snapshot statuses in the final report are:

```text
none
resumed
discarded_corrupt
discarded_scene_mismatch
```

A graceful interruption writes `resume_report.json` with completed blocks,
next block, scene digest, and block summaries, then exits `0`. A forced kill
leaves the last atomically committed snapshot. After completing a resumed
render, the implementation also renders a fresh reference and compares the
quantized RGBA and float32 depth byte-for-byte. A mismatch writes
`difference_report.json` containing block, pixel, channel, resumed value, and
reference value, then exits `4`.

## Verification

Run the NumPy-only suite:

```powershell
py -3.10 -m unittest discover -s tests -v
```

Actual local verification result:

```text
Ran 23 tests in 6.5s
OK
```

Covered cases include duplicate layer indices, out-of-range mesh indices,
degenerate triangles, transmittance range, light inside a closed mesh,
no-image validation failure, texture byte determinism and layer-order
independence, input-layer-order-independent compositing, front/back shading,
projected cutout variation, unoccluded rays outside an inner sheet,
softness-controlled transitions, deterministic report ordering, graceful
resume, corrupt/snapshot scene-mismatch discard, forced SIGKILL resume, and
the structured resume-difference report.

The same suite also passes under Python 3.11.18 / NumPy 2.4.6. A real
`SIGTERM` run exited `0`, produced `resume_report.json`, and resumed to the
included demo's exact RGBA and depth hashes.

Included 192x144 demo, uninterrupted:

```text
scene_digest  f56a207f730f56e8e4c96e981be335a9d09763b33ae57cfb03fa0663fa19f901
RGBA SHA-256  a6f44651bcae9c06014c82cf4da17f247897029880da3a0b97042968a55b4a07
depth SHA-256 8ab67d9c836c16000796473bca8bbd133386850821948d17dc03aca0b9d46bd6
layers        0..4, average transmittance 0.3897 .. 0.6079
blocks        36 / 36
shadow rays   1,013,984; occluder tests 2,997,152
penumbra      widths 0.03377 .. 0.13509, monotonic yes
```

The verification run used Python 3.14.4 and NumPy 2.5.5 on the provided
environment. The implementation itself targets Python 3.10+ and uses no
platform-specific rendering or external asset dependencies.

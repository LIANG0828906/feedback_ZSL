# Binary Scene Format `VSCN0001`

All multi-byte values are little-endian. The format is deliberately small and
fixed for metadata, followed by independently indexed float64 density payloads.
Offsets in chunk records are absolute file offsets.

## Header

| Offset | Size | Type | Field |
| ---: | ---: | --- | --- |
| 0 | 8 | bytes | magic `VSCN0001` |
| 8 | 4 | `u32` | version, currently `1` |
| 12 | 4 | `u32` | screen width |
| 16 | 4 | `u32` | screen height |
| 20 | 4 | `u32` | media count `M` |
| 24 | 4 | `u32` | density field count `D` |
| 28 | 24 | `f64[3]` | light direction, surface-to-light |
| 52 | 24 | `f64[3]` | light radiance RGB |
| 76 | 72 | `f64[9]` | camera intrinsics `K`, row-major |
| 148 | 128 | `f64[16]` | `world_to_camera`, row-major `[R|t; 0 1]` |
| 276 | | | media records |

The camera pixel center uses `(pixel_x + 0.5, pixel_y + 0.5)`. Rays are formed
with `normalize(inverse(K) @ [u, v, 1])` and transformed by
`inverse(world_to_camera)`.

## Media Record

Each record is 132 bytes:

| Relative offset | Size | Type | Field |
| ---: | ---: | --- | --- |
| 0 | 4 | `u32` | medium id |
| 4 | 1 | `u8` | kind: `0` box, `1` sphere |
| 5 | 3 | bytes | reserved, zero |
| 8 | 4 | `u32` | density field id |
| 12 | 24 | `f64[3]` | scattering RGB |
| 36 | 24 | `f64[3]` | absorption RGB |
| 60 | 8 | `f64` | Henyey-Greenstein `g`, in `(-1, 1)` |
| 68 | 8 | `f64` | density scale, non-negative |
| 76 | 24 | `f64[3]` | box minimum or sphere center |
| 100 | 32 | `f64[4]` | box maximum in `[0:3]` or sphere radius in `[0]` |

Boxes require `max > min` on every axis. Spheres require a positive radius.
Intersecting media must be disjoint or strictly nested; partial overlaps are
rejected.

## Density Field Record

Each field record is 32 bytes:

| Relative offset | Size | Type | Field |
| ---: | ---: | --- | --- |
| 0 | 4 | `u32` | field id |
| 4 | 12 | `u32[3]` | dims `(nx, ny, nz)` |
| 16 | 12 | `u32[3]` | chunk shape `(cx, cy, cz)` |
| 28 | 4 | `u32` | chunk count |

Field metadata records are followed by all chunk index records in field order.
Each chunk index record is 28 bytes:

| Relative offset | Size | Type | Field |
| ---: | ---: | --- | --- |
| 0 | 12 | `u32[3]` | chunk `(x, y, z)` |
| 12 | 8 | `u64` | absolute payload offset |
| 20 | 8 | `u64` | payload byte length |

The chunk key ranges are
`0 <= x < ceil(nx/cx)`, and likewise for `y` and `z`. Every chunk must appear
exactly once. Payload length must equal `nx_chunk * ny_chunk * nz_chunk * 8`.

## Density Payload And Sampling

Each payload is `f64` values in C order with logical shape
`(nz_chunk, ny_chunk, nx_chunk)`. In other words, x is the fastest-changing
axis.

A field is mapped to the axis-aligned bounds of every medium that references it.
World positions are normalized into those bounds and sampled trilinearly. All
eight interpolation stencil chunks are recorded as tile dependencies, including
chunks used by shadow rays.

Density values must be finite and non-negative.

## Validation

The full input is decoded and validated before a renderer is created. Checked
items include:

- magic and version;
- screen dimensions and metadata safety limits;
- camera finiteness, determinant, condition number and homogeneous row;
- light direction and radiance;
- unique media/field IDs and valid references;
- finite, non-negative material parameters;
- box/sphere degeneracy and medium nesting;
- exact chunk coverage, dimensions and payload lengths;
- finite, non-negative density values.

General malformed input maps to `E_SCENE_INVALID`. Input-boundary injection uses
the dedicated codes listed in the README.

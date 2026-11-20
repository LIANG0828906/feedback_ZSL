# VRVOL001 File Format

All fields are little-endian. The fixed header is 72 bytes followed by
`shape_z * shape_y * shape_x` raw voxels in C order.

## Header Layout

| Offset | Size | Type | Field |
| ---: | ---: | --- | --- |
| 0 | 8 | `char[8]` | magic, exactly `VRVOL001` |
| 8 | 2 | `uint16` | format version, currently `1` |
| 10 | 2 | `uint16` | header size, exactly `72` |
| 12 | 1 | `uint8` | dimension count, exactly `3` |
| 13 | 1 | `uint8` | scalar type code |
| 14 | 1 | `uint8` | flags |
| 15 | 1 | `uint8` | reserved, zero |
| 16 | 12 | `uint32[3]` | shape `(nx, ny, nz)` |
| 28 | 12 | `float32[3]` | spacing `(sx, sy, sz)` in world units |
| 40 | 4 | `float32` | window level, raw scalar units |
| 44 | 4 | `float32` | window width, raw scalar units |
| 48 | 8 | `uint64` | voxel count |
| 56 | 8 | `uint64` | payload byte count |
| 64 | 4 | `uint32` | CRC32 of raw payload |
| 68 | 4 | `uint32` | CRC32 of header with this field zeroed |

Struct format:

```text
<8sHHBBBB3I3fffQQII
```

## Scalar Type Codes

| Code | NumPy dtype |
| ---: | --- |
| 0 | `uint8` |
| 1 | `int16` |
| 2 | `float32` |
| 3 | `uint16` |

## Flags

| Bit | Meaning |
| ---: | --- |
| 0 | intensity/density volume |
| 1 | segmentation mask |

Other bits are reserved and ignored by version 1.

## Validation

Loading rejects:

- files shorter than the header,
- magic/version/header-size/dimension mismatches,
- unsupported scalar codes,
- inconsistent voxel/payload counts,
- payload length mismatch,
- header CRC or payload CRC mismatch.

The loader returns an immutable header plus a contiguous NumPy array with layout
`(nz, ny, nx)`. The header's public shape is `(nx, ny, nz)`.

## Deterministic Samples

`scripts/generate_samples.py` creates:

- `samples/density_64.vol`: `int16`, `64³`, spacing `(1,1,1)`,
  window `(level=120, width=260)`.
- `samples/segmentation_64.vol`: `uint8`, `64³`, labels
  `0=background, 1=body, 2=lesion, 3=vessel, 4=outer shell`.

The generator is analytic and does not use random numbers. See
`samples/manifest.json` for exact hashes, counts and generation parameters.

```text
density_64.vol
sha256 5501875c09bc4482b2d748519e656ee369324f70ea39f2a0364ee404967fb37b

segmentation_64.vol
sha256 dd130bf0ae9735636de043573723ddec9db075927b10b8341638f137aecce340
```

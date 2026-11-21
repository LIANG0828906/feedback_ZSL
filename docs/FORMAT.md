# Binary Formats

All integer and floating-point fields are little-endian. Floating-point
elevations are IEEE-754 binary32. Content hashes are SHA-256 over the payload
bytes in row-major order. Header size includes the magic and header fields, but
not the payload.

## Height Field (`THF1`)

Header size: 92 bytes.

| Offset | Type | Field |
|---:|---|---|
| 0 | `char[4]` | `THF1` |
| 4 | `u32` | header size, 92 |
| 8 | `u32` | format version, 1 |
| 12 | `u32` | vertex width |
| 16 | `u32` | vertex height |
| 20 | `f32` | cell spacing |
| 24 | `u32` | height type, 0 = float32 |
| 28 | `f32` | valid minimum |
| 32 | `f32` | valid maximum |
| 36 | `u32` | flags, 0 |
| 40 | `u64` | patch baseline version |
| 48 | `u64` | payload byte count |
| 56 | `u8[32]` | SHA-256 of float32 payload |
| 88 | `u32` | reserved, 0 |
| 92 | payload | `width * height` float32 values |

Validation order is magic, header size, format, dimensions, spacing, height
type, valid range, flags, payload length, payload hash, finiteness and valid
range. The first failed check returns `E_HEIGHT_INVALID` with the field name,
for example `width`, `valid_range`, `height[x,y]`, `payload_bytes` or
`payload_hash`. Dimensions are limited to 65535 because block IDs pack two
16-bit corners.

## Material Image (`TMI1`)

Header size: 80 bytes.

| Offset | Type | Field |
|---:|---|---|
| 0 | `char[4]` | `TMI1` |
| 4 | `u32` | header size, 80 |
| 8 | `u32` | format version, 1 |
| 12 | `u32` | width |
| 16 | `u32` | height |
| 20 | `u32` | channels, 1 |
| 24 | `f32` | cell spacing |
| 28 | `u32` | palette size |
| 32 | `u64` | baseline version |
| 40 | `u64` | payload byte count |
| 48 | `u8[32]` | SHA-256 of u32 index payload |
| 80 | payload | `width * height` u32 indices |

The image dimensions and spacing must match the height field. Every index must
be smaller than the palette size.

## Height Patch (`THP1`)

Header size: 96 bytes.

| Offset | Type | Field |
|---:|---|---|
| 0 | `char[4]` | `THP1` |
| 4 | `u32` | header size, 96 |
| 8 | `u32` | format version, 1 |
| 12 | `u32` | target x |
| 16 | `u32` | target y |
| 20 | `u32` | patch width |
| 24 | `u32` | patch height |
| 28 | `f32` | cell spacing |
| 32 | `u32` | height type, 0 |
| 36 | `u32` | reserved, 0 |
| 40 | `u64` | baseline version |
| 48 | `u64` | patch sequence |
| 56 | `u64` | payload byte count |
| 64 | `u8[32]` | SHA-256 of float32 payload |
| 96 | payload | `width * height` float32 values |

## Material Patch (`TMP1`)

Same 96-byte layout as `THP1`; magic is `TMP1`, offset 32 is channel count and
the payload is `width * height` u32 material indices.

Validation rejects a rectangle outside the target image with
`E_PATCH_BOUNDS`. A baseline different from the current committed version, or a
sequence that is not `current + 1`, returns `E_PATCH_STALE`. Height and material
patches in one batch must have the same baseline and sequence. No terrain bytes
are written until all checks pass.

## Manifest

The current commit marker is `manifest.current`, an ASCII key/value file:

```text
TERRAIN_MANIFEST 1
version=2
patch_sequence=2
patch_hash=<sha256 of batch metadata and payload hashes>
height_file=v000002.height.bin
height_hash=<sha256 of height payload>
material_file=v000002.material.bin
material_hash=<sha256 of material payload>
prev_manifest_hash=<previous manifest hash>
content_chain=<sha256(previous chain | patch hash | height hash | material hash | sequence)>
invalid_count=...
invalid_blocks=<comma-separated decimal block ids>
fallback_full=0
manifest_hash=<sha256 of every preceding byte>
```

Every committed version also keeps `manifests/vNNNNNN.manifest`. Snapshots are
versioned, so a failed or interrupted commit cannot overwrite a committed
snapshot. Recovery trusts only `manifest.current`, verifies the manifest hash
history and snapshot payload hashes, then replays contiguous patch batches with
sequence greater than the committed version.


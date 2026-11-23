# Data and Cache Format

## Project Directory

```text
data/
  manifest.json
  truth.json
  edits.json                 # optional edit overlay
  blocks/
    B_0_0/
      height.f64             # row-major float64 little-endian
      material.u16           # row-major uint16 little-endian, layers interleaved
      passability.u8         # row-major uint8
```

The generated sample uses:

- world grid `4 x 2`, block grid `24 x 16`, sample spacing `1.0`;
- six active blocks forming two terrain islands;
- seed `20261124`;
- three quantized material layers (`quantMax=10000`);
- subdivision `2`;
- seam angle threshold `0.25°`, five allowed relaxation iterations;
- seam material band `1` coarse cell;
- stats/scatter caps and tolerances in `manifest.json` and `truth.json`.

## Manifest Essentials

Each block declares:

- ID and logical col/row;
- grid size and world origin;
- height file spec and dimensions;
- material file spec, dimensions, layer count, quantization maximum;
- passability file spec and dimensions;
- scatter seed, density per cell, mask threshold, and instance cap.

Each seam declares `id`, blocks `a`/`b`, and opposite edges
(`right`/`left`, `top`/`bottom`). The loader verifies logical adjacency.

## Truth

`truth.json` contains:

- height samples at coarse sample coordinates with absolute tolerances;
- per-block/per-layer input material coverage ratios with tolerance `0.006`
  (seam-band equalization accounts for the small difference from final ratios);
- expected scatter counts from passability/density with tolerance `2`;
- expected seam maximum normal angle `0` with numerical tolerance `1e-4`.

`bake -validate` checks all 53 declared samples.

## Edit Overlay

```json
{
  "schemaVersion": 1,
  "edits": [
    {
      "seq": 1,
      "block": "B_0_0",
      "kind": "height",
      "col": 10,
      "row": 10,
      "value": 2.5
    }
  ]
}
```

Material edits also use a `layer` and an integer quantized `value`. Other layers
in the cell are rebalanced deterministically so the cell still sums to
`quantMax`.

## Snapshot

`cache/snapshot.json` is a single atomic JSON document. Its `entries` are in
stable commit order. Each entry embeds canonical artifact JSON as base64 and
carries:

- `blockId`;
- `cacheKey`;
- sorted `dependencies`;
- `artifactType`;
- `artifactSha256`;
- artifact bytes.

The top level stores content hash, algorithm version, project parameter
version/hash, generation, and commit order. Any mismatch rejects the whole
snapshot.

## Render Manifest

`build/manifest.json` lists every block in stable order with:

- status/cache-hit state and last bake duration;
- artifact file/hash;
- cache key and dependency set;
- vertex/instance counts;
- AABB;
- per-seam and aggregate seam statistics;
- material ratios and failure details when present.

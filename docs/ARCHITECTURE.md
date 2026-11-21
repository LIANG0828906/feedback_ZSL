# Architecture and invariants

## Pipeline

```
load/parse -> compatibility normalization -> flatten at declared tolerance
           -> candidate/intersection splitting -> winding side classification
           -> oriented boundary stitching -> self-intersection gate
           -> truth raster + bidirectional boundary error
           -> atomic committed result + immutable preview frame
```

The pure Python geometry core has no GUI dependency. PySide6 is only imported by
`vpipe.ui`; CLI and tests run without a desktop session.

## Tolerance propagation

An operation has one explicit working `tolerance`. It is used for:

- curve flattening error,
- intersection classification,
- point registry snapping,
- right/left boundary membership probes,
- raster boundary tolerance,
- error thresholds and reported amplification,
- project serialization and preview metadata.

Intersection results carry old/new tolerance, hit status, tangents, confidence,
added/removed points, classification changes, and maximum point delta.
Near-boundary cases are `uncertain`, not `intersect`. Boolean classification and
result error are both written to the report.

## Boolean boundary selection

Both operands are flattened with parameter-preserving adaptive subdivision.
Candidate edge pairs are found through an AABB grid. Edges are split at
crossings and near-collinear overlaps. For every split edge, two points are
probed just left and right of the midpoint. The operation truth
(`a|b`, `a&b`, `a&!b`, `a^b`) is evaluated using winding/even-odd membership.
The edge is retained only when truth changes across it and is oriented with the
inside on its left. This naturally produces CCW exteriors and CW holes.

Duplicate retained edges merge provenance. Opposite duplicate edges cancel.
Retained edges are stitched into closed loops. Loop nesting depth fixes final
direction. Non-adjacent output intersections raise `E_SELF_INTERSECT` before
commit. Every result segment retains source operand, contour, segment, and
Bezier interval provenance.

## Commit and concurrency

`BooleanService` permits one in-flight result per unordered input-geometry pair;
any second operation on that pair returns `E_BUSY`. Cancellation is cooperative
and checked at intersection, split, and classification boundaries.

All computation is local to a worker. A result becomes visible only after:

1. boolean loops close,
2. self-intersection check passes,
3. error report completes,
4. raster frame completes,
5. the cancel token is rechecked.

`ResultStore.commit` publishes the result record and immutable raster frame under
one lock. Cancellation, failure, or force termination therefore leaves the last
committed record untouched. Forced process termination does not promise an exit
report; recovery is only from the on-disk committed project.

## Preview

Rasterization uses a fixed sample grid and scanline interval fill. Identical
geometry, fill rule, bounds, colors, and grid produce byte-identical RGBA and
SHA-256. A preview frame is bound to the committed result hash. If a caller asks
for a hash absent from the committed manifest or no longer latest,
`E_PREVIEW_STALE` is returned and the UI stops its refresh timer.

## Error evaluation

Truth area is a scanline raster of the requested boolean operation over the two
input paths. Output area uses the same grid. Area symmetric difference is the
count of differing cells times cell area.

Boundary error samples operation-boundary portions of the participating input
contours and output contours, then performs bidirectional nearest-segment
queries through a spatial index. The reported value includes a conservative
sampling/flattening upper-bound term. A result above either declared threshold
is `degraded`; export checks this flag and raises `E_EXPORT_DEGRADED`.

## Error codes

- `E_PARSE_INVALID`: malformed document, unclosed contour, dangling group.
- `E_SHARE_INVALID`: v2 share index/coordinate/count mismatch.
- `E_VERSION_LOSS`: report on lossy v1 export.
- `E_SELF_INTERSECT`: output self-intersection; no commit.
- `E_BUSY`: an operation for the geometry pair is already running.
- `E_CANCELLED`: cooperative cancellation.
- `E_PREVIEW_STALE`: preview binding does not match committed list.
- `E_PROJECT_MISMATCH`: project format/version/hash or commit-state mismatch.
- `E_EXPORT_DEGRADED`: degraded/uncommitted result export refused.
- `E_BOOLEAN_FAILED`: malformed/open retained-edge graph.
- `E_INTERNAL`: unexpected worker exception; no commit or preview replacement.


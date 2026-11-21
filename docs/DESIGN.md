# Incremental Terrain Design

## Blocks and LOD

The root covers all terrain quad cells. A node is split while it is larger than
the configured block size or while its vertex height range exceeds the error
threshold and the minimum block size has not been reached. Each node stores its
rectangle, parent/children, depth, elevation extrema, mesh stride and a SHA-256
content hash.

A patch rebuilds only the immediate parents of intersecting leaves and their
edge neighbors. Rebuilding from a parent permits both splitting and merging, so
high-frequency patches can refine the tree while smoothed regions can coalesce.
Unrelated leaves and their render tiles remain live.

Each leaf mesh keeps a full-resolution boundary ring and samples its interior at
the leaf LOD stride. All chunks therefore contain matching boundary vertices at
every cell coordinate. Mesh seams are checked from the generated meshes, not
from the source height buffer. An injected mismatch returns
`E_CRACK_UNRESOLVED` with both adjacent block IDs.

## Dependency Invalidation

The graph has these node classes per leaf:

- `height`
- `material`
- `lod`
- `normal`
- `material-blend`
- `offscreen`
- one `seam` node per adjacent pair

Edges include height -> LOD/normal, LOD -> seam/offscreen, neighboring height ->
normal (one-cell stencil halo), material -> blend -> offscreen, seam -> both
offscreen nodes, and normal -> offscreen. Invalidation starts from leaves that
intersect the patch rectangles and performs a directed closure. LOD changes and
unprovable halo effects conservatively add neighbors; every block carries a
human-readable reason in the report. There is no whole-tree fallback flag.

## Surface and Materials

Normals use central differences over the global height grid, so a block edge
borrows the adjacent block's elevations instead of clamping to its own extent.
Only the changed rectangle expanded by one vertex plus invalidated blocks and
their neighbors are recomputed. A full surface pass is retained as a test
oracle. The measured maximum normal angle difference is currently 0 degrees.

Material colors bilinearly blend palette colors sampled from the index image.
Only the material rectangle expanded by one vertex is refreshed. The measured
maximum material component difference against full recomputation is 0.

## Offscreen Rendering

The deterministic software renderer maps each leaf to a disjoint screen tile at
a fixed orthographic view and fixed light. The tile contains RGBA8 color and a
16-bit normalized error heat buffer. Only dirty tiles are rasterized; unaffected
tiles are reused and all tiles are composited in sorted leaf order.

The acceptance path renders a second frame with a fresh full renderer and
compares both buffers byte-for-byte. It also repeats an incremental render with
an empty dirty set and compares hashes. Summaries contain patch sequence,
invalid block count, rerendered/reused block counts, maximum LOD error, screen
size and SHA-256 color/heat hashes.

The optional OpenGL preview uploads the deterministic color buffer to a texture
and draws it through an OpenGL 3.3 framebuffer object.

## Transaction and Recovery

A patch is evaluated on copies of the height field, material image, quadtree,
surface cache, mesh cache, renderer tile cache and manifest state. Bounds,
stale versions, crack failures, surface tolerance failures and render mismatches
discard those copies.

Commit writes new versioned height/material snapshots durably, syncs the
snapshot directory, then writes the version manifest, then atomically renames
`manifest.current`. The current manifest is the sole commit marker. Failure
before that rename leaves the previous version valid; versioned snapshots may be
orphaned but are never referenced. `TERRAIN_FORCE_KILL=1` combined with a
pre-manifest fault performs a real `_Exit(77)` and is covered by the test suite.

## Concurrency

`AsyncTerrainController` owns one worker slot. A second submit while a job is
active returns `E_BUSY`. Cancellation is polled between copy, LOD, surface,
seam, render and commit phases. Completion (success, cancellation or exception)
always clears the busy flag. Cancellation before commit discards working copies
and retains the last committed state.


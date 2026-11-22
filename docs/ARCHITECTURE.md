# Architecture

## 1. Scene model and conventions

The scene is a forest of scene nodes. Every node has a stable string id, an optional parent id, and a row-major `System.Numerics.Matrix4x4` local transform. World transforms follow row-vector semantics:

```text
world(root)    = local(root)
world(child)   = local(child) * world(parent)
world(point)   = Vector3.Transform(localPoint, world)
```

Geometry types:

- `aabb`: local min/max box.
- `sphere`: local center and radius.
- `triangleMesh`: local vertices plus triangle indices.

A single node may own multiple geometries. Geometry entries are stable objects keyed by `entry:{geometryId}`.

Content hashing includes seed, camera, node hierarchy/local matrices and full geometry data. Structure hashing includes node ids/parents and geometry ownership/kind. All numeric formatting uses round-trip invariant culture, so generation is deterministic across runs with the same runtime.

## 2. Hierarchical spatial index

The index mirrors the scene hierarchy instead of using one monolithic global BVH:

- `GeometryEntry` stores local bounds, world bounds, owner, transform version and cache status.
- `NodeDerived` stores the node world matrix and the aggregate bounds of geometry owned directly by that node.
- `IndexNode` stores the bounding volume of the node subtree (own geometry + all child subtrees).

Queries recursively traverse `IndexNode` summaries. A ray first tests the subtree AABB; only intersecting subtrees test their own geometry. Box selection projects subtree summaries and geometry AABBs. This makes the culling structure hierarchy-aware and allows an edit to update only one branch plus its ancestor summaries.

Node own-bounds and subtree index summaries are intentionally separate. A leaf edit can reuse its ancestor's world transform and own-bounds cache, while ancestor subtree summaries are updated locally.

## 3. Versions and freshness

There are three observable version families:

- `NodeDerived.TransformVersion`: bumped only when the node or an ancestor changes its world transform.
- `GeometryEntry.NodeTransformVersion`: must equal the owner's current `TransformVersion`; otherwise the entry cannot join a query.
- `CacheVersion`/`BoundsVersion`/`IndexNode.CacheVersion`: identify the rebuild generation of a derived object.

`ValidateDerivedState` checks that every scene node/geometry/index node has a fresh derived object and that entry transform versions match owner versions. Query methods call this validation before reading the index. Snapshot loading additionally validates snapshot hash, content hash, structure hash/version and parameter version before installing any cache object.

## 4. Incremental transform update

For a batch of local matrix changes:

1. Compute topmost changed nodes plus all descendants.
2. Walk the scene topological order and rebuild world matrices only for that set. A changed parent contributes its new world matrix; an untouched ancestor reuses its cached world matrix.
3. Rebuild `GeometryEntry` objects only for geometries owned by affected nodes. Entry bounds are computed in parallel with bounded `MaxDegreeOfParallelism`.
4. Recompute own-bounds for affected nodes.
5. Recompute subtree `IndexNode` summaries for affected nodes and their ancestors, bottom-up.
6. Swap scene/cache/index dictionaries only after all steps complete.

Unrelated dictionary values are copied by reference; `IncrementalStats` counts reference reuse. No full-scene derived rebuild is performed.

## 5. Dependency graph

The explicit graph uses these stable object ids:

```text
input:transform:{nodeId}
input:geometry:{geometryId}
derived:world:{nodeId}
derived:bounds:{nodeId}
derived:entry:{geometryId}
index:node:{nodeId}
```

Important edges:

```text
input:transform -> derived:world
derived:world(parent) -> derived:world(child)
derived:world(owner) -> derived:entry
input:geometry -> derived:entry
derived:entry -> derived:bounds(owner), index:node(owner)
derived:bounds(owner) -> index:node(owner)
index:node(child) -> index:node(parent)
```

`CollectInvalidations` performs a deterministic breadth-first traversal. Every output record names the invalidated object, the edge reason, and the upstream source object. Test assertions verify that a leaf transform invalidates its own entry and ancestor summaries but not an ancestor world cache or a sibling branch.

## 6. Pick semantics

### Ray

The camera uses a pinhole model with pixel-center rays. Geometry tests happen in local space by applying the inverse world matrix to the world ray; the ray parameter remains the world-space distance because the direction is transformed without renormalization.

- AABB: slab test.
- Sphere: quadratic ray/sphere test.
- Triangle mesh: nearest Moller-Trumbore triangle.

All geometry hits are collected, sorted by distance, `geometryId`, then `nodeId`, so equal-distance ties are deterministic. A miss is a successful query with a null hit.

### Box

World bounds are projected to screen space. Near-plane crossing edges are clipped and added to the projected hull. A node is selected if any of its geometry's projected bounds intersects the selection rectangle. Result node ids are unique and ordinal-sorted.

### Frustum

Six world-space planes are built from camera near/far and side planes. AABB vs plane positive-vertex tests cull each index subtree.

## 7. Pivot operations

Selection is first reduced to topmost roots. If parent and child are both selected, only the parent is transformed; descendants move through normal hierarchy propagation exactly once.

For each root with parent world `P` and world pivot `w`:

```text
translate:   L' = L * T(inverse-linear(P) * delta)
rotate:      O_w = T(-w) * R_world * T(w)
scale:       O_w = T(-w) * S_world * T(w)
rotate/scale: L' = L * P * O_w * inverse(P)
```

This guarantees the world pivot is fixed for rotation/scale and avoids combining local and world deltas incorrectly. A session owns a working local-matrix dictionary and a captured base revision. Commit submits one transform batch with the expected revision; any intervening runtime edit causes `E_STALE`.

## 8. Atomic commit and rollback

Mutations are copy-on-write at the dictionary level:

- validate every update first;
- build candidate scene/cache/index dictionaries;
- rebuild only affected derived objects;
- swap references under the write lock only when the entire batch succeeds.

Thus an exception, invalid reference, cancellation, or stale revision leaves the pre-batch runtime untouched. Individual geometry entry rebuild errors are contained: that entry is marked `Failed`, sibling entries remain ready, and `RetryFailedEntries` can attempt only the failed objects without poisoning the rest of the index.

## 9. Snapshot format and recovery

The snapshot payload is a single JSON object containing:

- format, content hash, structure hash/version, parameter version, scene revision;
- all `NodeDerived` world matrices/bounds and versions;
- all geometry entries with local/world bounds, owner and versions;
- all hierarchy `IndexNode` summaries;
- a SHA-256 snapshot hash over the canonicalized payload.

Writes are serialized by a static semaphore and an exclusive lock file. A uniquely named temporary file is written with `WriteThrough`, flushed to disk, and atomically moved over the final path. A killed process leaves at most an ignored temp file; readers only see the old or new complete snapshot.

`LoadInto` requires an empty runtime shell. It deserializes the whole payload, verifies hash/version metadata, converts every object, then installs and validates all caches. Any failure returns `E_SNAP` and leaves the shell uninitialized. `LoadOrRebuild` wraps this with explicit full rebuild after invalid snapshot rejection.

## 10. Concurrency

- `ReaderWriterLockSlim` permits concurrent query readers; all mutations take the write lock.
- Entry rebuild and query batches use `Parallel.For` with `MaxDegreeOfParallelism`.
- Query batch size and candidate buffer have configured maxima (`MaxQueryBatchSize`, `MaxCandidateBuffer`), returning `E_LIMIT`.
- Query batches write results to pre-sized arrays by input index, preserving request order regardless of thread completion order.
- Parallel edit preparation validates edits concurrently but applies them in the original sequence, so the effective edit order is identical to the sequential path.

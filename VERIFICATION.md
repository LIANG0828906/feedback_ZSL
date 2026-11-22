# Verification Record

## Environment

- Date: 2026-11-23
- OS: Ubuntu 26.04 on WSL2 (`x86_64`)
- SDK: .NET 8.0.428
- Runtime: .NET 8.0.34
- WinForms target: `net8.0-windows` (compiled successfully; no desktop window can be displayed from this WSL session)
- External packages: none

## Command

```bash
./scripts/verify.sh
```

Exit code: `0`

## Generated sample

```text
Generated 18 nodes, 14 geometries.
Scene content hash: df5db1233a27a6389d2b7f506715231e7da6b5cd6b9731cf0392a551d5d019ad
Final edit hash:    a60a595b28653790d671a1bc0d73717191df997fbdfc04afa5d1c0e02d5709ec
```

The sample contains AABB, sphere and triangle-mesh geometry, a four-level scene hierarchy, 16 ray queries, 5 box queries and a 6-step edit sequence containing transform edits, geometry add/remove and translate/rotate/scale pivot batches.

## Build

```text
Build succeeded.
    0 Warning(s)
    0 Error(s)
```

Release outputs were produced for `GameEditor.Spatial`, `GameEditor.Tools`, `GameEditor.WinForms` and `GameEditor.Tests`.

A final post-change rerun also produced `0 Warning(s)`, `18/18 tests passed`, and:

```text
Validation: derived cache versions are consistent.
Rays: 16 (4 hits, 12 misses), truth mismatches: 0
Box selections: 5 queries, 25 total node hits
Edit truth mismatches: 0
EQUAL: 18 nodes, 14 entries, 16 rays, 5 boxes.
```

## Acceptance suite

```text
PASS generator is deterministic and matches checked-in sample (75 ms)
PASS schema, cycle, reference and finite-number validation (31 ms)
PASS ray and box truth, misses and query-order independence (17 ms)
PASS frustum query returns visible nodes (6 ms)
PASS transform propagation reuses caches and invalidates precisely (17 ms)
PASS topology change and failed batch preserve state (5 ms)
PASS parent and child pivot selection moves only once (8 ms)
PASS pivot rotation and scale keep their world pivot fixed (13 ms)
PASS pivot sessions isolate, cancel and reject stale commits (4 ms)
PASS edit sequence matches analytic truth and full rebuild (18 ms)
PASS sequential and prepare-parallel edit order agree (10 ms)
PASS divergence checker reports first object and keeps snapshots (6 ms)
PASS snapshot round-trip restores all derived data (37 ms)
PASS snapshot version/hash/truncation rejection stays empty (15 ms)
PASS snapshot writes are serial and atomic (21 ms)
PASS single entry failure does not poison other entries (7 ms)
PASS parallel queries and batch limits are deterministic (7 ms)
PASS cancellation before commit leaves runtime unchanged (6 ms)

18/18 tests passed in 0.31s.
```

## Demo results

Selected observed ray/box/update results:

```text
ray-center           center-mesh/g-center-mesh t=19.022137
ray-left             left-sphere-a/g-left-sphere-a t=20.287365
ray-right            <miss>
ray-miss-corner      <miss>
box-left             [left-crate-a, left-mesh-a, left-mesh-tip, left-sphere-a]
box-right            [right-crate-a, right-mesh-a]
box-empty-corner     []

edit-transform-center-mesh   rev=2 rebuilt=1 reused=17 invalidations=6
edit-add-left-sphere         rev=3 rebuilt=1 reused=17 invalidations=4
edit-remove-back-mesh        rev=4 rebuilt=1 reused=17 invalidations=5
edit-pivot-translate         rev=5 rebuilt=5 reused=13 invalidations=21
edit-pivot-rotate            rev=6 rebuilt=4 reused=14 invalidations=15
edit-pivot-scale             rev=7 rebuilt=4 reused=14 invalidations=16

EQUAL: 18 nodes, 14 entries, 16 rays, 5 boxes.
Snapshot loaded: revision=7 hash=26007e9b47c2...
```

The `EQUAL` line is the result of comparing every node world matrix, node bounds, geometry entry bounds, hierarchy index bounds, nearest ray hit and complete box-selection set between the incrementally edited runtime and a new runtime rebuilt from an empty cache.

## Negative-path evidence

The automated suite also asserted:

- missing field -> `E_SCHEMA` with `$.nodes[0].parentId`;
- parent cycle -> `E_CYCLE`;
- bad geometry owner / mesh index -> `E_REF`;
- reparent -> `E_TOPO` with identical revision, content hash, matrices and bounds;
- mixed valid/invalid transform batch -> `E_REF` and full rollback;
- content-hash, parameter-version, snapshot-hash and truncated-file cases -> `E_SNAP`, shell remains uninitialized;
- `LoadOrRebuild` -> discards `E_SNAP` snapshot and produces a fully rebuilt runtime;
- canceled transform/session/query -> `E_CANCELED` with no state change;
- eight concurrent saves to one path -> all complete, subsequent load is valid and no temp file remains.

## Notes

- The current host is headless WSL, so the WinForms project was compiled but not launched. The UI code is a complete runnable example; on Windows run `./scripts/run-winforms.ps1`.
- `artifacts/demo-snapshot.json` is generated during demo verification and is intentionally ignored by git.

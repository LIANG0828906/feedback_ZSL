# Validation Guide

## Automated entry point

```bash
dotnet run --project tests/GameEditor.Tests
```

The runner is a plain console application and uses no test framework package. It returns exit code `0` only when every test passes.

For a complete regenerate/build/test/demo/snapshot cycle:

```bash
./scripts/verify.sh
```

or on Windows:

```powershell
.\scripts\verify.ps1
```

## What is compared

The acceptance suite compares:

- every generated ray's nearest hit node, geometry, distance and point;
- all per-geometry hits for each ray;
- every box query's complete node-id set;
- every node world matrix and own geometry bounds against analytic edit truth after each edit;
- final ray and box truth after the entire edit sequence;
- incremental runtime vs a fresh full rebuild after every edit;
- sequential vs prepare-parallel edit application;
- snapshot-restored runtime vs the runtime that produced the snapshot;
- parallel vs sequential query batches.

The incremental/full comparator checks node world matrices, node bounds, geometry entry bounds, hierarchy summary bounds, ray hits and box sets. It reports the first object id and field name and retains both full snapshots.

## Negative cases

The suite writes controlled JSON variants to `artifacts/test-tmp` and verifies:

- missing `parentId` -> `E_SCHEMA` at `$.nodes[0].parentId`;
- root/non-root parent cycle -> `E_CYCLE`;
- missing geometry owner -> `E_REF`;
- mesh index outside vertex range -> `E_REF`;
- out-of-float-range transform -> `E_SCHEMA`;
- declaration hash mismatch -> `E_SCHEMA`;
- reparent request -> `E_TOPO` with unchanged revision/content;
- batch containing a missing node -> `E_REF` with full rollback.

Snapshot tests tamper with content hash, parameter version, derived world matrix, truncate the JSON file, and load into a shell whose scene has a different content hash. Every case returns `E_SNAP` and keeps the shell uninitialized. `LoadOrRebuild` is separately verified to discard the bad snapshot and perform a full rebuild.

## Manual WinForms check

On Windows:

```powershell
.\scripts\run-winforms.ps1
```

1. Click a visible object; the status line reports the nearest ray hit.
2. Drag a rectangle; the selection label lists all box-selected nodes.
3. Choose `Translate`, `Rotate around Y`, or `Scale`, set pivot and amount, then preview.
4. Confirm the canvas preview changes while the runtime remains unchanged until `Commit batch`.
5. Commit and observe revision, invalidation count and rebuilt-node count.
6. Add/remove a sphere and observe reused-entry counts.

The sample can be regenerated from the toolbar; output remains byte-deterministic.

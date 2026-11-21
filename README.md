# Terrain Patch Viewer

C++17 implementation of a deterministic, incrementally updated terrain patch
pipeline. It loads a custom binary height field and material-index image,
builds a block quadtree, applies transactional height/material patches, computes
a dependency-closure invalidation set, repairs LOD seams, updates normals and
material blends locally, commits versioned snapshots atomically, and renders an
offscreen preview with block-buffer reuse.

The default build has no external dependencies. The deterministic software
offscreen renderer is used for acceptance tests because it is reproducible on
headless machines. A GLFW + glad OpenGL 3.3 preview target is provided as an
optional build when local dependencies are available.

## Quick Start

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
ctest --test-dir build --output-on-failure
```

Generate local deterministic samples and run the complete patch sequence:

```bash
./build/terrain_app gen ./samples
./build/terrain_app pipeline ./samples ./samples/store
```

Inspect or render a local sample:

```bash
./build/terrain_app inspect ./samples/height.bin ./samples/material.bin
./build/terrain_app ppm ./samples /tmp/terrain.ppm
./build/terrain_app recover ./samples/store ./samples/patches
```

Manual forced-termination recovery on the sample store:

```bash
TERRAIN_COMMIT_FAULT=before_manifest TERRAIN_FORCE_KILL=1 \
  ./build/terrain_app apply ./samples ./samples/store 3
./build/terrain_app recover ./samples/store ./samples/patches
```

The first command exits with code 77 after durable snapshots but before the
manifest commit. Recovery starts from the previous manifest and applies only
the uncommitted batches.

Run the terminal demo:

```bash
./build/terrain_demo ./samples
```

Demo controls:

- `a`: asynchronously apply the next patch batch.
- `c`: cooperatively cancel the active background job.
- `+` / `-`: change block size and rebuild in the background.
- `[` / `]`: change the quadtree error threshold in the background.
- `q`: quit.

The dashboard shows elevation heat, leaf boundaries, invalidated blocks in red,
the current offscreen preview, the offscreen error heat buffer, the patch batch
list, deterministic render summary, and the first invalidation reason.

## Windows 10/11 Build

Use a Visual Studio 2022 x64 developer prompt:

```bat
cmake -S . -B build -G "Visual Studio 17 2022" -A x64
cmake --build build --config Release
ctest --test-dir build -C Release --output-on-failure
```

Set `TERRAIN_ENABLE_GLFW=ON` and point `TERRAIN_GLFW_ROOT` /
`TERRAIN_GLAD_ROOT` at local, already-downloaded source/build trees to build
`terrain_gl_preview.exe`. No network access is attempted by CMake. See
`third_party/README.md`.

## Validation

`terrain_tests` creates deterministic samples in a temporary directory and
covers:

- malformed height/material headers with first-contradiction field names;
- non-finite and out-of-range elevations, length and content-hash mismatches;
- quadtree leaf coverage, extrema, content hashes and stable IDs;
- out-of-bounds height patches (`E_PATCH_BOUNDS`) and stale baselines
  (`E_PATCH_STALE`) with byte-for-byte unchanged terrain;
- dependency closure across height, material, LOD, normal, blend, seam and
  offscreen nodes, with a recorded reason for every invalidated block;
- local LOD subtree re-decision, neighbor seam checks, and injected crack
  localization (`E_CRACK_UNRESOLVED`);
- incremental normals/material blends against full recomputation;
- full versus incremental offscreen color + heat buffers, repeated incremental
  determinism, and no uncovered block pixels;
- atomic commit failure preserving the previous manifest, real child-process
  termination during commit, hash-chain verification, and replay of only
  uncommitted batches;
- asynchronous single-job enforcement (`E_BUSY`) and cancellation preserving
  the last committed version.

The current local run reports:

```text
checks=10060 failures=0
TERRAIN ACCEPTANCE PASSED
```

The sample pipeline applies four batches to a 129x129 terrain. Each batch
reports zero normal-angle deviation from full truth, zero seam deviation,
full/incremental byte equality, repeated-frame equality, and fewer rerendered
blocks than the full leaf count. Exact counts vary with the configured block
size and error threshold; they are printed in each deterministic summary.

Current Release sample run (512x512 offscreen buffers):

| Patch | Leaves | Invalid | Reused | LOD subtrees | Max normal error | Max seam error | Full == incremental |
|---:|---:|---:|---:|---:|---:|---:|---|
| 1 | 136 | 78 | 58 | 8 | 0 deg | 0 | yes |
| 2 | 142 | 101 | 41 | 5 | 0 deg | 0 | yes |
| 3 | 142 | 27 | 115 | 0 | 0 deg | 0 | yes |
| 4 | 142 | 71 | 71 | 4 | 0 deg | 0 | yes |

The Release CTest result is `1/1 passed` in about one second on the local WSL2
environment.

## Layout

- `include/terrain/`, `src/`: reusable C++17 core.
- `tests/terrain_tests.cpp`: deterministic acceptance suite.
- `src/demo.cpp`: terminal UI with background jobs.
- `src/gl_preview.cpp`: optional OpenGL 3.3 preview target.
- `docs/FORMAT.md`: exact binary and manifest layouts.
- `docs/DESIGN.md`: invalidation, seam, rendering, commit and recovery design.
- `docs/VERIFICATION.md`: recorded build, test, patch-sequence and recovery results.
- `third_party/README.md`: offline dependency setup.

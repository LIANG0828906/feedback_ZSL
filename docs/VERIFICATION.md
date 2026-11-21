# Verification Record

Date: 2026-11-22  
Environment: Ubuntu 26.04 on WSL2, GCC 15.2, CMake 3.30.5, x86-64, 24 CPUs  
Build: `Release`, software offscreen backend (no GPU required)

## Build

```text
cmake -S . -B build-release -DCMAKE_BUILD_TYPE=Release \
  -DTERRAIN_BUILD_TESTS=ON -DTERRAIN_BUILD_DEMO=ON
cmake --build build-release -j4
```

Result: all targets built with `-Wall -Wextra -Wpedantic`; no warnings.

An additional Debug build with
`-fsanitize=address,undefined -fno-omit-frame-pointer` completed the same
`checks=10060 failures=0` suite without sanitizer diagnostics.

## Automated Acceptance

```text
./build-release/terrain_tests
checks=10060 failures=0
TERRAIN ACCEPTANCE PASSED
```

```text
ctest --test-dir build-release --output-on-failure
1/1 Test #1: terrain_acceptance ... Passed about 1 sec
100% tests passed, 0 tests failed out of 1
```

The suite covers malformed headers, first contradictory field reporting,
non-finite/out-of-range elevations, payload length and hash errors, quadtree
coverage and node hashes, bounds/stale patch rollback, dependency closure,
incremental LOD rebuilds, seam closure and injected crack location, full-truth
normal/material comparison, full/incremental byte equality, repeated-frame
determinism, atomic commit failure, real child-process termination during
commit, hash-chain recovery, `E_BUSY`, cancellation, block-size and
threshold background rebuilds, and post-cancel commit.

## Deterministic Patch Sequence

Sample terrain: 129x129 vertices, 1.0 spacing, 512x512 offscreen buffers.

| Patch | Leaves | Invalid | Reused | LOD rebuilt | Normal error | Material error | Seam error | Full == incremental |
|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 1 | 136 | 78 | 58 | 8 | 0 deg | 0 | 0 | yes |
| 2 | 142 | 101 | 41 | 5 | 0 deg | 0 | 0 | yes |
| 3 | 142 | 27 | 115 | 0 | 0 deg | 0 | 0 | yes |
| 4 | 142 | 71 | 71 | 4 | 0 deg | 0 | 0 | yes |

Every row also reported:

- `fallback_full=0`
- `repeated_incremental_equal=1`
- deterministic invalid/recompute/reuse block lists
- deterministic SHA-256 color and heat hashes

## Forced Termination and Recovery

Manual command-line run:

```text
apply patch 1: E_OK
apply patch 2: E_OK
TERRAIN_COMMIT_FAULT=before_manifest TERRAIN_FORCE_KILL=1 apply patch 3
kill_exit=77
recover:
start_version=2 end_version=4 committed=2 skipped=2
replayed patch 3: E_OK ...
replayed patch 4: E_OK ...
```

The forced process death happened after durable snapshot writes but before the
manifest commit. Restart loaded version 2, skipped committed patches 1 and 2,
and replayed only patches 3 and 4.

## UI Smoke Test

```text
TERM=xterm-256color ./build-release/terrain_demo /tmp/terrain_demo_samples --once
```

The dashboard rendered the elevation heat map, block boundaries, patch list,
offscreen preview, error heat buffer, deterministic render hash and controls.
A PPM export of the offscreen frame contained zero uncovered/background block
pixels.


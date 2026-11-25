# Verification Record

Date: 2026-11-26  
Environment: Linux amd64, Go 1.22.12, system `libsqlite3.so.0`  
Seed: `42`

## Commands

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go run ./cmd/thermalcache -seed 42 -out artifacts
```

## Test Result

```text
?   	thermalcache/cmd/thermalcache	[no test files]
ok  	thermalcache	0.022s

?   	thermalcache/cmd/thermalcache	[no test files]
ok  	thermalcache	1.135s
```

`go vet ./...` produced no findings.

The suite contains 17 top-level tests, including:

- three-schema semantic equivalence and traversal order;
- fixed-seed normalized-result golden digest;
- unknown/mixed field rejection;
- fixed-seed gate, migration, expiry, forced migration, and batch-limit checks;
- worker-count invariance for one, two, and four workers;
- score boundary and saturation checks;
- pre-commit interrupt rollback;
- partial commit plus interrupt;
- observable layer-buffer backpressure;
- capacity and object-count deferral constraint codes;
- SQLite round trip, write-order log, truncation, version mismatch;
- partial-batch checkpoint resume;
- four incremental edit types against full recomputation;
- nine injected anomalies with stable codes.

## Demo Result

```text
thermalcache seed=42
  v1: fingerprint=23170f32ca45 ticks=7 batches=8 equal=true
  v2: fingerprint=23170f32ca45 ticks=7 batches=8 equal=true
  v3: fingerprint=23170f32ca45 ticks=7 batches=8 equal=true
  checkpoint: artifacts/checkpoint.sqlite (committed_tick=600, objects=10)
  partial resume: true, no_double_count=true
  interrupt rollback: source_retained=true
  incremental object_access    full_match=true reused=2 recomputed=5
  incremental layer_capacity   full_match=true reused=3 recomputed=4
  incremental append_access_event full_match=true reused=4 recomputed=3
  wrote artifacts/demo_output.json, artifacts/verification.txt
```

## Key Observations

| Observation | Result |
| --- | --- |
| Canonical timestamp after v1 tick normalization | integer milliseconds |
| Capacity after v2/v3 KiB normalization | `12288`, `32768`, `16384` bytes as declared |
| Schema semantic fingerprint | identical for v1/v2/v3 |
| Normalized result digest | `39293d8436900a8fc2cfda02ffb4d8d1a10620f709a9d3665ca47ab7f9951495` |
| Timeline | `[0 100 200 300 400 500 600]` |
| `o05` at tick 100 | source cold, target hot, target invisible, 1 pending write |
| `o05` at tick 200 | hot, source/target version 1, target visible |
| `o10` forced migration | starts at 500, target denied before commit |
| `o10` at tick 600 | hot and target version 1 |
| TTL | `o09` reports `R_TTL_EXPIRED` at tick 400 |
| Deferred candidate | `o07` reports `C_BATCH_BYTES` in batch 3 |
| Concurrency invariance | worker budgets 1/2/4 identical |
| Interrupt before commit | source layer and occupied bytes unchanged |
| Partial checkpoint | one object target, remaining object source, resumes cleanly |
| Incremental invalidation | all results equal full recompute |
| Capacity accounting | every object counted once across hot+cold |

The machine-readable details are in:

- `artifacts/demo_output.json`
- `artifacts/verification.txt`
- `artifacts/generated/v1.json`
- `artifacts/generated/v2.json`
- `artifacts/generated/v3.json`

Deterministic artifact SHA-256:

```text
f059e4e6f7320e35c4cc386d2546c571d56003037d7db52070d99f345e70f63d  artifacts/demo_output.json
c3f3d91a825bfea04af5b4ce62cbc63070f61ffca2b79150865e22e1a92bf91e  artifacts/verification.txt
88a3dae187a4d4bcb4651638c8df8068338b046f52289aba522b95bd4b4820b1  artifacts/generated/v1.json
33441a565eee5c52bc68e3eaf5ddd16ae91e6b90767c5b04dedc28ab5da46f83  artifacts/generated/v2.json
a29a81bf7389d9a31208bf07e0849a1ae584e4b3fca63cc70e285fe33ec558d6  artifacts/generated/v3.json
```

# Acceptance Record

Run date: 2026-11-26  
Environment: WSL2 Ubuntu, Linux 6.18, Go 1.22.12, pure-Go SQLite driver  
Mode: local deterministic samples only; no Docker, database service, account or
external target.

## Automated Commands

```text
$ go test -mod=vendor ./... -count=1
ok   qosshape.local/gateway/qos

$ CGO_ENABLED=0 go test -mod=vendor ./... -count=1
ok   qosshape.local/gateway/qos

$ go test -race -mod=vendor ./qos -count=1
ok   qosshape.local/gateway/qos

$ go vet ./...
(no findings)
```

## Deterministic Sample

```text
seed=42
project_version=gen-42-r1
project_digest=58d10311c9e6b320f5501ef33e1e206e1ef91b753f4bc3a5c4cc73def66d5966
steps=34
emitted=15
delayed_events=32
dropped=14
expired=3
final_tokens={gold:220, silver:30, bronze:266}
```

v1/v2/v3 all loaded with channel order `[gold silver bronze]`, the same digest,
the same per-step decisions and the same emitted sequence. Each generated truth
file decoded back to the engine result:

```text
load v1 channels=[gold silver bronze] digest=58d10311c9e6b320 truth=OK
load v2 channels=[gold silver bronze] digest=58d10311c9e6b320 truth=OK
load v3 channels=[gold silver bronze] digest=58d10311c9e6b320 truth=OK
cross-version normalized channels, emissions, drops and token balances: OK
```

## Fixed-Point Critical Cases

All cases in `artifacts/demo/critical_cases.json` have
`within_tolerance=true` and `pass=true`, including:

- per-mille `2500` -> per-myriad `25000`
- per-myriad `25000` identity
- 8 packets * 128 bytes -> 1024 bytes
- refill 100 + 30 = 130 bytes, deficit 86, exact delay 29 ms quantized to 40 ms
- quantization error 11 ms <= 19 ms tolerance
- 32-bit declared-width overflow detected as `E_RATE_OVERFLOW`
- 10000 one-millisecond refills at rate 25000 produce exactly 25000 bytes,
  remainder 0

The long-run engine test advances 10000 ticks under all three schemas and gets
the same emission and final token balance.

## Incremental Edit

Demo edit: gold rate `60000 -> 90000` per-myriad effective at 40 ms.

```text
start_step=4
reused_steps=4
recomputed_steps=30
cached_until_step=3
root_channels=[gold]
closure_channels=[gold silver bronze]
prefix_cache_preserved=true
```

The shared scheduler makes every channel part of the propagation closure after
the edited step. All prefix CacheTokens and cache payloads remain unchanged.

## Checkpoint Resume

Normal checkpoint demonstration:

```text
checkpoint committed next_step=5 next_ms=50 queue=3 channels=3
checkpoint resume status=resume:step=5 final_equal=true
```

Actual hard-kill demonstration:

```text
$ go build -mod=vendor -o /tmp/qosdemo ./cmd/qosdemo
$ /tmp/qosdemo run \
    --schema 1 --seed 42 --out artifacts/demo \
    --checkpoint artifacts/demo/run.db --crash-after 5
simulating hard kill after committed step=4 next_step=5
Killed
crash_exit=137

$ /tmp/qosdemo resume \
    --schema 1 --seed 42 --out artifacts/demo \
    --checkpoint artifacts/demo/run.db
run complete committed_steps=29 next_step=34 unresolved=0
resume_exit=0
```

The automated `TestCheckpointSIGKILLThenResume` repeats this with a subprocess
receiving `SIGKILL`, then loads the checkpoint and compares final emissions,
drops and token balances with a full run.

## Anomaly Injection

All source-stability checks passed:

```text
negative_rate                  E_RATE_NEGATIVE
bucket_smaller_than_packet     E_BUCKET_TOO_SMALL
field_conflict                 E_FIELD_CONFLICT
thread_range                   E_THREAD_RANGE
queue_budget_range             E_QUEUE_BUDGET
expiry_order_reversed          E_EXPIRY_ORDER
checkpoint_version_mismatch    E_CHECKPOINT_VERSION
truncated_checkpoint           E_CHECKPOINT_TRUNCATED
```

Each case includes a source-selected `path` and structured context. The input
byte slices equal their pre-injection copies. Checkpoint version mismatch causes
`ResumeEngine` to return a fresh engine at step 0; truncated files are rejected
before state reconstruction.

## Evidence Files

- `artifacts/demo/demo_stdout.txt`: full CLI demonstration
- `artifacts/demo/demo_report.json`: machine-readable demonstration summary
- `artifacts/demo/manifest.json`: sample digest and result counts
- `artifacts/demo/critical_cases.json`: fixed-point critical calculations
- `artifacts/demo/sample_v{1,2,3}.json`: three schema encodings
- `artifacts/demo/truth_v{1,2,3}.json`: per-time expected truth

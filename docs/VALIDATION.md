# Validation Record

日期：2026-11-24  
环境：WSL2 Ubuntu 26.04，Go 1.22.12 linux/amd64，CGO enabled，gcc 15.2.0  
数据：固定种子 `20261124` 生成的 `samples/workbench_samples.json`  

## Automated tests

### Core

```bash
go test ./...
```

Result:

```text
ok  spatialworkbench/internal/acoustics
ok  spatialworkbench/internal/audio
ok  spatialworkbench/internal/checkpoint
ok  spatialworkbench/internal/generator
ok  spatialworkbench/internal/loader
ok  spatialworkbench/internal/session
ok  spatialworkbench/internal/verify
```

### Race detector

```bash
go test -race ./...
```

Result: all packages passed with no race report.

### Fyne-tagged build and headless panel test

The current host is headless and does not ship X11 development headers system-wide. A local sysroot was assembled under `/tmp/spatial-sysroot` from X11/OpenGL development packages, then:

```bash
go test -tags fyne ./...
```

Result:

```text
ok  spatialworkbench/internal/desktop
ok  spatialworkbench/internal/acoustics
ok  spatialworkbench/internal/audio
ok  spatialworkbench/internal/checkpoint
ok  spatialworkbench/internal/generator
ok  spatialworkbench/internal/loader
ok  spatialworkbench/internal/session
ok  spatialworkbench/internal/verify
```

`TestHeadlessPanelBuildAndStep` constructs the Fyne test application, builds the full diagnostics panel, loads/generates a sample, commits one frame, refreshes the table/plots, and passes.

The Windows-only MCI player is isolated in `player_windows.go` and is not compiled on Linux.

## Sample generation

```bash
go run ./cmd/workbench generate -out samples/workbench_samples.json -seed 20261124 -samples 3
```

```text
generated 3 deterministic sample(s) at samples/workbench_samples.json (seed=20261124)
```

First sample:

```text
sample: local-arena-5.1-20365853
rate: 48000 Hz
layout: arena-5.1 (6 channels)
irSamples: 4096
drySamples: 4096
  FL   direct=64  itd=0
  FR   direct=80  itd=16
  C    direct=104 itd=40
  LFE  direct=136 itd=72
  SL   direct=160 itd=96
  SR   direct=184 itd=120
```

## Three-path equality

```bash
go run ./cmd/workbench verify \
  -sample samples/workbench_samples.json \
  -index 0 \
  -resume-at 7
```

```text
sample=local-arena-5.1-20365853 frames=16 resumeAt=7
PASS realtime == resume
PASS realtime == persisted-recompute
PASS resume == persisted-recompute
three paths are sample-for-sample identical
```

This compares 16 frames, 6 output channels, all delay/energy/ITD/DRR values, four current/cumulative error bounds, commit sequence, and 24,576 rendered float64 samples (4096 samples × 6 channels).

The same verification was run for the other two generated layouts:

```text
sample=local-studio-quad-20470582 frames=14 resumeAt=5
PASS realtime == resume
PASS realtime == persisted-recompute
PASS resume == persisted-recompute

sample=local-stage-stereo-20575311 frames=20 resumeAt=11
PASS realtime == resume
PASS realtime == persisted-recompute
PASS resume == persisted-recompute
```

## Exit and restart

First process run:

```bash
go run ./cmd/workbench diagnostics \
  -sample samples/workbench_samples.json -index 0 \
  -db "$DB/state.sqlite" -frames 4
```

Second process run against the same database:

```text
resume loaded=4 invalidated=false reason=
frames 0..3: committed
frames 4..15: pending
```

The second run continued to frame 7 without recomputing or changing the first four committed frames.

## Checkpoint invalidation

Automated tests cover:

- parameter-hash mismatch → all rows deleted, `Invalidated=true`;
- input-content-hash mismatch → all rows deleted;
- algorithm-version mismatch (direct SQL tamper) → all rows deleted;
- non-contiguous/out-of-order commit → `E_STATE`;
- concurrent commits of the same frame → exactly one succeeds;
- render BLOB float64 round-trip → bit-exact.

## Anomaly chain

Automated tests cover:

| Injection | Result |
|---|---|
| missing channel | `E_CHAN`, channel ID |
| reversed arrival | `E_ORDER`, frame and channel |
| gain overflow | `E_CLIP`, frame and channel |
| bad frame | `E_FRAME`, frame enters `failed` |

After a bad frame is injected at frame 1:

```text
frame 0 committed
frame 1 failed
frames 2+ pending
```

Clearing the injection and calling retry succeeds; frame 0's committed result is byte-for-byte unchanged.

## Buffer budget

`TestWorkerPoolBudgetBlocksAndLogs` holds the weighted budget full and verifies a waiting task records:

```text
sample-buffer-exhausted: task queued until an active worker releases its buffer
```

It also verifies a per-worker overflow request returns `E_FRAME`.

## Render export

```bash
go run ./cmd/workbench render \
  -sample samples/workbench_samples.json \
  -index 0 \
  -out artifacts/render.wav
```

```text
rendered local-arena-5.1-20365853: 6 channels x 4096 samples at 48000 Hz -> artifacts/render.wav
```

`artifacts/render.wav` size: `98,348` bytes. Expected float32 data:

```text
44-byte header + 4096 samples * 6 channels * 4 bytes = 98,348 bytes
```

This confirms the output contains exactly the dry input duration and no extra boundary samples.

Quad export:

```text
rendered local-studio-quad-20470582: 4 channels x 3400 samples at 44100 Hz -> artifacts/render_quad.wav
```

Size: `54,444` bytes = 44 + 3400 × 4 × 4.

## Windows-specific player type check

`player_windows.go` plus `player.go` were copied to a temporary standalone module and checked with:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go test ./...
```

Result: passed. This validates the `winmm.dll` MCI syscall declarations independently of the Linux Fyne build.

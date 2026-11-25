# Skeleton Retarget

Deterministic, pure-Go implementation of the bone-animation/retarget acceptance
chain. The first release is intentionally self-contained: all bindings, motion,
target proportions and truth samples are generated from a fixed seed by code in
this repository. There are no external data files, services, GPUs or containers.

## Quick Start

Requirements: Go 1.22+ on a Linux filesystem. SQLite is
`modernc.org/sqlite v1.34.5` (pure Go, no CGO).

```sh
go build ./...
go vet ./...
go test ./...
go run ./cmd/skeletonretarget verify
go run ./cmd/skeletonretarget demo
go run ./cmd/skeletonretarget demo --json
go run ./cmd/skeletonretarget serve --addr 127.0.0.1:0
```

`serve` refuses non-loopback addresses and prints the selected ephemeral port.
It exposes `/` for the diagnostic page and `/api/report` for the same data as
JSON. The page shows the source hierarchy, target proportions, mapping table,
per-frame target rotations/positions, residual and derived-bound curves, the
edited interval invalidation list, cache hits, and load/incremental/recovery
comparisons.

## Architecture

| Package | Responsibility |
| --- | --- |
| `internal/model` | Unified document, skeleton, clip, mapping, truth and error model |
| `internal/fixed` | Integer fixed-point quaternion/vector math, rounding and time conversion |
| `internal/topology` | Flat/nested topology normalization, cycle and missing-parent checks |
| `internal/generator` | Seeded analytic sample + three release-format encoders + truth |
| `internal/codec` | Studio v3, Engine v2 and Legacy v1 loaders |
| `internal/retarget` | World-orientation accumulation, remapping, translation, precision report and bounded worker pool |
| `internal/state` | Five-state per-interval state machine |
| `internal/checkpoint` | Atomic SQLite interval checkpoints, validation and prefix recovery |
| `internal/pipeline` | Dependency-aware interval cache, invalidation, incremental/full/recovery runs |
| `internal/demo` | One deterministic demo used by CLI, page and tests |
| `internal/webui` | Loopback diagnostic HTTP server |

## Release Formats

All three formats carry the same semantic action and are normalized into
`model.Document`. A cross-format semantic action hash is used for cache identity.

| Format | Version | Hierarchy | Quaternion order | Fixed scales | Time | Constraint angles |
| --- | --- | --- | --- | --- | --- | --- |
| Studio | `studio-3` | Nested tree | `xyzw` | rotation/translation `1e5` | frame sequence | degrees |
| Engine | `engine-2` | Flat bone table | `wxyz` | `1e6` | milliseconds | radians |
| Legacy | `legacy-1` | Flat table with per-offset scale | `xyzw` | `1e7` | frame sequence | radians |

The loader records time and degree/radian conversions, including remainder,
divisor and rounding direction. Required fields and unknown versions return
`E_SCHEMA` with a JSON path and source version. Malformed JSON returns
`E_DECODE`; an out-of-range fixed component returns `E_COMP`; non-increasing
frame/time sequences return `E_ORDER`; topology faults return `E_TOPO`; mapping
faults return `E_MAP`.

## Fixed-Point Rules

The retarget chain does not accumulate floating-point state. Input quaternions
are rescaled to the configured working scale, normalized with integer square
roots and rounded division, and every component division records its remainder.
Multiplication is performed in fixed point and normalized after each parent
link. Scale, rotation ratio and vector proportion operations all use the same
rounding primitives.

`fixed.QuantizeRoundTrip` models high -> low -> high release conversion. The
test suite checks component drift and norm drift against the declared tolerance
(20 ppm / 10 ppm in the generated sample). Normalization residuals and
per-component truncation positions are retained in every `FrameResult` under
`normalizations`.

## Retarget Semantics

1. Source local rotations are normalized and clamped to their joint constraints.
2. Source world rotations are accumulated parent-first with normalized fixed
   quaternion multiplication.
3. A mapping copies the mapped source world orientation to the target, then
   applies the mapping's rotation scale to the quaternion vector part.
4. Target local rotations are derived from the target parent world orientation.
5. Root displacement is scaled by mapping translation and target proportion;
   child positions use the target rest offset rotated by the target parent's
   world orientation.
6. Output includes target world and local rotations plus world displacement.
   Truth comparison uses target world orientation and world position, and only
   on generator-provided analytic samples.

The default working scale is `1e6`. Generated truth is evaluated independently
inside the generator at `1e9`; it does not consume retarget output.

## Precision Bound

Each target joint report records:

- normalization residual;
- parent-chain fixed multiplication truncation;
- ratio/scale rounding, including mapping coefficient rounding;
- remap orientation term;
- a conservative cumulative upper bound.

For chain depth `d`, working scale `S`, observed normalization residual `n`, and
observed multiplication truncation `m`, the implementation estimates:

```text
multiply = d * ceil(8_000_000 / S)
remap    = d * ceil(4_000_000 / S)
scale    = ratio_rounding + ceil(4_000_000 / S)
bound    = n + multiply + m + scale + remap + 32 ppm
```

No truth value is used to compute this bound. If a truth-backed observation
exceeds the bound or the document's declared rotation tolerance, `E_BOUND`
names the joint and frame.

## Incremental Cache And Invalidation

Frames are grouped into stable intervals (default size 4). Each interval record
stores:

- interval number, frame range and state;
- target rotations/positions, residuals and interval summary;
- dependency list and dependency hash;
- cache key = SHA-256(action hash, interval ID, topology version, parameter
  version, dependency hash, upstream cache key);
- content hash, parameter version and algorithm version.

The dependency set includes action, topology, mapping, precision, upstream
interval and every constraint actually active in that interval. Tightening or
loosening a constraint therefore changes keys only where a frame is actually
outside the new limit. All downstream intervals are invalidated through their
upstream key dependency. Unaffected prefix intervals are reused from SQLite.

Edits can be applied with `pipeline.ApplyEdits`; the corresponding `Edit` value
produces the invalidation report. Mapping, topology and precision changes
invalidate all intervals because every interval contains target poses affected
by those dependencies.

The tests assert byte-for-byte target rotation/position equality between:

- full load and incremental recomputation;
- full load and restart recovery;
- three release formats;
- edited full recomputation and edited incremental recomputation.

## State Machine And Checkpoints

Interval states are `pending_load`, `pending_retarget`, `retargeting`,
`committed`, and `failed`. The legal graph is:

```text
pending_load -> pending_retarget -> retargeting -> committed
                                      |              |
                                      +-> failed <---+ (via invalidation/retry)
failed -> pending_retarget
committed -> pending_retarget
pending_load -> failed
```

An illegal transition returns `E_STATE` and leaves state/progress unchanged.
Only `committed` records are written. Each commit is one SQLite transaction with
payload, summary, residuals, dependencies, hashes and versions. A process-wide
write mutex plus a monotonic interval check keeps commits serial and ordered.

On restart, the snapshot action/algorithm and each interval payload hash,
versions, dependencies and committed state are checked. The first invalid
interval and its suffix are deleted atomically; the consistent prefix is
returned. Forced termination is simulated with `StopAfter`, then recovery is
verified to load only committed intervals and complete the remainder.

## Concurrency And Failure Isolation

Target joints are dispatched by target-topology level across all requested
frames. A bounded worker pool enforces maximum parallelism, queue budget and
per-worker joint-state buffer limit. When the queue budget is exhausted,
submission blocks and records a wait event containing the job label, reason
`frame_queue_budget_exhausted` and elapsed nanoseconds. Frames are never
silently dropped.

`FailFrames` injects a single-frame internal error. The failed frame is reported
as `E_FRAME`; other frames remain available and a retry succeeds. The pipeline
does not commit a failed interval and does not disturb already committed
intervals.

The loader tests exercise `E_DECODE`, `E_COMP`, `E_ORDER`, `E_TOPO`, and
`E_MAP`; retarget tests exercise `E_NORM` and `E_BOUND`; state/cache tests
exercise `E_STATE`.

## Error Codes

| Code | Meaning |
| --- | --- |
| `E_SCHEMA` | Unknown version or missing/invalid required field |
| `E_DECODE` | Malformed binding JSON/payload |
| `E_COMP` | Fixed rotation component outside its declared scale |
| `E_ORDER` | Frame/time sequence is not strictly increasing |
| `E_NORM` | Normalization residual exceeds declared tolerance |
| `E_TOPO` | Missing parent, duplicate joint or hierarchy cycle |
| `E_MAP` | Mapping references a missing source/target or invalid mapping |
| `E_BOUND` | Truth-backed deviation exceeds derived bound/declared tolerance |
| `E_STATE` | Illegal interval transition or checkpoint ordering |
| `E_FRAME` | Single-frame internal failure, isolated and retryable |

## Verified Results

Environment used:

```text
go1.22.12 linux/amd64
module /home/liang/feedback/uid42a8691e-default-316/go.mod
GOCACHE on the Linux filesystem
modernc.org/sqlite v1.34.5
```

The following commands pass:

```sh
go build ./...
go vet ./...
go test ./...
go test -race ./internal/retarget ./internal/pipeline ./internal/checkpoint
go run ./cmd/skeletonretarget verify
```

`verify` output for seed 42:

```text
format=studio version=studio-3 action=38ee1d1a605c frames=24 conversions=36
format=engine version=engine-2 action=38ee1d1a605c frames=24 conversions=24
format=legacy version=legacy-1 action=38ee1d1a605c frames=24 conversions=24
path=load complete=true frames=24 hits=0 recomputed=6 recovered=0 digest=d5f2a838dce2
path=incremental complete=true frames=24 hits=6 recomputed=0 recovered=6 digest=d5f2a838dce2
path=recovery complete=true frames=24 hits=6 recomputed=0 recovered=6 digest=d5f2a838dce2
compare=load/incremental equal=true
compare=load/recovery equal=true
compare=studio/engine equal=true
compare=studio/legacy equal=true
edit invalidations=2 cacheHits=4 recomputed=2 fullEqualsIncremental=true
verification: PASS
```

Precision sweep at working scales `1e5`, `5e5`, `1e6`, `1e7`:

```text
scale=100000   actual=20ppm bound=1122ppm
scale=500000   actual=4ppm  bound=252ppm
scale=1000000  actual=2ppm  bound=141ppm
scale=10000000 actual=0ppm  bound=58ppm
```

At the default scale the three formats each achieve a maximum observed
rotation deviation of `2 ppm` and translation deviation of `5 ppm` against
generator truth. Tightening precision never increases either actual deviation
or the derived bound in this sweep.

# STRATA — Point Cloud Reconstruction Studio

STRATA is a local-first desktop point-cloud and surface-reconstruction application for game and interactive-media workflows. It ships with a deterministic sample generator, three source encodings, an incremental chunked reconstruction pipeline, a preview/exact dual rendering path, SQLite checkpoints, format round-tripping, and a Wails v2 desktop shell.

The browser entrypoint and the Windows desktop shell share one Go service and one frontend. No Docker, external account, or network service is required at runtime.

![STRATA exact reconstruction](artifacts/strata-ui.png)

## Quick Start

### Browser review (works in this WSL workspace)

```bash
go test ./...
go run ./cmd/strata generate -seed 195 -out data/samples
go run ./cmd/strata validate -dir data/samples -json artifacts/validation.json
go run -tags web . -addr 127.0.0.1:4173
```

Open `http://127.0.0.1:4173`.

### Windows desktop prerequisites

```powershell
go version                       # require Go >= 1.22
go env -w GOPATH=C:\Users\<you>\go
go env -w GOMODCACHE=C:\Users\<you>\go\pkg\mod
node --version                   # Node.js 18+ for frontend development
npm --version
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.2
wails doctor
```

Install the Microsoft WebView2 Evergreen Runtime if it is not already present. SQLite uses `modernc.org/sqlite` (pure Go), so no C toolchain is required.

The checked-in `frontend/dist` is already built and the Wails config skips npm install/build. To build the native desktop binary:

```powershell
wails build -s -skipbindings
.\build\bin\strata.exe
```

The default build selects `main_desktop.go`. The `web` build tag selects `main.go`, which serves the same UI locally for browser review.

## Deterministic Samples

`go run ./cmd/strata generate -seed 195 -out data/samples` writes:

| Version | File | Mapping |
|---|---|---|
| `ply-ascii/v1` | `scene-ascii-v1.ply` | `x y z nx ny nz red green blue intensity`, text float64 |
| `ply-binary/v1` | `scene-binary-v1.ply` | `z x y red green blue intensity`, float32 positions, no normals |
| `pcbin/v1` | `scene-compact-v1.pcbin` | `x y z intensity`, bounding-box u16 positions, no RGB/normals |

`truth.json` records the point count, source and clean bounds, extrema/features, expected face count, and per-chunk expectations. The seed-195 sample intentionally contains 24 outliers: 1,837 source points become 1,813 pipeline points.

## Pipeline

1. **Load and map** — PLY ASCII, PLY binary little-endian, and PCZ1 compact binary are mapped into one canonical point model. Source version, field order, offsets, optional attributes, quantization, and header metadata remain queryable.
2. **Denoise** — statistical k-nearest-neighbour filtering (`outlierSigma`, `normalK`).
3. **Canonicalize** — positions snap to a cross-format internal grid so ASCII, float32, and bbox-u16 inputs produce a vertex-identical reconstruction.
4. **Uniform resample** — each 1.2 × 1.2 spatial chunk becomes a 13 × 13 grid at 0.1 spacing.
5. **Local surface fit** — each grid face is triangulated; normals come from central differences. The 12 chunks produce 2,028 vertices and 3,456 triangles.
6. **Incremental invalidation** — a point edit rebuilds its chunk plus Chebyshev-neighbour chunks. In the seed-195 C00 case: 4 rebuilt, 8 reused, and SQLite cache hits for unchanged chunk content.

All stage parameters validate independently and return `E_PARA` when outside their declared ranges. Chunk work is bounded by `maxWorkers` and `vertexBufferLimit`.

## Preview / Exact Consistency

- Dragging renders a local flat-normal preview immediately; the UI also samples a backend preview at a throttled rate.
- The backend preview uses the same camera, clipping plane, chunk bounds, and topology as the exact frame, with a reduced shading sample rate (`0.35` by default).
- Stopping interaction triggers a full exact reconstruction and render.
- The automatic comparison measures:
  - silhouette coverage deviation (`<= 2%`),
  - projected bounding-box delta (`<= 1%`),
  - face-normal histogram L1 (`<= 18%`).
- Failure returns `E_DIVERGE` with all three measured deviations plus the first mismatching pixel and its chunk ID.

## Versions and Persistence

- Every point edit or parameter change creates a new immutable snapshot.
- Preview always reads the latest snapshot.
- An exact task captures its snapshot before work begins, so concurrent edits can only affect newer versions.
- Committed exact chunks are frozen; SQLite uniqueness and the service state machine return `E_STATE` on an attempted mutation of stale/frozen state.
- Every committed chunk is serialized through one transaction containing point indices, chunk-local vertices/faces, content key/hash, parameter version, algorithm version, reason, and sequence number.
- Pause/exit/restart resumes from committed rows. The verification suite compares full, incremental, restarted-resume, and preview paths by mesh hash and checkpoint sequence; a mismatch returns `E_DIFF` with the first differing chunk.

## Export and Error Codes

Clouds and meshes can be exported to any of the three target versions. Each export is immediately loaded back and checked for point/face counts. Attribute loss produces structured `E_LOSSY` warnings and downgrade traces, for example:

- normals omitted by `ply-binary/v1`,
- normals/RGB omitted by `pcbin/v1`,
- float64 → float32,
- float64 → bbox-u16.

| Code | Meaning |
|---|---|
| `E_VER` | unknown format/version, missing required field, invalid record width |
| `E_DECODE` | corrupt/truncated input or checksum failure |
| `E_PARA` | stage/camera/clip/export parameter out of range |
| `E_STATE` | stale snapshot, frozen chunk mutation, invalid task state |
| `E_DIVERGE` | preview/exact metric exceeds tolerance |
| `E_DIFF` | path mesh or checkpoint sequence mismatch |
| `E_LOSSY` | successful export with downgrade warnings |

## UI Actions

The right rail exposes pipeline parameters, point edits, and the chunk ledger. The bottom bar exposes:

- `PREVIEW` — low-cost flat-normal frame.
- `FINALIZE EXACT` — full reconstruction, SQLite commit, and consistency comparison.
- `RUN INCREMENTAL` — edit plus neighbor-aware partial rebuild.
- `PAUSE` / `RESUME` — SQLite checkpoint continuation.
- `VERIFY 4 PATHS` — full vs incremental vs restart-resume vs preview.

Orbit with drag, zoom with the wheel, pan with Shift-drag. The section-plane control clips along X/Y/Z. Source cards switch among the three encodings; `OPEN LOCAL POINT CLOUD` accepts `.ply`/`.pcbin` in browser mode.

## Verification

Run:

```bash
go test ./...
go test -race ./...
go test -tags web .
go run ./cmd/strata validate -dir data/samples -json artifacts/validation.json
```

The latest local validation report is [artifacts/validation.json](artifacts/validation.json). It contains measured preview/exact latency, comparison metrics, incremental reuse counts, all four path hashes/sequences, export readback results, and error-code checks.

The native Wails link step requires platform WebView development libraries. In this Linux/WSL workspace the core, HTTP UI, and default desktop-package compile checks pass; the actual Windows `.exe` link/build is performed from a Windows Go toolchain as documented above.

## Project Layout

```text
cmd/strata/              generator + validation CLI
frontend/dist/           checked-in interactive UI
internal/formats/        PLY ASCII/binary and PCZ1 codecs
internal/generator/      deterministic terrain/outlier generator
internal/pipeline/       denoise, resample, chunked mesh, invalidation
internal/render/         shared camera, clipping, raster, comparison
internal/service/        snapshots, jobs, exports, path verification
internal/store/          atomic SQLite checkpoints and chunk cache
internal/web/            local HTTP transport
main.go                  browser entrypoint
main_desktop.go          Wails v2 desktop entrypoint
wails.json               desktop build configuration
```

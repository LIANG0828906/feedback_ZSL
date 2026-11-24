# Virtual Stage Composite Pipeline

A deterministic desktop data pipeline and verification tool for virtual-production
foreground compositing. The project loads three different foreground-composite
format versions, maps them to one internal model, composites frame windows in
linear light, persists SQLite checkpoints, recomputes only invalidated windows,
and exports back to any supported source version.

The desktop UI and the CLI use the same `app.Service` and the same internal model
snapshot. The frontend is embedded and needs no Node.js/npm install.

## Quick Start

Requirements verified on the delivery host:

```text
Go 1.22.12
SQLite 3.50.2
Wails CLI 2.10.2
```

```bash
# 1. Deterministic inputs: v1/v2/v3 layers, instructions and truth
go run ./cmd/vspipeline sample --out work --seed 42

# 2. Load all versions and compare sampled frames/timestamps/visibility to truth
go run ./cmd/vspipeline validate --work work

# 3. Full composite through the SQLite checkpoint path
go run ./cmd/vspipeline full --work work

# 4. Change one layer; only its intersecting windows are recomputed
go run ./cmd/vspipeline incremental --work work \
  --kind layer_parameter --layer prop --opacity 0.61

# 5. Compare incremental output to an empty-cache full run, pixel by pixel
go run ./cmd/vspipeline verify-incremental --work work \
  --layer prop --opacity 0.57

# 6. Export and verify target-format round trips and E_LOSSY warnings
go run ./cmd/vspipeline export --work work --version 1
go run ./cmd/vspipeline export --work work --version 2
go run ./cmd/vspipeline export --work work --version 3

# 7. Restart: restore only version/hash-compatible committed windows
go run ./cmd/vspipeline resume --work work

# 8. Interactive local UI (same embedded frontend as the desktop shell)
go run ./cmd/vspipeline serve --work work --addr 127.0.0.1:8787
```

The complete acceptance sequence is also available as:

```bash
./scripts/acceptance.sh work
```

## Desktop App

The formal Wails project is in `desktop/` and binds the same service methods used
by the browser UI.

```bash
# Required on Debian/Ubuntu for a production Wails binary:
sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev

cd desktop
wails build -tags webkit2_41
./build/bin/virtual-stage
```

The Go module and desktop wrapper compile locally. The production Wails link step
could not be completed on this host because `pkg-config`, `libgtk-3-dev` and
`libwebkit2gtk-4.1-dev` are absent and package download is blocked by the host
proxy. This is an OS dependency limitation, not an application compile error.
The embedded UI was verified through `vspipeline serve`; see
`docs/validation.md`.

## Deliverables

- `pipeline/`: version loaders, canonical model, compositor, state machine,
  incremental engine, deterministic export, SQLite checkpoints, comparison tools.
- `sample/`: fixed-seed generator for all local inputs and frame/window truth.
- `app/`: shared application service used by both UI frontends.
- `webui/`: embedded HTML/CSS/JS dashboard.
- `cmd/vspipeline/`: CLI and local interactive server.
- `desktop/`: Wails v2 desktop shell.
- `pipeline/*_test.go`, `app/service_test.go`: automated acceptance tests.
- `docs/`: format contracts, architecture and actual validation results.

## Design Highlights

- Internal frames are straight-alpha linear-light float32 RGBA. Source sRGB is
  decoded to linear light; premultiplied layers are deterministically
  unpremultiplied before source-over composition.
- A window is an atomic zlib-compressed cache packet containing every frame in
  that fixed frame range. A packet is synced and renamed before its SQLite row is
  committed.
- A single SQLite connection plus a writer mutex serializes checkpoint writes.
  Queries and recovery use stable window-index order.
- Any content hash, source-version fingerprint or algorithm-version mismatch
  purges the whole checkpoint set; partial old/new result mixing is forbidden.
- Layer edits invalidate only windows intersecting the old/new layer lifecycle.
  Time-base edits invalidate all windows and downstream export state.
- Export is deterministic JSON plus deterministic PNG frames. Low-version
  hierarchy flattening and alpha normalization are recorded as `E_LOSSY` traces.

Detailed contracts:

- [Architecture](docs/architecture.md)
- [Format mapping](docs/formats.md)
- [Validation results](docs/validation.md)

# Validation Record

Validation host:

```text
Go        go1.22.12 linux/amd64
SQLite    3.50.2 2025-06-28
Wails CLI v2.10.2
Node      not installed (not required; embedded vanilla frontend)
```

## Automated Tests

```text
$ go test ./...
ok virtualstage/app
ok virtualstage/pipeline
?  virtualstage/cmd/vspipeline [no test files]
?  virtualstage/sample [no test files]
?  virtualstage/webui [no test files]

$ (cd desktop && go test ./...)
?  virtualstage/desktop [no test files]

$ go test -race ./pipeline ./app
ok virtualstage/pipeline
ok virtualstage/app
```

The tests cover:

- loading each source version and mapping source-version provenance,
- `E_VER`, `E_SCHEMA` path/field, `E_DECODE`, `E_LAYER` missing/cycle,
- sRGB linearization, premultiplied/straight equivalence and deterministic hashes,
- legal/illegal window transitions and snapshot locking,
- incremental invalidation, cache reuse and pixel-exact full comparison,
- v1/v2/v3 export round trips, byte determinism and hierarchy loss warnings,
- checkpoint restore, corrupt packet purge and algorithm-version purge.

## Sample And Truth

Seed: `42`

```text
Source versions:       [1, 2, 3]
Layer source versions: [1, 2, 3]
Layer order:           background, prop, character
Frames:                24
Windows:               4 × 6 frames
Truth frames checked:  24
Truth samples checked: 96
Max absolute error:    0
```

## Full Composite

```text
committed windows: 4 / 4
elapsed:           151 ms (delivery host, one observed run)
checkpoint rows:   windows 0..3
commit sequence:   1,2,3,4
```

## Incremental Edit

Edit: `prop.opacity = 0.61`

```text
Affected windows: [0,1,2]
Invalid layer:    prop
Reason:           layer parameter changed
Invalid stage:    composite+export
Reused windows:   [3]
```

Automatic comparison against an empty-cache full run:

```json
{
  "match": true,
  "frames_compared": 24,
  "first_mismatch": {
    "found": false,
    "window": 0,
    "x": 0,
    "y": 0
  },
  "reused": [3],
  "affected": [0, 1, 2]
}
```

## Export Round Trip

| Target | Stack equal | Visibility equal | Deterministic bytes | E_LOSSY warnings |
|---|---:|---:|---:|---|
| v1 | yes | yes | yes | alpha normalization (+ hierarchy if present) |
| v2 | yes | yes | yes | 1 alpha normalization |
| v3 | yes | yes | yes | 0 |

Each export contains:

- `layers-vN.json`,
- 24 deterministic 16-bit PNG frames,
- `export-report.json` with SHA-256, warnings and degradation traces.

Repeated `ExportModel` calls for the same internal model produce identical bytes
and content hash.

## Restart And Corruption

Normal restart:

```text
restored windows: [0,1,2,3]
purged: false
committed: 4 / 4
```

After appending bytes to `cache/frames/window-00002.vspw`:

```text
restored windows: []
purged: true
reason: window 2 persisted file hash mismatch
committed: 0 / 4
rows after purge: 0
```

Updating one row's `algorithm_version` similarly causes all rows to be purged.

Window packets are zlib-compressed atomic files. For the 320×180×4-channel,
6-frame sample windows, the four persisted packets are 15–18 KB each; the complete
seeded work directory (inputs, frames, SQLite database, state, and all exports) is
under 1 MB.

## Interactive UI Server

Verified endpoints against the seeded work directory:

```text
GET  /                     embedded dashboard HTML
GET  /api/state            model + window matrix + invalidation state
POST /api/full             full recompute
POST /api/edit             incremental layer/visibility/time-base edit
GET  /api/frame            committed PNG frame
GET  /api/heatmap          previous-vs-current window diff grid
POST /api/export           deterministic export + round-trip verification
POST /api/validate         truth validation
```

Observed:

```text
state: 4/4 committed, 3 layers
frame: PNG image data, 320 x 180, 16-bit/color RGB
heatmap: available, 8×4 cells, max_delta sampled during incremental verification
```

## Wails Desktop

The Wails module and desktop binding code compile with Go 1.22:

```text
$ (cd desktop && go build .)
success (default Wails stub build)
```

A production window link was attempted with `-tags desktop,production`, but this
host has no `pkg-config`, `libgtk-3-dev` or `libwebkit2gtk-4.1-dev`. The host
package proxy returns HTTP 502, so those packages could not be installed without
root/network access. Required command on a prepared workstation:

```bash
sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
cd desktop
wails build -tags webkit2_41
./build/bin/virtual-stage
```

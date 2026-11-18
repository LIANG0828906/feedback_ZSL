# Recorded Test Results

Date: 2026-11-18  
Command: `python -m unittest discover -s tests -v`  
Environment: Python 3.14.4, NumPy 2.5.5, Linux/WSL2 x86-64

## Automated Suite

```text
Ran 32 tests in 44.269s

OK
```

The full raw log is `deliverables/test_results.txt`. The suite covers:

- Every required validation business code used by the deterministic invalid
  fixtures, plus the no-image-on-abort rule.
- Byte-stable scene generation and table export.
- Table invariance under triangle traversal reordering.
- Face-seed dependence, unit micro-normals, thickness-to-color modulation.
- Light-order-independent tile rendering.
- Hue continuity, caustic landing recomputation and leaf clipping.
- Penumbra monotonicity and smooth profile.
- Additive energy, ratio sums within `1e-6`, and non-negative contributions.
- Deterministic tile rendering with opaque premultiplied alpha.
- Snapshot round trip, checksum damage, stale block extents, scene changes, and
  film-parameter changes.
- Two uninterrupted renders matching byte-for-byte.
- Forced kill and resume matching the baseline.
- SIGINT finishing a block, committing the snapshot, exiting `0`, and resuming.
- A baseline mismatch producing block/pixel/channel diff records and exit `3`.

## Reference Image

```text
Scene digest:
700d0a5e2616b69ae8109982382e678b8f3afab340952db7938cd818448547b7

Combined RGBA + face-ID digest:
436e91d89e424166a4cff1c063c5151eb000327413f1bf4210303913107b73c3

Views: 7 (C00..C06), 192x144
Blocks: 112, each 48x36
Triangles: 320
```

Visual inspection files:

- `deliverables/reference/contact_sheet.png`
- `deliverables/reference/caustic_closeup_C03.png`
- `deliverables/reference/C03.rgba.png`

## Hue Continuity

```text
threshold:          8.0 degrees
max adjacent jump:  2.307692308 degrees
saturation gate:    0.10
result:             PASS
```

## Caustic Landing and Penumbra

```text
source  surface samples  in-bounds landings  width min   width median  width max
L1      320              102                 0.039277    0.049727      0.055347
L2      320              125                 0.042543    0.052213      0.056019
L3      320              104                 0.040241    0.047873      0.051337
```

The three probes per light are sorted by
`d_leaf*d_source/(d_leaf+d_source)`. Every later probe has a larger half-width.
The isolated Gaussian brightness at half-width is
`exp(-0.5) = 0.60653066` of center brightness. The report includes both values.

## Energy Consistency

```text
active report samples:          25
multi-light samples:            25
samples with all 3 lights:      23
samples with 2 lights:          2
max ratio-sum error:            0.0
leaf pixels audited:            14,405
max combined-minus-sum excess:  5.55e-17
max forward/reverse order delta:2.78e-17
tolerance:                      1e-12
result:                         PASS
```

## Restart Demonstration

`deliverables/resume_demo/` was produced by:

1. Start a render with 48x36 tiles and an 80 ms post-commit test pause.
2. Send SIGINT after five committed blocks.
3. Confirm exit code `0`, five blocks in the snapshot, and no image files.
4. Resume with `--resume --baseline deliverables/reference`.
5. Compare final arrays and caustic records.

Recorded result:

```text
interrupt signal:              SIGINT
committed blocks at stop:      5
interrupt exit code:           0
partial images at stop:        0
resume action:                 resumed
restored blocks:               5
resume exit code:              0
baseline difference status:    identical
baseline difference count:     0
```

The stop-time recovery report is
`deliverables/resume_demo_interrupt.json`.

## Notes

The forced-kill path is covered by the automated suite using `Popen.kill()`
rather than SIGINT. It reaches at least five committed blocks, terminates, then
resumes and compares to the same baseline. No final image exists before resume.

The PNG and zlib bytes, like all byte comparisons, are expected to be stable
within a fixed Python/NumPy/zlib runtime. The raw RGBA, face-ID, caustic, and
report semantics remain deterministic across equivalent platforms.

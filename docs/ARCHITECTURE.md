# Architecture

## Delivery Shape

StrokeLab is one importable Go package (`strokelab`) plus one command
(`cmd/strokediag`). The package has no hidden service and no CGO dependency.
Generated samples are exported in their target version and immediately loaded
back; the returned in-memory project is therefore the result of the public
loader, not a private generator shortcut.

## Internal Model

`Project` owns:

- deterministic identity/seed, canvas size, block size, rounding policy;
- candidate coverage mask (0..255);
- groups and normalized strokes;
- truth reference mask, per-domain statistics, and per-stroke truth
  contributions;
- source version, conversion version, and field-level conversion trace.

A normalized stroke uses:

- absolute integer pixel start/end;
- brush radius;
- hardness in permille;
- coverage weight in pp10000;
- non-negative stage priority and one-based operation sequence;
- source group and declaration index.

The renderer uses no floating point. Pixel centers and brush geometry are
scaled to 1/16 pixel integers. Integer square root computes radial distances.
Alpha uses checked integer source-over and half-away-from-zero rounding.

## Version Conversion

The loader dispatches exclusively on the top-level `version`. It never guesses
by shape. Each schema maps to the internal model:

- v1 absolute pixels, per-mille weights, absent hardness, optional zero-based
  step sequence, no group semantics. Groups are synthesized as
  `stage-<priority>`; this makes a v1 -> v3 upgrade expressible without
  inventing a semantic group.
- v2 relative offsets from the previous endpoint, quantum 2, per-mille weights,
  absent hardness, one-based order numbers, per-stroke group names.
- v3 nested groups, group stage, absolute pixels, explicit hardness, pp10000
  weights, one-based sequence.

Default backfills append traces of the form:

```json
{
  "path": "operations[0].hardness_permille",
  "old_value": "<missing>",
  "new_value": "1000",
  "rule": "v1.schema.default_hardness",
  "source_default": "1000"
}
```

Export is strict by default. A loss item contains a stable code, path, reason,
value, residual, and downgrade disposition. Examples:

- `group_structure_not_expressible` for semantic v2/v3 groups to v1;
- `hardness_not_expressible` for non-default hardness to v1/v2;
- `relative_coordinate_quantization` when a v2 offset is not aligned to its
  quantum;
- `weight_scale_truncation` when pp10000 is not exactly representable in
  per-mille.

For any successful strict export, the exporter immediately loads the bytes and
compares the target-expressible internal fields. A mismatch returns `E_RT`.
Conversion multiplications/additions use checked int64 arithmetic and return
`E_OVF` with field and operands.

## Composition Order

Strokes are sorted by `(stage, seq)`. A duplicate pair makes the result
ambiguous and returns `E_ORD`; declaration order is never a fallback tie-break.
The global order version hashes stroke IDs and order keys. Each block also has a
local order version derived only from participants that intersect the block.

This local version is important: a global order change only invalidates a block
when that block's participating sequence actually changes.

`BuildDependencyGraph` materializes the same relationships for audit:
`stroke -> order`, `stroke -> participant block`,
`order -> block`, `block -> topology`, and
`topology -> attribution/diagnostic`. Invalidation plans embed this graph.

## Raster and Blocks

The block grid is row-major. A stroke is rasterized only inside blocks whose
expanded brush bounds intersect the block.

For each process:

1. candidate pixels seed the block masks;
2. each canonical stroke contribution is computed independently per block;
3. the component is applied to the block mask;
4. the component is attached to the full-canvas truth/diagnostic contribution.

Workers may finish in any order. The scheduler returns results in submitted
order and the engine applies strokes in canonical order. Block masks and
attribution are therefore independent of scheduling.

Each block trace records:

- every geometric participant in canonical order;
- the last nonzero contributor and its basis;
- earlier nonzero strokes suppressed by later source-over;
- zero-coverage participants.

## Topology

Foreground domains are extracted at threshold 128 with row-major 4-connectivity.
For each domain:

- area and centroid in milli-pixels;
- tight bounding box;
- enclosed background components (holes);
- boundary pixel count;
- ring width index `round(2000 * area / boundaryPixels)` milli-pixels.

Expected and actual domains are greedily paired by descending IoU with
deterministic tie-breaks. Area, centroid, bbox, hole count, ring width, IoU, and
whole-mask pixel difference are tolerance checked. Any failure returns a
structured `E_TOP` report containing domain IDs and measured values.

## Attribution

For every pixel whose accumulated alpha differs from truth:

- collect all canonical stroke contribution components;
- select the largest component as primary responsibility;
- ties select the earlier canonical stroke;
- record the first nonzero contributor separately;
- compute share as `primary * 10000 / sumStrokeContributions`;
- if no stroke contributes, mark the candidate base as the reason.

The report is sorted row-major and does not depend on block completion order.

## Cache Keys and Invalidation

A block cache key is SHA-256 over:

- candidate content and block rect;
- participating stroke IDs and content hashes;
- local canonical order version;
- conversion parameter version;
- algorithm version.

Editing coordinates/hardness/weight invalidates the union of old and new
footprint blocks. Editing stage/seq invalidates only blocks whose local
participant order hash changes. A final cache-key comparison catches any missed
content dependency. Downstream topology, attribution, and diagnostics are
marked invalid when at least one block changes.

Complete block outputs and per-stroke block components are persisted in a
versioned JSON block cache using atomic replacement. Unaffected blocks are
loaded by exact key; affected blocks enter the scheduler.

## Checkpoint Journal

The standard-library persistence substitute is an append-only journal:

```text
STROKECKPT1\n
<base64(JSON record)> <crc32-hex>\n
...
```

One stroke transaction contains all affected block components. Appends are
serialized by a mutex; transaction IDs are contiguous; each append is one write
followed by `Sync`. Recovery:

- verifies the header and CRC of every complete record;
- rejects non-contiguous transaction IDs;
- ignores an incomplete/bad final tail;
- truncates the invalid tail before opening for future appends;
- resumes only if project, order, conversion, algorithm, and residual digests
  all match.

Resume replays committed components, verifies each block's component and
cumulative content digest, and starts at the first uncommitted canonical
stroke. A changed project or algorithm is rejected with `E_CKPT`.

## Three Paths

- Full: no journal; every block is recomputed.
- Incremental: every stroke is committed atomically as it completes.
- Resume: replay only committed version-matching strokes, then continue.

`CompareEvaluations` checks block masks first. If a block differs, it compares
per-stroke component digests to locate the first differing stroke and reports
both block ID and canonical stroke ordinal. It then checks the composite,
topology statistics, and attribution.

## Scheduler

The scheduler has explicit limits:

- maximum worker parallelism;
- maximum block pixels per worker;
- pending queue budget;
- retry count.

Queue-full and pixel-budget waits are recorded with reason and elapsed
milliseconds. Failed blocks are retried in isolation. A stroke is committed
only after every affected block succeeds, so a failure cannot leak a partial
stroke into the journal.

## Diagnostics

`WriteEvaluationDiagnostics` emits:

- `report.html` (local entry point);
- `evaluation.json` (machine-readable result);
- candidate/composite/reference/diff PNGs;
- one PNG per stroke contribution;
- a block status map colored reused/recomputed/invalidated.

The HTML report includes topology, attribution, invalidation entries, checkpoint
status, block winner/participants, and queue/retry records.

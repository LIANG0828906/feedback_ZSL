# Path and project formats

## Common geometry

Every contour has a `start` point and an ordered segment list. A segment is one
of:

```json
{"type":"line","to":[x,y]}
{"type":"quad","control":[x,y],"to":[x,y]}
{"type":"cubic","control1":[x,y],"control2":[x,y],"to":[x,y]}
```

The final segment endpoint must equal `start` within the declaration tolerance.
Batch results may additionally carry a `trace` array on each segment. A trace
entry names the operand, source contour, source segment, subsegment, and source
Bezier parameter interval.

## v1

```json
{
  "format": "vector-path",
  "version": 1,
  "fill_rule": "nonzero",
  "contours": [
    {
      "id": "outer",
      "start": [0, 0],
      "segments": [
        {"type": "line", "to": [10, 0]},
        {"type": "line", "to": [10, 10]},
        {"type": "line", "to": [0, 10]},
        {"type": "line", "to": [0, 0]}
      ]
    }
  ]
}
```

v1 has no explicit direction, group, share table, or tolerance. The loader
uses this deterministic rule:

1. Determine nesting depth from contour containment.
2. Even depth is `ccw` (solid); odd depth is `cw` (hole).
3. Reverse a contour's record if its vertex order disagrees with the inferred
   direction, recording `orientation_corrected`.
4. Vertices closer than the undeclared v1 default tolerance are added to a
   derived share table.

The undeclared v1 comparison tolerance is `1e-6`. Any operation may choose a
larger explicit working tolerance; that choice is recorded in the report.

## v2

```json
{
  "format": "vector-path",
  "version": 2,
  "fill_rule": "nonzero",
  "tolerance": 0.01,
  "groups": ["body", "cutout"],
  "contours": [
    {
      "id": "outer",
      "start": [0, 0],
      "direction": "ccw",
      "group": "body",
      "segments": []
    }
  ],
  "vertex_shares": [
    {
      "point": [10, 10],
      "refs": [
        {"contour": 0, "vertex": 2},
        {"contour": 1, "vertex": 0}
      ]
    }
  ]
}
```

v2 requires `tolerance`, contour `direction`, contour `group` (string or null),
the `groups` table, and `vertex_shares`. Direction must agree with vertex order.
Every group reference must resolve. Every share reference must be in range and
its coordinate must match the referenced vertex within `tolerance`. A bad
share returns `E_SHARE_INVALID` with `contour_index`.

## Downgrade

Writing v2 as v1 removes fields v1 cannot express. The operation still writes
the v1 file, but the compatibility report has code `E_VERSION_LOSS` and lists
every affected contour/field: direction, group, explicit shares, tolerance,
edge traces, groups, and path metadata. Nothing is silently dropped.

## Project

The project is a JSON envelope:

```json
{
  "project_format": "vpipe-project",
  "project_version": 1,
  "content_hash": "sha256:...",
  "payload": {
    "path_a": {},
    "path_b": {},
    "operation": "union",
    "tolerance": 0.01,
    "version_settings": {},
    "thresholds": {},
    "last_committed": {}
  }
}
```

`content_hash` is SHA-256 over canonical JSON of `payload`. Save uses a sibling
temporary file, file `fsync`, then `os.replace`; POSIX also fsyncs the directory.
`last_committed` is absent unless a result was actually committed.


# Verification Record

## Actual environment

- Date: 2026-11-23
- Host: WSL2 Linux (target instructions remain Windows 10+/Python 3.11)
- Python: 3.14.4
- PySide6: 6.12.0
- NumPy: 2.5.5
- Execution: local only, no Docker, no external service or network resource

The core uses only Python standard-library modules. The installed NumPy/PySide6 versions
were used for environment compatibility and the offscreen Qt smoke test.

## Automated tests

Command:

```bash
python3 -m pytest -q
```

Result:

```text
41 passed in 36.60s
```

Coverage includes:

- `E_SCHEMA`, `E_RANGE`, `E_REF` field/coordinate reporting;
- deterministic serial/parallel A* and `E_HEUR` overestimate reporting;
- smooth path passability, dual grid/world readings, cost floor, monotonic waypoint
  count under tighter tolerance, `E_PARA`, `E_FORM`;
- tick turn limit, on-polyline checks, invalid-step `E_PARA`, hard formation `E_ADV`,
  soft-deviation slowdown reason, stationary paths, and independent trace validation for
  segment jumps;
- leaf edit only invalidating `delta`;
- root edit invalidating the full descendant closure;
- off-path edit reusing all caches;
- blockade activate/deactivate and full consistency;
- multiple mixed edits and full consistency;
- off-path cost decrease and `E_DIVERGE` first-difference reporting;
- isolated member failure followed by retry without polluting siblings;
- explicit simulation interval dependency edges;
- checkpoint round-trip, version invalidation, edited-map invalidation, corrupt digest;
- concurrent checkpoint writes remaining atomic and stable;
- bounded executor order and error isolation;
- PySide6 offscreen GUI smoke test.

## Deterministic diagnostic

Command:

```bash
python3 scripts/run_diagnostics.py
```

Machine-readable result: `artifacts/verification_report.json`.
Exported trajectory: `artifacts/trajectory_export.json`.

Expected and observed key results:

| Scenario | Direct path invalidation | Reuse | Full comparison |
| --- | --- | --- | --- |
| Initial build | all four members | none (cold cache) | 2,004 ticks equal |
| Edit `(11,7)` cost `1 -> 4` | `delta` only | alpha, bravo, charlie paths and intervals | equal |
| Edit `(13,9)` cost `5 -> 8` | `alpha` only | bravo/charlie/delta path caches | equal; descendant smooth/replay recomputed |
| Activate `blk-crossing` | alpha + delta | bravo/charlie path caches | equal |
| Checkpoint after edits, reload base scene | snapshot rejected | no partial cache use | full rebuild required |

Final diagnostic timing from the recorded run:

```text
initial full build      882.983 ms
baseline full compare   883.429 ms (2,004 ticks, 76 path cells, 8 waypoints)
leaf edit compare       898.614 ms (delta invalidated, 63 intervals reused)
root edit compare      1042.155 ms (84 intervals recomputed through descendant closure)
blockade edit compare  1020.122 ms (alpha/delta path invalidation)
```

The final diagnostic JSON is the authoritative record of the run.

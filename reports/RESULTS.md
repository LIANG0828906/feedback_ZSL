# Causal kernel local validation

- Overall: **PASS**
- Seed: `20261121`
- NumPy: `2.5.5`
- Python: `3.14.4`

## Scenario comparisons

| scenario | fixed final | float final | truth diffs | branch diffs | uncertain | replay |
|---|---|---|---:|---:|---:|---|
| branch_flip.json | `5f9ae820860f` | `b9da28d2360c` | 2 | 1 | 2 | PASS |
| deterministic_small.json | `a68dd586b6cb` | `f4c0099a74e7` | 0 | 0 | 0 | PASS |
| deterministic_medium.json | `ff2be0501d0d` | `ca27a9ebc6a3` | 0 | 0 | 0 | PASS |
| deterministic_deep.json | `1e9522584a60` | `95aa91903fa7` | 0 | 0 | 0 | PASS |

## Fault injection

| injection | code | trace written | result |
|---|---|---|---|
| cycle | `E_INJECT_CYCLE` | False | PASS |
| deep-chain | `E_INJECT_DEPTH_EXCEEDED` | False | PASS |
| nonfinite | `E_INJECT_NONFINITE_COEFFICIENT` | False | PASS |
| precision-conflict | `E_INJECT_PRECISION_CONFLICT` | False | PASS |
| truncated | `E_INJECT_TRUNCATED` | False | PASS |

## Recovery and integrity

- Forced termination recovery: PASS
- Recovered trace byte-identical: PASS
- Snapshot state-hash rejection: PASS
- Snapshot version rejection: PASS
- Byte-identical deterministic traces: PASS
- Tampered trace rejection: PASS
- Truncated trace rejection: PASS

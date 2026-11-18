# Report contract

`report.json` is written atomically and uses sorted JSON keys.  `business_digest`
is SHA-256 over the stable business fields: input identities/hashes, consumed
inputs, dedup, late, joined window rows, join diagnostics and skew plan/loads.
Two fresh runs of the same input have the same digest even if duration, RSS or
SQLite file sizes vary.

Top-level fields:

| Field | Contents |
| --- | --- |
| `schema_version` | report schema, currently 1 |
| `status` | `complete` or `failed` |
| `error` | null on success, stable `{code,message,subject}` on failure |
| `semantics` | executable business rules |
| `input` | shards/batches with declared rows, hashes, declared batch times |
| `processing` | consumed inputs, last batch index, watermark |
| `dedup` | canonical event count/key-window count and dropped events |
| `late` | total/per-window late counts, side split, amount and event-time range |
| `windows` | minute Join aggregates with counts, amounts, net, close reason |
| `join_diagnostics` | unmatched refunds and currency mismatches |
| `skew` | slots, threshold, planned and actual per-slot loads, split records |
| `state` | last commit, open/committed window counts, spill bytes |
| `recovery` | recovery count and checkpoints |
| `resources` | workers, memory/spill thresholds, peak RSS and cleanup |
| `warnings` | manifest gaps or non-monotonic declarations |
| `timing` | start/finish/duration |

Output files:

* `windows.csv` – minute aggregate;
* `key_windows.csv` – per payment id / minute detail;
* `metrics.prom` – Prometheus text exposition;
* `report.partial.json`, `windows.partial.csv`, `key_windows.partial.csv`
  on `E_INPUT`/`E_MEMORY`;
* `last_error.json` – stable error envelope;
* `termination_injected.json` – fault-injection marker.

Monetary values appear as exact minor-unit integers plus decimal strings.
All window arrays and input arrays are sorted deterministically.

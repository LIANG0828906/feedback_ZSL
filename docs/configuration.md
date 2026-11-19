# Configuration Reference

## Change plan

Required top-level fields:

| Field | Meaning |
|---|---|
| `schema_version` | Must be `1`. |
| `release_id` | Business release identifier. |
| `target_version` | Version installed by batches. |
| `rollback_version` | Default rollback target. |
| `observation_window_seconds` | Positive observation window. |
| `instances` | Declared instance inventory. |
| `batches` | Ordered release batches. |
| `network` | Loopback host, ports and timeouts. |
| `storage` | SQLite and default report paths. |
| `sampling` | Fixed interval and per-instance quota. |

Each instance requires `id`, `group`, `port` and `initial_version`. Instance ids
and ports must be unique.

Each batch requires `id`, `order`, a non-empty `instances` array and may override
`rollback_version`. Orders must be unique, contiguous and start at 1. An
instance cannot occur in more than one batch.

`network.port_range` is the scenario allocation range. `backend_port_range` is
used for ephemeral version backends. `scenario` replaces occupied preferred
ports with free ports from the declared range and writes the resolved values to
`resolved-plan.json`.

## SLO

| Field | Meaning |
|---|---|
| `availability.target_percent` | Availability target, 0..100. |
| `latency.percentile` | Percentile to calculate, 0..100. |
| `latency.target_ms` | Positive latency threshold in milliseconds. |
| `latency.allowed_bad_percent` | Allowed tail violations, 0..100. |
| `minimum_samples` | Positive minimum total sample count. |
| `error_budget.max_consumed_percent` | Maximum combined consumed budget, 0..100. |

The decision fails on any of:

- sample count below the minimum
- availability below target
- calculated percentile latency above target
- combined error budget above the configured cap

The P99 trigger is independent of the budget trigger.

## Injection

Top-level `expected_outcome` must be one of `RELEASED`, `ROLLED_BACK`,
`PARTIAL_ROLLBACK`, `ROLLBACK_FAILED` or `ABORTED`. The scenario exits with
code 4 if the actual outcome differs.

`defaults.versions` applies to every instance. `instances.<id>.versions`
overrides individual version profiles. A profile supports:

- `latency_ms`
- `error_percent`
- `error_status` (500..599)
- `health_fail_ms`

`rollback_restart_refused` is a per-instance or default boolean.

`load.rate_per_second` and `load.workers` control the local HTTP load
generator.

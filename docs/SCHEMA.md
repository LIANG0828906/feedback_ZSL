# Schema versions

The deterministic generator emits the same logical project in three JSON
spellings. All spellings load into one normalized `registry.Project`.

## Shared event stream

Events are ordered by tick. A version changes duration fields and field names,
not the logical event order.

| Event | Meaning |
| --- | --- |
| `register` | Register an instance and start its lease. |
| `deregister` | Move an instance to removal-pending. |
| `heartbeat` | Renew a registered instance lease. |
| `probe_result` | Complete the current in-flight probe with success/failure. |
| `query` | Build a catalog for one service and optional display scale. |

The effective probe result rule is intentionally deterministic: an event with
an explicit `probe_id` must match an in-flight task; an event without
`probe_id` binds to the instance's current in-flight task.

## v1

- Field style: `snake_case`.
- Instance shape: top-level flat `instance_rows` array.
- Durations: positive integer ticks (`lease_ttl_ticks`,
  `heartbeat_every_ticks`, `probe_every_ticks`, `probe_timeout_ticks`).
- Thresholds: absolute consecutive counts
  (`success_threshold_abs`, `failure_threshold_abs`).
- Lease watermark: absent.

Normalization:

```text
milliseconds = ticks * tick_ms
threshold_absolute = threshold_absolute
```

## v2

- Field style: `camelCase`.
- Instance shape: instances nested under each service.
- Durations: integer milliseconds (`leaseTtlMs`, `heartbeatEveryMs`,
  `probeEveryMs`, `probeTimeoutMs`).
- Thresholds: remaining verdict count
  (`successThresholdRemaining`, `failureThresholdRemaining`).
- Lease watermark: optional `leaseWatermarkMs`.

Normalization:

```text
milliseconds = milliseconds
threshold_absolute = threshold_remaining + 1
```

If a watermark is present, it must equal the normalized lease TTL. The
watermark is then removed from the runtime model because it has no runtime
semantics. This keeps canonical hashes equal across versions.

## v3

- Field style: `kebab-case`.
- Instance shape: instances nested under each service.
- Durations: composite seconds plus milliseconds, for example
  `"lease-ttl-seconds": 1, "lease-ttl-millis": 500`.
- Thresholds: remaining verdict count, or the legacy generic
  `success-threshold`/`failure-threshold` with `threshold-mode`.
- Lease watermark: `lease-watermark-ms`, optional.
- Legacy `lease-ttl` accepts a unit-ambiguous number only when
  `lease-watermark-ms` can disambiguate ticks from milliseconds.

Normalization:

```text
milliseconds = seconds * 1000 + millis
threshold_absolute = threshold_remaining + 1
```

The milliseconds component must be in `[0, 999]`. The total duration must be
positive and at most seven days.

## Threshold state machine

Canonical thresholds are consecutive absolute counts.

| State | Success rule | Failure rule |
| --- | --- | --- |
| `initial` | success threshold -> `healthy` | failure threshold -> `unhealthy` |
| `healthy` | stays healthy | failure threshold -> `unhealthy` |
| `unhealthy` | success threshold -> `half_open` | stays unhealthy |
| `half_open` | success threshold -> `healthy` | failure threshold -> `unhealthy` |

Every state change resets both consecutive counters. Probe timeout is treated
as a probe failure with cause `probe_timeout`.

## Catalog rules

Catalog order is stable:

1. Tier: healthy (0), admitted initial (1), demoted half-open (2).
2. Effective weight descending.
3. Canonical instance declaration order.

Exclusion basis codes are stable:

- `E_EXCL_NOT_REGISTERED`
- `E_EXCL_LEASE_EXPIRED`
- `E_EXCL_DEREGISTERED`
- `E_EXCL_UNHEALTHY`
- `E_EXCL_HALF_OPEN`
- `E_EXCL_INITIAL`

Half-open instances are either excluded or admitted at a declared factor,
depending on the service policy. Display scale never changes candidate order.

## Embedded truth

Sample files include `truth.steps[]`. Each step contains:

- all instance phases, health, lease expiry/remaining and renewal count;
- due, suppressed, deferred and dropped probes;
- health transitions;
- all service catalogs;
- explicit query results.

The loader verifies the embedded truth's project version and normalized model
hash when those fields are present.

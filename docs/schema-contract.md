# Schema Contract

## Normalized Channel

All three loaders populate the same canonical fields:

| Canonical field | Meaning |
|---|---|
| `ID` / `Order` | Stable channel identity and declared order |
| `RatePermyriad` | `1/10000 byte/ms` |
| `CapacityBytes` | Burst bucket capacity |
| `InitialTokens` | Initial bucket watermark |
| `Policy` | `drop` or `defer` |
| `Priority` | Channel scheduling priority |
| `WeightPermyriad` | Deficit credit weight |
| `QueueAgeMS` | Delay expiry quantization |
| `MaxDelayMS` | Hard queue age, 0 means unbounded |
| `PriorityBoost` | Optional DSCP priority lift, default 0 |
| `DSCPBoostClasses` | DSCP classes that activate the lift |

## v1: Flat Tick/Packet Table

```json
{
  "schema": "qos.v1",
  "project": {
    "version": "gen-42-r1",
    "tick_ms": 10,
    "integer_width": 64,
    "nominal_packet_bytes": 128,
    "egress_budget_packets": 4,
    "worker_count": 4,
    "queue_depth": 128,
    "horizon_ticks": 80,
    "shaping": [
      {
        "queue": "gold",
        "order": 0,
        "rate_permille": 6000,
        "burst_packets": 8,
        "initial_watermark_packets": 4,
        "policy": "defer",
        "priority": 2,
        "weight_permille": 600,
        "queue_age_ticks": 2,
        "max_delay_ticks": 30
      }
    ],
    "arrivals": [
      {
        "packet": "p001",
        "queue": "gold",
        "arrival_tick": 0,
        "bytes": 320,
        "priority": 1,
        "dscp": 46
      }
    ]
  }
}
```

v1 intentionally has no priority-boost field. The loader defaults it to zero.
Truth output uses absolute tick values for `expire_tick`.

## v2: Per-Channel Parameter Tree

```json
{
  "schema": "qos.v2",
  "project": {
    "version": "gen-42-r1",
    "runtime": {
      "tick_ms": 10,
      "integer_width": 64,
      "nominal_packet_bytes": 128,
      "egress_budget_bytes": 512,
      "worker_threads": 4,
      "queue_depth": 128,
      "horizon_ms": 800
    },
    "channels": {
      "profiles": [
        {
          "id": "gold",
          "ordinal": 0,
          "qos": {
            "rate": {"permyriad": 60000},
            "burst": {"bytes": 1024},
            "watermark": {"bytes": 512},
            "queue_age_ms": 20,
            "max_delay_ms": 300,
            "policy": "defer",
            "priority": 2,
            "weight": {"permyriad": 6000},
            "priority_boost": 0,
            "dscp_boost_classes": []
          }
        }
      ]
    },
    "arrival_stream": [
      {
        "id": "p001",
        "channel_id": "gold",
        "arrival_ms": 0,
        "bytes": 320,
        "priority": 1,
        "dscp_class": 46
      }
    ]
  }
}
```

v2 truth uses absolute millisecond expiry values.

## v3: Flat Millisecond Table

```json
{
  "schema": "qos.v3",
  "project": {
    "version": "gen-42-r1",
    "tick_ms": 10,
    "defaults": {
      "integer_width": 64,
      "nominal_packet_bytes": 128,
      "worker_count": 4,
      "queue_depth": 128
    },
    "egress_budget_bytes": 512,
    "horizon_ms": 800,
    "shaping_table": [
      {
        "channel": "gold",
        "slot": 0,
        "rate_value": 60000,
        "rate_scale": 10000,
        "burst": {"packets": 8},
        "watermark": {"packets": 4},
        "queue_age": {"remaining_ms": 20},
        "max_remaining_ms": 300,
        "policy": "defer",
        "priority": 2,
        "weight_scale": 1000,
        "weight": 600
      }
    ],
    "packets": [
      {
        "id": "p001",
        "channel": "gold",
        "at_ms": 0,
        "size_bytes": 320,
        "priority": 1,
        "dscp_class": 46
      }
    ]
  }
}
```

v3 truth expresses each delay-queue member as `remaining_ms` relative to the
step `at_ms`. It may omit `priority_boost` when zero.

## Conversion Rules

```text
RatePermyriad = rate_value * (10000 / rate_scale)
rate_scale in {1000, 10000}

CapacityBytes = burst_packets * nominal_packet_bytes
arrival_ms     = arrival_tick * tick_ms
expire_ms      = now_ms + remaining_ms
```

Every multiplication and addition is checked against the declared signed integer
width (32 or 64). Refill keeps an exact remainder in `[0,9999]`.

## Conflict Handling

If two aliases are present, their normalized values must match. Examples:

- `rate_permille` and `rate_permyriad`
- `burst.packets` and `burst.bytes`
- `arrival_tick` and `arrival_ms`
- `queue_age_ticks` and `queue_age.remaining_ms`

Conflicts return `E_FIELD_CONFLICT` with the normalized values and source JSON
paths. No loader mutates the input bytes.

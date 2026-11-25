# Schema Mapping

The three documents below describe the same domain project. Complete generated
examples live in `artifacts/generated/v1.json`, `v2.json`, and `v3.json`.

## Common Normalized Concepts

- `Tier` is exactly `hot` or `cold`.
- All node capacities become signed `int64` bytes.
- KiB values are multiplied by 1024 with overflow detection.
- All logical times become integer milliseconds.
- v1 tick values are multiplied by `tickMs`.
- Object ids, node ids, rack names, and event fields are canonicalized before
  use, so traversal order is schema-independent.

## v1: Compact Ticks

Discriminator:

```json
{"schema": "compact/v1"}
```

Field examples:

```json
{
  "projectId": "edge-thermal-42",
  "tickMs": 100,
  "nodes": [
    {
      "id": "hot-a",
      "tier": "hot",
      "rack": "rack-a",
      "capacityBytes": 12288,
      "objects": ["o01", "o02"],
      "evictionTick": 8
    }
  ],
  "objects": [
    {
      "id": "o05",
      "sizeBytes": 2048,
      "tier": "cold",
      "accessCounts": [0, 4, 0, 0, 0, 0, 0],
      "lastAccessTick": 0,
      "ttlTicks": 50,
      "priority": 10,
      "heatWatermark": 3500
    }
  ],
  "accessEvents": [{"atTick": 3, "objectId": "o08", "count": 6}],
  "opEvents": [{"atTick": 1, "objectId": "o05", "kind": "write", "requestedTier": ""}],
  "capacityEvents": [{"atTick": 3, "nodeId": "hot-b", "capacityBytes": 16384}],
  "migrations": [{"atTick": 5, "objectId": "o10", "from": "cold", "to": "hot", "targetNodeId": "hot-a"}]
}
```

Normalization:

- `lastAccessTick * tickMs -> lastAccessMs`
- `ttlTicks * tickMs -> ttlMs`
- event `atTick * tickMs -> atMs`
- `commitDelayTicks * tickMs -> commitDelayMs`

## v2: Normalized Milliseconds

Discriminator:

```json
{"schema_version": "normalized/v2"}
```

Field examples:

```json
{
  "project_id": "edge-thermal-42",
  "tick_ms": 100,
  "nodes": [
    {
      "id": "hot-a",
      "tier": "hot",
      "rack": "rack-a",
      "capacity_kib": 12,
      "objects": ["o01", "o02"],
      "eviction_tick": 8
    }
  ],
  "objects": [
    {
      "id": "o05",
      "size_bytes": 2048,
      "tier": "cold",
      "access_counts": [0, 4, 0, 0, 0, 0, 0],
      "last_access_ms": 0,
      "ttl_ms": 5000,
      "priority": 10,
      "heat_watermark": 3500
    }
  ]
}
```

Normalization:

- node `capacity_kib * 1024 -> capacityBytes`
- capacity event `capacity_kib * 1024 -> capacityBytes`
- all time fields already in milliseconds

## v3: Rack Tree And Tier Nested Objects

Discriminator:

```json
{"schemaVersion": "racked/v3"}
```

Field examples:

```json
{
  "projectId": "edge-thermal-42",
  "tickMs": 100,
  "rackGroups": [
    {
      "rack": "rack-a",
      "tiers": {
        "hot": [
          {"id": "hot-a", "capacityKiB": 12, "objects": ["o01", "o02"], "evictionTick": 8}
        ],
        "cold": [
          {"id": "cold-a", "capacityKiB": 32, "objects": ["o01", "o03"], "evictionTick": 16}
        ]
      }
    }
  ],
  "objectsByTier": {
    "hot": [
      {
        "id": "o01",
        "sizeBytes": 2048,
        "accessCounts": [4, 0, 0, 0, 0, 0, 0],
        "lastAccessMs": 0,
        "ttlMs": 5000,
        "priority": 7
      }
    ],
    "cold": [
      {
        "id": "o05",
        "sizeBytes": 2048,
        "accessCounts": [0, 4, 0, 0, 0, 0, 0],
        "lastAccessMs": 0,
        "ttlMs": 5000,
        "priority": 10,
        "heatWatermark": 3500
      }
    ]
  }
}
```

Normalization:

- rack name is copied into every node;
- rack `tiers` keys become `Node.Tier`;
- `objectsByTier` keys become `Object.InitialTier`;
- `capacityKiB * 1024 -> capacityBytes`;
- object and node arrays are canonicalized after flattening.

## Strictness

Each version has a separate typed decoder with unknown-field rejection. It is
therefore not possible to silently mix v1 and v2 field names. Multiple
discriminators, unknown nested fields, malformed JSON, missing required fields,
and invalid enum values return stable `E_SCHEMA_*` codes with context.

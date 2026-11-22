# TickArena 文件格式

所有 JSON 由以下规则序列化：

```python
json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
```

文件使用 UTF-8，每个 JSON 对象一行，以 `\n` 结尾。

## 1. 事件流

第一行必须是 match header：

```json
{"format_version":1,"initial_state":{...},"kind":"match"}
```

后续每行是一个事件。`tick` 必须是整数；事件类型由 `type` 决定。

### 移动

```json
{"actor":"u1","dx":1,"dy":0,"tick":12,"type":"move"}
```

### 技能

```json
{"actor":"u1","skill":"bolt","tick":12,"target":"u2","type":"skill"}
```

### 采集

```json
{"actor":"u1","tick":12,"type":"gather"}
```

### 道具

```json
{"actor":"u1","item":"potion","tick":12,"type":"item"}
```

引擎使用事件行在文件中的输入序号，不信任事件内的可选序号字段。

## 2. 初始状态

```json
{
  "format_version": 1,
  "seed": 20261123,
  "tick_count": 100000,
  "map": {"width": 32, "height": 32},
  "rules": {
    "gather_base": 20,
    "gather_variance": 5,
    "gather_drop_bps": 1200,
    "potion_drop": 1,
    "potion_heal_bps": 3500,
    "revive_hp_bps": 5000,
    "revive_resource_bps": 5000,
    "passive_resource": 1,
    "score_per_kill": 1,
    "win_score": 1000000,
    "crit_multiplier_bps": 20000
  },
  "players": [
    {
      "player_id": "p1",
      "team": "red",
      "unit": {
        "unit_id": "u1",
        "x": 2,
        "y": 2,
        "spawn_x": 2,
        "spawn_y": 2,
        "hp": 260,
        "max_hp": 260,
        "resource": 100,
        "max_resource": 200,
        "attack": 10,
        "defense": 2,
        "move_speed": 1,
        "revive_ticks": 25,
        "skills": {
          "bolt": {
            "cost": 18,
            "cooldown": 4,
            "range": 64,
            "damage": 45,
            "variance_bps": 1200,
            "crit_bps": 1500,
            "hit_bps": 9200,
            "crit_multiplier_bps": 20000
          }
        },
        "items": {"potion": 3}
      }
    }
  ]
}
```

解析后所有默认规则都会补齐。初始表规范化和指纹基于补齐后的对象。

## 3. 回放

### Header

```json
{
  "kind": "header",
  "protocol": "tickarena.replay/1",
  "format_version": 1,
  "seed": 20261123,
  "max_ticks": 100000,
  "initial_fingerprint": "<sha256>",
  "event_fingerprint": "<sha256>",
  "snapshot_interval": 1000,
  "event_buffer_limit": 4096,
  "initial_state": {...}
}
```

### Tick

```json
{
  "kind": "tick",
  "tick": 42,
  "units": {
    "u1": {
      "x": 3,
      "y": 2,
      "hp": 260,
      "resource": 92,
      "alive": true,
      "cooldowns": {"bolt": 3}
    }
  },
  "scores": {"blue": 0, "red": 0},
  "result": null,
  "events": [
    {"seq": 77, "type": "skill", "actor": "u1", "code": null},
    {"seq": 78, "type": "skill", "actor": "u1", "code": "E_COOLDOWN"}
  ],
  "event_offset": 79
}
```

超额事件摘要：

```json
{"seq": 79, "type": "*", "actor": "*", "code": "E_BUFFER", "count": 12}
```

### Snapshot

```json
{
  "kind": "snapshot",
  "tick": 1000,
  "units": {
    "u1": {
      "player_id": "p1",
      "team": "red",
      "x": 8,
      "y": 4,
      "hp": 214,
      "max_hp": 260,
      "resource": 120,
      "max_resource": 200,
      "alive": true,
      "respawn_at": null,
      "cooldowns": {"bolt": 0},
      "items": {"potion": 2}
    }
  },
  "scores": {"blue": 2, "red": 3},
  "result": null,
  "event_offset": 803,
  "rng_decisions": 901,
  "rejected_total": 14
}
```

### Footer

```json
{
  "kind": "footer",
  "protocol": "tickarena.replay/1",
  "format_version": 1,
  "records": 100003,
  "body_sha256": "<sha256>",
  "final_tick": 99999,
  "final_state": {...},
  "key_metrics": {
    "final_tick": 99999,
    "scores": {"blue": 985, "red": 978},
    "result": {...},
    "event_offset": 80028,
    "rng_decisions": 91000,
    "rejected_total": 19984
  }
}
```

`body_sha256` 覆盖 header 之后、footer 之前的每条 canonical JSON 行（含换行）。

## 4. 检查点

```json
{
  "kind": "tickarena.checkpoint",
  "format_version": 1,
  "engine_tick": 9216,
  "payload": {
    "seed": 20261123,
    "max_ticks": 100000,
    "initial_fingerprint": "<sha256>",
    "event_fingerprint": "<sha256>",
    "replay_format_version": 1,
    "limits": {
      "max_batch_ticks": 1024,
      "event_buffer_limit": 4096,
      "memory_limit_bytes": 67108864
    },
    "state": {...},
    "event": {"index": 7350, "last_seen_tick": 9216},
    "replay": {
      "enabled": true,
      "path": "runtime/replays/large.replay.jsonl",
      "progress": {
        "partial_path": "runtime/replays/large.replay.jsonl.partial",
        "bytes": 4200000,
        "records": 9226,
        "body_sha256": "<sha256>",
        "last_tick": 9216,
        "last_snapshot_tick": 9000
      }
    },
    "replay_finalized": false
  }
}
```

恢复时：

- 未知 envelope 版本、事件指纹、初始指纹或回放版本失配：废弃检查点，从 tick 0 重新推演。
- partial 缺失、哈希不匹配或长度不足：同样整局重推。
- partial 比检查点长：截断到最后一次已提交长度，再继续。


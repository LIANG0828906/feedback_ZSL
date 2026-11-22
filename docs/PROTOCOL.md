# 同步帧协议规范

内部权威摘要不包含 `source_version` 或 `source_time_base`，因此同一核心状态从三个协议版本解码后具有相同 SHA-256 摘要。`source_version` 仅作为可观测元数据保留，不会污染跨版本一致性。

## 内部定点和时间模型

| 字段 | 单位 |
|---|---|
| `x`, `y` | `1/1000` 世界单位，范围 0..64000 |
| `heading` | `1/1000` 度，范围 0..359999 |
| `hp`, `max_hp` | 整数定点生命 |
| `cooldown` | 剩余 tick 数 |
| `resource` | 整数定点资源 |
| `logical_clock` | 每 tick 增加 120，即 120 Hz 逻辑时钟 |

与服务端 tick 的换算：

```text
v1 tick           = server_tick
v2 tick           = round(server_time_us * tick_rate / 1_000_000)
v3 logical_clock  = tick * 120
```

v2 编码时间采用向上取整的微秒换算，避免低 tick 被截断到 0；解码时四舍五入回 tick。

## v1: legacy-tick-delta

头字段：

```json
{
  "protocol_version": 1,
  "type": "sync",
  "sequence": 12,
  "server_tick": 12,
  "full": false,
  "match": "local-collab-demo",
  "units": []
}
```

单位字段：

| Wire 字段 | 内部字段 |
|---|---|
| `id` | `unit_id` |
| `p` | `[x, y]` |
| `rot` | `heading` |
| `hp` | `hp` |
| `hp_max` | `max_hp` |
| `cd` | `cooldown` |
| `mana` | `resource` |
| `st` | `status_mask` |
| `owner` | `owner_id` |
| `team` | `team` |

除 tick 0 的引导快照和显式恢复快照外，只传变化字段。访问字段缺失表示保留上一已确认值。

示例差量：

```json
{"id":"u1","p":[8231,8010],"rot":4512,"cd":2}
```

## v2: microsecond-delta-with-mask-semantics

头字段：

```json
{
  "protocol_version": 2,
  "kind": "sync_delta",
  "sequence": 12,
  "server_time_us": 100001,
  "full_snapshot": false,
  "match_id": "local-collab-demo",
  "content_hash8": "d16897e6",
  "units": []
}
```

单位字段：

```json
{
  "unit_id": "u1",
  "position": {"x": 8231, "y": 8010},
  "facing_mdeg": 4512,
  "health": {"current": 94000, "maximum": 100000},
  "cooldown_ticks": 2,
  "resource_units": 49050,
  "status": 1,
  "owner_id": "p1",
  "team": "alpha"
}
```

普通字段缺失表示保留上一值；`status` 是显式例外：缺失表示归零。编码器在状态位非零时每帧显式携带 `status`，在从非零变为零时显式携带 `0`。

`content_hash8` 是完整内部状态摘要的前 8 个十六进制字符。解码器重建状态后必须复核该前缀，不一致返回 `E_DECODE`。

## v3: logical-clock-snapshot-delta

头字段：

```json
{
  "protocol_version": 3,
  "kind": "snapshot_delta",
  "sequence": 12,
  "logical_clock": 1440,
  "match_id": "local-collab-demo",
  "snapshot_hash": "<sha256>",
  "snapshot": {},
  "delta": []
}
```

`snapshot` 每帧完整携带所有单位：

```json
{
  "u1": {
    "owner_id": "p1",
    "team": "alpha",
    "position": {"x": 8231, "y": 8010},
    "heading_mdeg": 4512,
    "health_current": 94000,
    "health_maximum": 100000,
    "cooldown_ticks": 2,
    "resource_units": 49050,
    "status_mask": 1
  }
}
```

`delta` 是相对上一帧的变化字段投影。解码器用完整快照建状态，再校验每个差量字段与快照一致；差量引用不存在单位或与快照冲突返回 `E_SEMANTIC`。逻辑时钟必须严格递增且按 120 Hz 对齐。

## 确定性编码

所有帧在落盘、传输和摘要前统一执行：

```python
json.dumps(value, ensure_ascii=False, allow_nan=False,
           separators=(",", ":"), sort_keys=True).encode("utf-8")
```

因此：

- 同一内部状态重复编码字节完全一致。
- 字典插入顺序、线程或进程不同不改变输出。
- v1/v2/v3 的 v3 完整哈希、v2 哈希前缀和 v1 重算策略分别明确。

## 降级映射

编码到 v1 时记录：

- `logical_clock -> server_tick`，策略 `tick_div`
- `snapshot_hash -> 重算`，策略 `omit_and_recompute`
- `source_time_base -> server_tick`，策略 `time_base_coercion`

编码到 v2 时记录：

- `logical_clock -> server_time_us`，策略 `clock_to_microseconds`
- `snapshot_hash -> content_hash8`，策略 `hash_prefix_8`
- `source_time_base -> server_time_us`，策略 `time_base_coercion`

以上均产生 `E_LOSSY` 警告，但不会阻止编码。v3 没有降级警告。

## 异常判定顺序

1. JSON 解析失败：`E_DECODE`。
2. 版本缺失/未知：`E_VER`，路径 `$.protocol_version`。
3. 必填头/单位字段缺失：`E_VER`，带具体路径和源版本。
4. 类型或哈希损坏：`E_DECODE`。
5. 时间倒退、单位引用不存在、差量冲突：`E_SEMANTIC`，带冲突对象。
6. 相邻差量序号不连续：`E_SEQ`。

客户端在处理 `E_SEQ` 时只缓冲未来帧或忽略重复/倒序帧，不修改最后确认状态。


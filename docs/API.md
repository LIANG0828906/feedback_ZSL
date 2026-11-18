# HTTP API

默认前缀：`http://127.0.0.1:8090`。除 `/metrics` 外，请求和响应均为
UTF-8 JSON；`/metrics` 为 Prometheus `text/plain; version=0.0.4`。

HTTP/1.1 请求必须发送正确的 `Content-Length`。业务失败统一为：

```json
{
  "ok": false,
  "error": {
    "code": "OFFSET_REGRESSION",
    "numeric_code": 40005,
    "message": "offset would move the committed offset backwards",
    "details": {
      "committed_offset": 42,
      "attempted_offset": 41
    }
  }
}
```

## 错误码

| code | numeric | HTTP | 含义 |
| --- | ---: | ---: | --- |
| `INVALID_REQUEST` | 40001 | 400 | 参数或路由非法 |
| `UNKNOWN_MEMBER` | 40002 | 404 | 成员从未加入 |
| `MEMBER_NOT_ACTIVE` | 40003 | 409 | 成员已离开或超时 |
| `PARTITION_NOT_OWNED` | 40004 | 409 | 成员不持有分区 |
| `OFFSET_REGRESSION` | 40005 | 409 | 提交偏移小于已提交值 |
| `PARTITION_PAUSED` | 40006 | 409 | 分区暂停，提交被冻结 |
| `REBALANCE_IN_PROGRESS` | 40007 | 409 | 分配切换中 |
| `STALE_GENERATION` | 40008 | 409 | generation 已过期 |
| `OFFSET_NOT_AVAILABLE` | 40009 | 404 | 偏移不可用 |
| `KAFKA_UNAVAILABLE` | 50001 | 503 | Kafka 连接或协议失败 |
| `BACKEND_ERROR` | 50002 | 503 | 消息底座错误 |
| `CONFIGURATION_ERROR` | 50003 | 500 | 配置错误 |

## 成员

### `POST /v1/members/join`

```json
{
  "member_id": "worker-1",
  "session_timeout_ms": 10000,
  "heartbeat_interval_ms": 1000
}
```

成功返回：

```json
{
  "ok": true,
  "group_id": "order-events-consumers",
  "topic": "order-events",
  "member_id": "worker-1",
  "generation": 2,
  "assignment_generation": 2,
  "owned_partitions": [0, 3, 6, 9],
  "paused_partitions": [],
  "assignments": [
    {"partition": 0, "member_id": "worker-1", "generation": 2, "paused": false}
  ],
  "rebalance_pending": false,
  "rebalance_in_progress": false
}
```

同一成员重复 join 是幂等的。已超时/离开的成员再次 join 会创建新 incarnation。

### `POST /v1/members/{member_id}/heartbeat`

空对象 `{}` 即可。可携带新的 `session_timeout_ms` 或
`heartbeat_interval_ms`，但正常运行时应在 join 时固定。

```json
{
  "ok": true,
  "member_id": "worker-1",
  "active": true,
  "generation": 2,
  "owned_partitions": [0, 3, 6, 9],
  "paused_partitions": [],
  "revoked_partitions": [],
  "revoke_ack_required": false,
  "rebalance_pending": false,
  "rebalance_in_progress": false
}
```

成员已超时返回 `MEMBER_NOT_ACTIVE`。协调器只在
`now - last_heartbeat > session_timeout_ms` 时移出成员；短暂消费停顿只要
心跳线程仍在运行就不会触发重平衡。

### `POST /v1/members/{member_id}/leave`

主动离开，回收该成员全部分区并写入隔离记录。重复 leave 幂等。

### `POST /v1/members/{member_id}/ack-revoke`

```json
{"partitions": [3, 6]}
```

成员停止消费并处理完已派发批次后确认撤销。省略 `partitions` 表示确认该成员
当前所有待撤销分区。

## 拉取与处理

### `POST /v1/members/{member_id}/fetch`

```json
{"partition": 3, "max_records": 250, "max_bytes": 1048576}
```

`partition` 可省略，协调器在成员当前可消费分区中轮转。返回：

```json
{
  "ok": true,
  "member_id": "worker-1",
  "partition": 3,
  "generation": 2,
  "fetch_offset": 120,
  "next_commit_offset": 123,
  "high_watermark": 500,
  "paused": false,
  "records": [
    {
      "partition": 3,
      "offset": 120,
      "key": "ORD-0000000121",
      "value": {
        "order_id": "ORD-0000000121",
        "amount": 19.5,
        "event_time": "2025-01-01T00:00:01.200Z"
      },
      "timestamp_ms": 349
    }
  ]
}
```

- 分区暂停或全部暂停时返回 200，`records=[]`，并给出 `paused_reason`。
- 分派记录会建立批次租约；租约未通过 commit 释放前，重平衡不会把该分区交给
  另一个成员。
- 返回记录不会自动推进已提交偏移；worker 完成处理后再提交
  `next_commit_offset`。

## 提交

### `POST /v1/members/{member_id}/commit`

```json
{"partition": 3, "offset": 123, "generation": 2}
```

`generation` 可省略；携带时会校验。成功：

```json
{
  "ok": true,
  "member_id": "worker-1",
  "partition": 3,
  "committed_offset": 123,
  "previous_offset": 120,
  "generation": 2,
  "duplicate": false,
  "updated_at_ms": 1795000000000
}
```

也支持同一成员批量提交，但每项独立校验：

```json
{"commits": [
  {"partition": 3, "offset": 123, "generation": 2},
  {"partition": 6, "offset": 98, "generation": 2}
]}
```

### `GET /v1/offsets` / `GET /v1/offsets/{partition}`

查询每个分区的已提交值、提交成员、generation、更新时刻、当前 owner 和暂停
状态。未提交分区返回 `committed=false`、`committed_offset=0`。

## 暂停与恢复

### `POST /v1/members/{member_id}/pause`
### `POST /v1/members/{member_id}/resume`

```json
{"partition": 3}
```

暂停后：

- `/fetch` 对该分区返回空；
- commit 返回 `PARTITION_PAUSED`，已提交偏移保持不变；
- 普通重平衡不会改变其归属；
- 事件日志包含 `partition`、`member_id`、`operation_at_ms` 和当时提交偏移。

恢复后从协调器记录的最后提交偏移继续。

## 状态与核对

### `GET /v1/assignments`

返回稳定分配列表、每个活跃成员持有分区和当前 generation。

### `GET /v1/status?event_limit=50`

返回：

- 成员列表、心跳时间、状态、持有/暂停/撤销分区；
- 当前分配；
- 隔离分区及回收原因；
- pending / in-progress 重平衡；
- 最近一次重平衡原因，包括抑制原因；
- 最近事件日志。

### `GET /v1/partitions` / `GET /v1/partitions/{partition}`

每分区 owner、generation、状态、暂停、提交偏移、隔离信息。

### `POST /v1/lag/verify`（`GET /v1/lag` 等价）

核对开始时先快照全部已提交偏移，再查询 Kafka 12 个分区的实际日志末端。
查询 Kafka 期间不持有协调器锁，消费和提交可以继续；结果不会被核对开始后的
新提交改写。

```json
{
  "ok": true,
  "verification_id": "f88f...",
  "topic": "order-events",
  "started_at_ms": 1795000000000,
  "completed_at_ms": 1795000000040,
  "snapshot_generation": 4,
  "total_lag": 0,
  "partitions": [
    {
      "partition": 0,
      "committed_offset": 167,
      "log_end_offset": 167,
      "lag": 0,
      "member_id": "worker-1",
      "paused": false,
      "committed": true,
      "commit_ahead_of_end": false
    }
  ]
}
```

### `GET /v1/events?limit=100`

最近操作日志，含 join/leave、心跳超时、分区回收、提交、暂停/恢复、撤销确认、
重平衡抑制和完成事件。

## 观测

### `GET /healthz`

进程、topic、后端和当前阶段。

### `GET /metrics`

见 README 的指标清单。所有分区指标均带稳定的 `partition` 标签。


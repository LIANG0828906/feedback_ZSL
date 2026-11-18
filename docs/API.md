# API 与状态机

默认监听 `127.0.0.1:8080`，所有请求和响应均为 JSON。

## 查询与客户端拉取

### `GET /v1/config?namespace=payments`

返回当前已发布 head 快照。响应中的 `version` 是快照版本，`digest` 是服务端对完整条目集计算的 SHA-256 摘要。

```json
{
  "namespace": "payments",
  "version": 2,
  "digest": "…",
  "items": [
    {"key": "payment.timeout", "value": "250ms", "version": 7}
  ]
}
```

### `GET /v1/config/pull?instance_id=sim-01&namespace=payments&local_version=2`

服务端首次看到实例时，按实例 ID 的稳定哈希分配灰度或全量环，并持久化环归属。

- 全量环：返回 head 快照及其版本，不包含任何候选配置。
- 灰度环：存在 `canary` 或 `observed_pass` 发布时返回候选快照和 `release_id`；否则返回 head。
- 始终返回完整配置集合，`local_version` 仅作为客户端版本上报。

```json
{
  "namespace": "payments",
  "version": 3,
  "digest": "…",
  "release_id": "rel-…",
  "items": [],
  "instance_id": "sim-01",
  "ring": "canary",
  "client_version": 2,
  "served_at": "2026-11-19T…Z"
}
```

## 配置修改

### `POST /v1/admin/bootstrap`

仅在命名空间为空时原子初始化完整配置。

```json
{
  "namespace": "payments",
  "operator": "release-bot",
  "items": {"timeout": "100ms", "limit": "1000"}
}
```

### `PUT /v1/admin/items`

```json
{
  "namespace": "payments",
  "key": "timeout",
  "value": "250ms",
  "delete": false,
  "expected_version": 1,
  "operator": "release-bot",
  "request_id": "change-001"
}
```

`expected_version=0` 表示新建。并发提交中只有一个事务能匹配当前版本；其余返回 HTTP 409：

```json
{"code": "E_CONFLICT", "message": "version conflict: expected 1, current 2"}
```

有活动发布（`draft/canary/observed_pass`）时，直接修改返回 `E_RELEASE_ACTIVE`，避免推全时覆盖发布基线。

### `GET /v1/admin/items/history?namespace=payments&key=timeout&limit=20`

按版本倒序返回历史。默认保留 20 个版本，最低配置下限为 5。

## 发布

### `POST /v1/releases`

草稿会从当前 head 复制完整快照并应用 `changes`。草稿不改变已发布配置。

```json
{
  "namespace": "payments",
  "operator": "release-bot",
  "observation_seconds": 10,
  "changes": [
    {"key": "timeout", "value": "500ms"},
    {"key": "obsolete.key", "delete": true}
  ]
}
```

### `POST /v1/releases/{id}/transition`

```json
{"target": "canary", "operator": "release-bot", "reason": ""}
```

允许：

- `draft -> canary`
- `canary -> observed_pass`（仅观察器）
- `observed_pass -> full`
- `canary|observed_pass -> rollback`

观察未结束时请求 `full` 返回 `E_OBSERVATION_ACTIVE`。其他非法迁移返回 `E_INVALID_TRANSITION`，状态不变。

### `POST /v1/releases/{id}/report`

仅灰度实例可上报，计数按实例累计值保存，服务端采用每个实例的最大累计值避免重复上报放大。

```json
{
  "instance_id": "sim-01",
  "success_count": 980,
  "error_count": 2,
  "local_version": 3,
  "offline": false
}
```

观察器默认每 100 ms 检查到期发布，使用严格条件 `error_rate > 0.005` 触发自动回滚。回滚不修改 head，因此全量环保持原版本。

### `GET /v1/releases?namespace=payments`

返回发布状态、累计成功/错误计数、错误率、阈值、回滚原因和时间戳。

## 审计和管理

- `GET /v1/admin/audit?namespace=payments&limit=200`
- `GET /v1/admin/clients?ring=canary`
- `POST /v1/admin/clients/{id}/ring`
- `GET /v1/admin/stats`
- `GET /v1/namespaces`
- `GET /healthz`

审计字段包括：`operation`、`namespace`、`key`、`release_id`、`old_digest`、`new_digest`、`operator`、`status_code`、`created_at`、`details`。值摘要为 SHA-256，不记录完整敏感值。

## 业务码

| 业务码 | HTTP | 含义 |
| --- | ---: | --- |
| `E_CONFLICT` | 409 | 配置项版本 CAS 冲突或观察版本不匹配 |
| `E_INVALID_TRANSITION` | 409 | 发布状态迁移非法 |
| `E_OBSERVATION_ACTIVE` | 409 | 观察期未结束，禁止强制推全 |
| `E_STALE_BASE` | 409 | 草稿基线已经不是当前 head |
| `E_RELEASE_ACTIVE` | 409 | 命名空间已有活动发布 |
| `E_NOT_CANARY` | 409 | 非灰度实例试图上报观察数据 |
| `E_NOT_FOUND` | 404 | 命名空间、配置项或发布不存在 |
| `E_INVALID_ARGUMENT` | 400 | 请求参数或 JSON 非法 |
| `E_INTERNAL` | 500 | 内部错误 |

## SQLite 事务边界

- 单项更新：CAS 检查、配置项写入、单项历史、新完整 head 快照、审计在同一事务。
- 全量推进：所有配置项替换、逐项历史、逐项审计、发布迁移审计、head 切换在同一事务。
- 观察回滚：发布状态、回滚原因、迁移审计在同一事务。
- 观察上报：单实例累计计数 upsert 在同一事务。
- 业务失败会回滚业务事务。CAS/非法迁移的失败结果另写一条独立审计记录，供追踪冲突；该记录不包含任何配置或版本写入。

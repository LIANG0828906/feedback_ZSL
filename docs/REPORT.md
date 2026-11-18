# 模拟报告字段

模拟客户端运行结束后原子写入 `-report` 指定的 JSON 文件。实例按 `instance_id` 排序；固定种子和固定 `-poll-count` 时，故障序列和每个实例的拉取次数可复现。

## 顶层字段

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `schema_version` | string | 当前为 `1.0` |
| `started_at` / `finished_at` | RFC3339 string | 运行起止时间 |
| `duration_ms` | int | 实际运行时长 |
| `seed` | int | 故障注入随机种子 |
| `simulation_config` | object | 实例数、轮询间隔、固定轮询次数、故障概率、延迟范围、读取并发等 |
| `instances` | array | 每个实例的最终加载版本、环归属、离线次数等 |
| `canary_observations` | array | 按发布时间正序的灰度观察结果 |
| `latest_canary_observation` | object/null | 最近一次灰度观察 |
| `rollback_count` | int | 已自动或人工回滚的发布数 |
| `offline_fallback_triggers` | int | 所有实例使用本地快照兜底的离线事件数 |
| `read_consistency` | object | 6 并发读取校验结果和延迟分位数 |
| `release_query_error` | string | 拉取发布列表失败时的错误，正常为空 |
| `validation` | object | 实例版本、读取一致性、P95 和摘要校验的汇总 |

## 实例字段

| 字段 | 说明 |
| --- | --- |
| `instance_id` | 稳定实例标识 |
| `ring` | `canary` 或 `full` |
| `final_loaded_version` | 最终本地快照版本 |
| `final_release_id` | 最终接收到的灰度发布 ID；全量无候选时为空 |
| `online` | 最后一次拉取是否在线 |
| `using_snapshot` | 是否存在可用本地快照 |
| `offline_fallback_count` | 使用最近成功快照兜底的离线事件数 |
| `pull_count` | 拉取尝试次数 |
| `successful_pulls` | HTTP 成功且摘要校验通过的拉取次数 |
| `successful_reports` | 成功提交的灰度观察上报数 |
| `digest_failures` | 服务端配置摘要不匹配次数 |
| `last_error` | 最近一次错误，正常为空 |

## 灰度观察字段

包含 `release_id`、`state`、`success_count`、`error_count`、`error_rate`、`error_threshold`、`rolled_back` 和 `rollback_reason`。

阈值判断使用严格大于：错误率 `<= 0.005` 通过观察，`> 0.005` 自动回滚。

## 读取一致性字段

| 字段 | 说明 |
| --- | --- |
| `workers` | 并发探测 worker 数，端到端场景为 6 |
| `requests` | 完成的查询请求数 |
| `http_failures` | 查询失败数 |
| `digest_mismatches` | 返回条目重算摘要与响应摘要不一致次数 |
| `duplicate_key_views` | 同一响应内重复 key 次数 |
| `invalid_versions` | 非正版本号次数 |
| `item_count_mismatches` | 低于 `-read-min-items` 的响应数 |
| `p95_ms` / `max_ms` | 完整 HTTP 查询延迟 |
| `p95_within_100ms` | P95 是否不超过 100 ms |
| `passed` | 无错误、无摘要/重复/invalid 视图，且 P95 达标 |

## 结果复现

```bash
./bin/simclient \
  -server http://127.0.0.1:8080 \
  -namespace payments \
  -instances 24 \
  -poll 2s \
  -poll-count 25 \
  -duration 120s \
  -seed 263 \
  -fault-5xx-rate 0.03 \
  -network-error-rate 0.02 \
  -latency-probability 0.30 \
  -report reports/simulation-report.json
```

`-poll-count` 固定每个实例的拉取次数，避免注入延迟导致总拉取次数漂移。`-duration` 仅作为安全截止时间。

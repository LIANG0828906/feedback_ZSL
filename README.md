# 租户日志配额与背压协调服务

本工作区实现了一个位于 Vector 与 Redis 之间的租户级日志配额协调器，并提供可靠写入器、确定性 OTLP/HTTP 负载生成器、故障注入和结果核对脚本。服务使用 Go 1.22，Redis 7.2 List 作为可靠队列，Vector 0.38 负责接收 OTLP/HTTP 日志并转发到协调器。

## 组件

```text
deterministic loadgen
  -> Vector 0.38 OTLP/HTTP source (:4318)
  -> tenant-routed HTTP sinks (NDJSON)
  -> coordinator (:8080)
       - per-tenant fixed-window record/byte quota
       - global backpressure (HTTP 429 + Retry-After for Vector)
       - per-tenant timeout-window degradation
       - reliable Redis queue and receipt/cursor state
  -> Redis 7.2 per-tenant lists
  -> writer
       - JSONL append + fsync before ACK
       - replay unconfirmed work after restart
       - batch-id dedup when rebuilding from the output file
```

同一 Vector 实例把六个租户分别路由到六个 HTTP sink。这样某个租户的配额拒绝不会让 Vector 把其他租户的同一 HTTP 请求整体丢弃。

## 快速开始

依赖：

- Go 1.22+
- Vector 0.38 官方二进制
- Redis 7.2（Windows 兼容发行版或 WSL2）
- Python 3.10+（仅场景编排与报告）

Linux / WSL2：

```bash
make build
make test
make scenario
```

如果二进制不在 `PATH`：

```bash
export GO_BIN=/path/to/go
export REDIS_SERVER_BIN=/path/to/redis-server
export VECTOR_BIN=/path/to/vector
python3 scripts/run_scenario.py
```

不启动 Vector、直接向协调器发送 OTLP/HTTP：

```bash
python3 scripts/run_scenario.py --direct
```

默认报告写入 `artifacts/report.json`，运行日志、配置快照、指标快照和写入结果位于 `run/scenario-*`。

## 配置

`config/coordinator.json` 定义：

- 六个租户的 `logs_per_second`、`bytes_per_second`、丢弃策略和降级队列上限。
- 背压进入/退出队列深度、限流比例、轮询周期和退出保持时间。
- 写入超时、超时统计窗口、降级阈值、恢复所需 ACK 样本数和恢复静默期。

`config/vector.yaml` 定义 OTLP source、按租户的 route 转换和六个 HTTP sink。默认端口是 `4318` 与 `8080`；场景运行器会改写为空闲端口，避免本地端口冲突。

`config/scenario.json` 是默认确定性认证场景。它使用固定种子、固定批次计划、固定请求顺序，并注入：

- `tenant-06` 单租户突发。
- 下游消费者暂停。
- Redis 写入延迟。
- 运行中的 `tenant-01` 配额变更。

生成器同时保留基于 `tenant_rates` 的持续速率模式；认证报告使用固定计划模式，以避免 Vector 调度和重试时序造成计数抖动。题面允许降低样本量和批大小。

## HTTP API

### 日志接入

`POST /v1/logs`，别名 `POST /ingest`。

支持：

- OTLP/HTTP protobuf：`Content-Type: application/x-protobuf`。
- OTLP JSON：`resourceLogs[].scopeLogs[].logRecords[]`。
- Vector JSON sink 的 NDJSON 事件。
- `Content-Encoding: gzip`。

租户字段支持 `tenant.id`、`tenant_id`、`tenantId`、`tenant`。批次和记录字段支持 `batch.id`、`batch_id`、`record.id`、`record_id` 等。也可通过 `X-Batch-ID` 或 `Idempotency-Key` 传入批次 ID。

### 配额

- `GET /v1/tenants/{tenant}/quota`
- `PUT /v1/tenants/{tenant}/quota`
- `PATCH /v1/tenants/{tenant}/quota`

更新示例：

```json
{"logs_per_second": 400, "bytes_per_second": 4194304}
```

更新在持有状态锁的下一次准入判定中生效。已接收批次、待写字节和既有队列不受追溯修改。非法值返回 `INVALID_QUOTA`，原配额保持不变。

### 状态与确认

- `GET /status`、`GET /v1/status`：固定字段顺序、按租户 ID 排序，不含绝对路径。
- `GET /v1/cursors`：所有租户的消费游标。
- `GET /v1/tenants/{tenant}/cursor`：单租户游标。
- `GET /v1/receipts/{tenant}/{batch_id}`：批次回执；`acknowledged=true` 仅在写入器完成 JSONL 写入与 `fsync` 后设置。
- `POST /v1/ack`：写入器确认接口。
- `GET /metrics`：Prometheus 文本指标。
- `GET /health`：协调器和 Redis 健康状态。

确认请求：

```json
{
  "tenant_id": "tenant-01",
  "batch_id": "batch-123",
  "seq": 12,
  "storage_offset": 4096
}
```

### 故障控制

- `POST /v1/admin/fault`：`{"redis_delay_ms": 50}`。
- 写入器控制端口：`POST /admin/pause`、`POST /admin/fault`，支持消费者暂停、租户延迟和 ACK 延迟。

管理接口默认仅绑定本机。生产部署应关闭 `admin_enabled` 或置于本机管理网络。

## 稳定业务码

| 业务码 | 典型 HTTP 状态 | 含义 |
| --- | ---: | --- |
| `ACCEPTED` | 200 | 全部记录入队 |
| `DUPLICATE_ACCEPTED` | 200 | 幂等重试，未重复入队 |
| `PARTIAL_QUOTA_DROP` | 207 | 按 `drop` 策略接受配额内前缀并丢弃超额部分 |
| `TENANT_QUOTA_EXCEEDED` | 422 | 租户每秒条数或字节配额不足 |
| `GLOBAL_BACKPRESSURE` | 429 | Redis 积压触发全局背压；包含 `Retry-After` |
| `TENANT_DEGRADED` | 422 | 仅该租户进入有限队列并拒绝新批次 |
| `UNKNOWN_TENANT` | 404 | 租户未配置 |
| `INVALID_BATCH` | 400 | 批次格式或内容非法 |
| `INVALID_QUOTA` | 400 | 配额更新非法 |
| `INVALID_TENANT` | 400 | 日志记录缺少租户标识 |
| `REDIS_UNAVAILABLE` | 503 | Redis 不可用 |

全局背压使用 `429`、`Retry-After`、`X-Vector-Backpressure: true` 和 `X-Backpressure-Ratio`，Vector HTTP sink 原生重试。租户独占错误使用 `422`，避免 Vector 对单个租户的确定性丢弃进行无界重试。

## 配额、背压与降级语义

- 配额按 UTC 整秒固定窗口统计；请求中的条数和规范化字节数都不得超过剩余额度。
- 每个租户独立维护窗口；一个租户超限不会消耗其他租户额度。
- 背压进入阈值为队列中的 `queue + processing` 批次数。进入后每个租户有效配额乘以 `backpressure.ratio`。
- 积压降到退出阈值以下并持续 `exit_hold_ms` 后自动退出。退出时当前秒窗口被置满，下一整秒前不接纳新批次，避免恢复瞬间冲高。
- 未确认批次超过 `write_timeout_ms` 后计入该租户的滚动超时窗口。窗口内样本达到 `timeout_batches_threshold` 时只降级该租户。
- 降级记录包含触发指标、触发值、阈值、窗口样本量和时间。恢复必须同时满足：来自降级期间实际 ACK 的样本数达到阈值、静默期内无新超时、待写批次回落到有限队列上限以内。

## Redis 键

默认前缀 `lq:`，场景运行器使用独立 Redis 实例和前缀。

- `lq:queue:<tenant>`：待消费批次 List。
- `lq:processing:<tenant>`：已取出、未确认批次 List。
- `lq:receipt:<tenant>:<hash(batch)>`：批次回执 Hash。
- `lq:cursor:<tenant>`：已确认游标 Hash。
- `lq:seen:<tenant>`：批次幂等记录 Hash。
- `lq:seq:<tenant>`：租户内单调批次序号。
- `lq:control:*`：运行期控制状态。

入队脚本原子写入幂等记录、队列和回执。协调器重启时从 Redis 恢复待写集合；写入器重启时把 `processing` 重新放回 `queue`。输出文件本身是最终去重依据：启动时扫描所有 `_batch_id`，重投批次只确认、不重复写入。

## Prometheus 指标

所有租户指标带 `tenant` 标签，租户顺序固定：

- `log_quota_ingested_records_total`
- `log_quota_ingested_bytes_total`
- `log_quota_dropped_records_total`
- `log_quota_backpressure_rejected_batches_total`
- `log_quota_write_timeouts_total`
- `log_quota_pending_write_bytes`
- `log_quota_backlog_depth`
- `log_quota_consume_records_per_second`
- `log_quota_consume_bytes_per_second`
- `log_quota_degraded`
- `log_quota_quota_records_per_second`
- `log_quota_quota_bytes_per_second`
- `log_quota_window_records`
- `log_quota_window_bytes`

全局指标包含 `log_quota_backpressure_active`、`log_quota_backpressure_ratio`、`log_quota_redis_backlog_depth` 和各租户总和。

## 验证

完整场景：

```bash
python3 scripts/run_scenario.py --report artifacts/report.json
```

报告核对项：

- 背压至少触发一次。
- 仅有突发的 `tenant-06` 被降级并恢复。
- 全部租户待写队列归零。
- 每个租户 `offered = ingested + dropped`。
- 已入队记录数等于最终 JSONL 行数。
- 最终输出无重复记录。
- 每个租户都有确认游标。

确定性对比：

```bash
python3 scripts/run_scenario.py --report run/report-a.json
python3 scripts/run_scenario.py --report run/report-b.json
python3 scripts/verify_report_determinism.py run/report-a.json run/report-b.json
```

除时间戳和耗时字段外，固定场景的计数、降级租户、恢复租户和最终落盘结果一致。

协调器与写入器崩溃恢复测试：

```bash
python3 scripts/run_restart_test.py
```

该测试在写入器已经 `fsync`、但 ACK 尚未发出的窗口内同时杀掉协调器和写入器。重启后核验：未确认批次被重投、重投批次被去重、游标恢复、最终每个记录只出现一次。报告写入 `artifacts/restart-report.json`。

## 资源约束

- 场景运行器为 Go 组件设置 `GOMAXPROCS=2`，Vector 使用 `--threads 2`。
- 默认认证场景只处理约 4.4k 条日志；Vector 内存缓冲、Redis 队列和 JSONL 输出远低于 1.25 GiB / 2 GB 限制。
- 需要更高流量时，可移除 `config/scenario.json` 中的 `plan`，恢复 `tenant_rates` 持续速率模式，并降低运行时长或批大小以适配资源。

## Windows 说明

Go 服务和 Python 场景脚本可在 Windows 原生运行；Redis 可使用 Windows 兼容发行版，或按题面在 WSL2 中启动。Vector 使用官方 Windows 二进制。若通过 WSL2 运行 Redis，将 `config/coordinator.json` 中的 Redis 地址改为 WSL2 可达地址，并确保 Vector/协调器均可访问。

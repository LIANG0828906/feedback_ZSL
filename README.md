# Order Guard

面向 Windows 单机 SQLite 零售订单服务的本地保护与容量决策控制面。项目从零实现配置化 SLI/SLO、租户入口限流与并发闸门、数据库读路径熔断、缓存/精简字段降级、资源耗尽保护、原子审计、Prometheus 指标、容量报告和可重复故障场景。

运行时不访问外部网络，不依赖云服务或容器。主服务使用 FastAPI + Uvicorn，数据库使用 Python 标准库 `sqlite3`。

## 1. 快速启动

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
pip install -r requirements.txt
python -m order_guard --config config\service.json
```

服务默认监听 `127.0.0.1:8000`。健康检查不进入业务线程池，因此在业务线程被慢查询占满时仍可响应：

```powershell
Invoke-RestMethod http://127.0.0.1:8000/healthz

Invoke-RestMethod `
  -Uri "http://127.0.0.1:8000/api/v1/orders?status=created&page=1&page_size=20" `
  -Headers @{ "X-Tenant-Id" = "tenant-alpha" }
```

配置文件中的相对路径以配置文件所在目录为基准。默认数据库位于 `data/order_guard.db`，首次启动会创建订单表、索引、审计表，并确定性地生成租户订单数据。

## 2. 目录结构

```text
config/service.json         默认服务、SLO、租户、熔断与容量配置
order_guard/
  config.py                 严格配置解析与字段级校验
  app.py                    FastAPI 路由、入口准入、管理接口
  service.py                查询/写入业务与降级链
  control.py                保护原因合并、状态机与原子审计发布
  sli.py                    滑出窗口 SLI、P99 与错误预算
  limiter.py                每租户令牌桶和在途并发闸门
  circuit.py                连续失败/超时比例熔断与半开探测
  database.py               SQLite WAL、连接闸门、确定性数据
  faults.py                 运行时慢查询/不可达/SQLite 忙注入
  capacity_report.py        可复核的连接数与页大小建议
  monitor.py                SLI 求值与资源耗尽采样线程
  metrics.py                Prometheus 文本指标注册表
  audit.py                  追加式 SQLite 审计表
scenarios/fault_mix.json    慢查询 + 短暂不可达 + 恢复流量
scenarios/tenant_isolation.json  单租户持续冲击隔离场景
scripts/run_scenario.py     固定速率混合读写、故障注入与结果汇总
scripts/query_audit.py      独立查询一次运行的完整审计序列
locustfile.py               Locust 混合读写负载入口
tests/                      单元与真实进程集成测试
```

## 3. 配置模型

默认配置位于 `config/service.json`。核心字段：

- `slis.availability`：成功响应占比滑窗，`target` 为目标成功率和 `degrade_level` 为预算耗尽后的降级等级。
- `slis.latency`：`target_ms` 为目标 P99，`allowed_bad_ratio` 为允许的慢请求比例，实际 P99 和慢请求预算双重判断。
- `tenants[]`：每租户 `rate_per_second`、`burst`、`max_inflight` 和 `enabled`。
- `degradation.levels`：等级到查询模式的映射，支持 `full`、`cache_first`、`slim`，并声明页大小上限。
- `circuit`：连续失败阈值、超时比例阈值、统计窗口、打开时长和半开探测数。
- `capacity`：工作线程占用、积压深度、SQLite 忙事件阈值，以及进入/恢复的连续采样次数。
- `fallback.order`：熔断期降级顺序，支持 `cache` 与 `slim_snapshot`。
- `query`：默认页大小和全量页大小上限。

配置非法时进程拒绝启动，并以点路径定位问题，例如：

```text
invalid configuration C:\...\service.json:
  - $.tenants[0].rate_per_second: expected number >= 0.001
  - $.slis.latency.degrade_level: no matching $.degradation.levels.9
```

未知配置键也会被拒绝，避免字段拼写错误静默生效。

### SLI 与错误预算

两项 SLI 都使用单调时钟和滑出窗口：

- 可用率：`availability = success / samples`。
- 可用率预算消耗：`(1 - availability) / (1 - target)`。
- P99：窗口内最近秩 P99。
- 延迟预算消耗：`(latency > target_ms) 的请求比例 / allowed_bad_ratio`。
- 剩余预算：`max(0, 1 - consumed)`。

预算耗尽且样本数达到 `min_samples` 后进入对应降级等级。恢复采用滞回：预算消耗低于 `recover_budget_ratio`，且 P99 回到目标内；没有样本时也视为恢复。保护和恢复都写审计。

## 4. 保护状态机

保护原因包含四类：

```text
sli:availability
sli:latency
circuit:orders_db
capacity:local
```

有效降级等级取所有原因的最高等级。容量保护会暂停 `POST` 和 `PATCH` 写入，并把查询页大小上限收窄到 `capacity.page_size_cap`。原因解除后等级自动下降，最终回到 `normal`，无需重启。

### 租户准入

每个租户有独立令牌桶和独立在途并发计数。限流请求在业务处理前直接返回：

- HTTP `429` + `TENANT_RATE_LIMITED`；
- HTTP `429` + `TENANT_CONCURRENCY_LIMITED`；
- `Retry-After` 秒数及 `details.retry_after_ms`；
- 未启用或未知租户返回 HTTP `403` + `TENANT_NOT_ACTIVE`，不占用业务并发。

`/healthz`、`/readyz`、`/metrics`、`/admin/*` 不消耗业务额度。

### 读路径熔断

查询数据库出现以下情况时进入熔断判定：

- 连续失败达到 `failure_threshold`；
- 窗口内超时比例达到 `timeout_ratio_threshold`，且样本数达到 `timeout_min_requests`。

熔断打开后不再直接访问数据库，按 `fallback.order` 返回：

1. 精确查询缓存；
2. 启动时加载的内存精简快照，仅返回 `order_id`、`status`、`created_at`。

冷却结束后只放行半开探测。探测成功关闭熔断；探测失败重新打开，并再次记录降级等级和原因。

### 容量保护

后台线程按 `sample_interval_ms` 采样：

- 业务工作线程占用：在途请求数 / `worker_threads`；
- 请求积压：在途请求数 - 正在执行 SQLite 的查询数；
- SQLite `database is locked` 事件增量。

任一信号达到阈值并连续满足 `enter_after_samples` 后进入容量保护。所有信号回落到恢复阈值并连续满足 `recover_after_samples` 后自动退出。进入、退出都输出日志、审计事件和 `order_guard_capacity_protected` 指标。

## 5. HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 轻量健康检查，不访问 SQLite、不消耗租户额度 |
| GET | `/readyz` | 就绪状态，降级时仍返回 ready |
| GET | `/metrics` | Prometheus 文本格式 |
| GET | `/api/v1/orders?status=&page=&page_size=` | 租户分页查询 |
| POST | `/api/v1/orders` | 下单 |
| PATCH | `/api/v1/orders/{order_id}/status` | 状态流转 |
| GET | `/admin/status` | 保护状态、SLI、租户桶、熔断、容量采样概览 |
| GET | `/admin/slis` | 当前滑窗 SLI 明细 |
| GET | `/admin/audit?run_id=&after_seq=&limit=` | 独立查询审计序列 |
| GET | `/admin/capacity-report?scenario=` | JSON 容量报告 |
| POST | `/admin/faults` | 运行时故障注入 |
| DELETE | `/admin/faults` | 清除故障 |
| PATCH | `/admin/tenants/{tenant_id}/limits` | 调整租户限流参数并写审计 |

管理接口使用 `X-Admin-Token`。业务请求使用 `X-Tenant-Id`。

稳定业务码通过 `X-Business-Code`、`X-Protection-Code` 和 JSON 字段返回。常见值：

```text
OK
TENANT_RATE_LIMITED
TENANT_CONCURRENCY_LIMITED
TENANT_NOT_ACTIVE
ORDERS_DB_CIRCUIT_OPEN
DATABASE_DEGRADED
DATABASE_TIMEOUT
CAPACITY_PROTECTED
SLO_AVAILABILITY_BUDGET_EXHAUSTED
SLO_LATENCY_BUDGET_EXHAUSTED
```

## 6. 故障注入

故障注入只改运行时状态，不修改服务源码。`slow` 对数据库操作增加延迟，`unavailable` 抛出短暂不可达错误，`busy` 模拟 SQLite 忙事件：

```powershell
$headers = @{
  "X-Admin-Token" = "local-admin-token"
  "Content-Type" = "application/json"
}

Invoke-RestMethod -Method Post `
  -Uri http://127.0.0.1:8000/admin/faults `
  -Headers $headers `
  -Body '{"mode":"slow","delay_ms":900,"duration_seconds":8}'

Invoke-RestMethod -Method Delete `
  -Uri http://127.0.0.1:8000/admin/faults `
  -Headers @{ "X-Admin-Token" = "local-admin-token" }
```

`duration_seconds` 到期自动清除，用于模拟短时依赖故障。

## 7. 负载与场景结果

场景器按 `traffic_profiles` 固定速率调度混合读写，并复用每线程 HTTP 长连接，避免负载工具自身耗尽临时端口。

```powershell
python scripts\run_scenario.py scenarios\fault_mix.json
python scripts\run_scenario.py scenarios\tenant_isolation.json
```

默认由场景器启动独立服务进程并在结束后关闭。若服务已在运行：

```powershell
python scripts\run_scenario.py scenarios\fault_mix.json `
  --no-start-service --base-url http://127.0.0.1:8000
```

输出：

```text
artifacts/scenario-<name>.json
artifacts/capacity-<name>.json
artifacts/service-<name>.log
```

场景结果固定包含：

- `traffic`：计划/完成请求数、生成器滞后数、流量阶段；
- `tenants`：每租户请求数、成功率、限流分类、fallback 数、总延迟与读延迟 P50/P95/P99/max、业务码；
- `protection`：限流总数与原因、熔断打开次数、fallback 数、容量进入次数、降级/容量/熔断持续秒数、最终状态；
- `error_budget`：各 SLI 最终值和本场景峰值预算消耗；
- `faults`：故障注入时间、请求参数和实际状态；
- `audit`：事件数、事件类型计数和完整的有序事件摘要；
- `capacity_report`：本次场景容量建议及推导输入。

同一场景重复运行会得到一致的结果字段树；时间戳和具体耗时数值不参与结构一致性判断。完整审计可通过 API 或脚本独立复原：

```powershell
python scripts\query_audit.py data\order_guard.db `
  --run-id scenario-fault-mix-20261119T004803Z-332015
```

也可直接查询 SQLite：

```sql
SELECT seq, event_type, business_code, reason, created_at
FROM audit_events
WHERE run_id = 'scenario-fault-mix-20261119T004803Z-332015'
ORDER BY seq;
```

## 8. 审计一致性

`audit_events` 包含：

```text
seq, run_id, event_type, tenant_id, from_state, to_state,
business_code, reason, metrics_snapshot_json, thresholds_json,
sli_snapshot_json, decision_json, created_at
```

控制面修改内存状态前，先在 `BEGIN IMMEDIATE` 中提交同一次切换的全部审计事件；只有提交成功才发布新状态。审计失败时状态保持原值。熔断器在审计失败时回滚到切换前状态。租户限流参数也先审计、后应用。

## 9. Prometheus 指标

稳定指标名：

```text
order_guard_rate_limit_rejections_total{tenant,reason}
order_guard_inflight_requests{tenant}
order_guard_inflight_total
order_guard_backlog_depth
order_guard_worker_thread_occupancy_ratio
order_guard_circuit_state{circuit="orders_db"}        # 0/1/2
order_guard_degrade_level
order_guard_capacity_protected
order_guard_sqlite_busy_events_total
order_guard_sqlite_operations_total
order_guard_sli_value{sli="availability|latency"}
order_guard_sli_samples{sli}
order_guard_error_budget_consumed_ratio{sli}
order_guard_error_budget_remaining_ratio{sli}
order_guard_error_budget_peak_consumed_ratio{sli}
order_guard_fallback_total{source,reason}
order_guard_circuit_transitions_total{to}
order_guard_capacity_transitions_total{to}
order_guard_writes_rejected_total{reason}
```

必需指标的零值序列在启动时初始化，不会因为尚未发生事件而消失。

## 10. 容量报告

报告使用本次运行指标快照计算，`formula_version=capacity-v1`：

```text
latency_pressure = min(1, latency_target_ms / peak_p99_latency_ms)
busy_ratio       = sqlite_busy_events_total / sqlite_operations_total
busy_penalty     = max(0.5, 1 - busy_ratio)
pressure         = max(0.05, latency_pressure * busy_penalty)

connection_limit = clamp(floor(current_connection_limit * pressure), 1, current)
page_size_limit  = clamp(floor(current_page_size_limit * pressure), 10, current)
```

`inputs` 保存参与计算的全部指标，`derivation` 保存中间值、公式版本和上下界。相同指标快照会得到相同建议。

## 11. 测试

```powershell
pytest -q
```

测试覆盖：

- 配置字段定位与拒绝启动；
- 可用率/P99 滑窗、预算和恢复；
- 租户限流/并发隔离；
- 熔断连续失败、半开恢复和探测失败重开；
- 审计失败不发布状态；
- 真实 Uvicorn + SQLite 进程下的限流、熔断 fallback、容量保护进入/退出、写入暂停、SLI 恢复和指标契约；
- 场景结果字段结构可重复。

本地验证结果：

- `fault-mix`：558 请求，22 次并发闸门拒绝，3 次熔断打开事件，容量保护进入/退出各 1 次，峰值延迟预算消耗 8.00，最终 `degrade_level=0`、熔断 `closed`、容量保护关闭。
- `tenant-isolation`：600 请求，攻击租户 243 次限流拒绝；正常租户无 429、无传输错误，P99 32.61ms，仍显著低于 350ms 延迟 SLO。

## 12. Locust 可选入口

场景器是固定速率和结果汇总的主入口。若需要交互式压测，也可运行：

```powershell
$env:ORDER_GUARD_TENANTS = "tenant-alpha,tenant-beta,tenant-gamma"
locust -f locustfile.py --host http://127.0.0.1:8000
```

## 13. 资源与隔离

- 单服务进程、单场景器进程，无容器。
- SQLite 通过 `connection_limit` 限制并发操作，WAL 模式降低读写互斥。
- 内存精简快照按租户上限加载，默认每租户 1000 行。
- 场景器限制 `max_workers` 与 `max_pending`，默认远低于 1.25GiB 内存预算。
- 需要多实例时，为每个实例使用独立配置文件、数据库路径、端口和 `ORDER_GUARD_RUN_ID`；场景器可通过 `--base-url` 连接指定实例。

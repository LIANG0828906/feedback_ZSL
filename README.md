# DualWrite：华东/华南 PostgreSQL 双活收敛服务

本项目在单台主机上的两个 PostgreSQL 16 实例（`55432` 华东、`55433` 华南）之间做行级双向同步。服务分别处理两类字段：

- `members.points`：可累加字段，按“对端当前值 + 本次增量”合并。
- `orders.status`：互斥字段，按 `(row_version, site_priority)` 决胜；同版本时固定站点优先级。

每个变更都有稳定全局键 `source_site:source_change_id`。对端使用该键做幂等应用，并在追溯中保留原始投递、重复投递、冲突、每列判定依据与最终采用值。

## 目录

- `dualwrite/schema.py`：表、row version trigger、变更流和幂等接收表。
- `dualwrite/sync.py`：双向拉取、增量合并、状态决胜、过期识别、去重应用。
- `dualwrite/audit.py`：SQLite 审计、游标、冲突、批次快照和追溯存储。
- `dualwrite/validation.py`：允许并发写入的全量收敛校验。
- `dualwrite/observability.py`：Prometheus 文本指标和状态缓存。
- `dualwrite/http_api.py`：JSON 状态、追溯、冲突和校验接口。
- `scripts/load_and_verify.py`：双向负载、收敛等待、全量校验和时延验证。

## 快速启动

Python 3.11+：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
```

修改 `config.example.json` 中两个 DSN，或在 PowerShell 中用环境变量覆盖，避免把密码写入文件：

```powershell
$env:DUALWRITE_EAST_DSN="host=127.0.0.1 port=55432 dbname=postgres user=postgres password=你的密码 connect_timeout=2"
$env:DUALWRITE_SOUTH_DSN="host=127.0.0.1 port=55433 dbname=postgres user=postgres password=你的密码 connect_timeout=2"
$env:DUALWRITE_SQLITE_PATH="var/dualwrite.sqlite3"
```

初始化固定种子数据并启动。全局参数放在子命令之前：

```powershell
.\.venv\Scripts\python -m dualwrite --config config.example.json init --members 20000 --orders 30000 --reset
.\.venv\Scripts\python -m dualwrite --config config.example.json serve
```

默认 API 为 `http://127.0.0.1:8090`。初始化脚本在两实例写入相同的 20,000 个会员和 30,000 个订单，种子公式固定；业务行初始 `row_version=1`。

`--reset` 会清空两张业务表、同步侧表以及 SQLite 审计/游标数据，适用于重新开始一轮固定种子试验。若只需要保留审计而重置业务数据，应改用新的 SQLite 文件并手动处理同步表。

## 数据模型

```sql
members(member_id bigint primary key, points bigint, row_version bigint, updated_at timestamptz)
orders(order_id bigint primary key, status text, row_version bigint, updated_at timestamptz)
```

业务写入无需手工写变更流。`BEFORE INSERT OR UPDATE` trigger 会：

1. 本地更新时推进 `row_version`；若调用方显式给出更大的版本则保留。
2. 将旧行、新行、积分增量或状态变化写入 `sync_change`。
3. 在 `sync_row_state` 记录每类字段的当前版本和最后贡献站点。

同步端应用远端变更时，在事务内设置 `app.sync_apply=on`，因此不会再次生成变更流，避免回声。

## 合并规则

### 积分

每条去重后的远端积分变更执行：

```text
adopted_points = target_current_points + incoming_change.points_delta
```

重复投递由对端 `sync_applied.change_key` 唯一键拦截，不会重复加积分。乱序到达的积分增量仍只应用一次；如果其行版本较旧，会记录为 `partial_stale`，但不会整行覆盖对端更新值。

### 订单状态

状态候选值为 `(row_version, site_priority)`，比较规则为：

```text
更大 row_version 胜出
row_version 相同时，site_priority 更大者胜出
```

`config.example.json` 中 `east.priority=20`、`south.priority=10`，因此同版本固定由华东胜出。较旧或同版本低优先级的状态不会覆盖当前状态，并记入冲突/过期追溯。

每条已应用事件在 `sync_apply_events.field_decisions` 中分别记录字段级依据，例如：

- `incremental_merge`
- `incremental_merge_overlap`
- `incoming_row_version_newer`
- `equal_version_site_priority`
- `equal_version_lower_site_priority`

## 提交与恢复边界

一次远端应用包含：

1. 对端 PostgreSQL 事务：插入幂等键 `sync_applied`、更新业务行、写 `sync_apply_events`，三者同事务提交。
2. SQLite 事务：镜像已应用事件、字段最终依据、冲突记录、投递记录，并推进源游标确认。
3. 源游标只在 SQLite 审计镜像成功后推进。

若进程在 PostgreSQL 提交后、SQLite 提交前退出，源变更会被再次读取；对端 `sync_applied` 返回已有事件，服务补写 SQLite 后确认，不会重复修改业务数据。游标使用连续确认；乱序完成的变更进入 `sync_ack_gap`，缺口补齐后才推进到连续位置。

SQLite 主要表：

- `deliveries`：每次投递，包含重复投递和最终应用/丢弃结果。
- `applied_changes`：每个全局变更键只出现一次。
- `conflicts`：按行、列记录双方值、版本、采用值和判定依据。
- `row_field_state`：当前字段最终值的最后变更键与依据。
- `validation_batches`、`validation_snapshot`、`validation_mismatches`：批次结论与不一致明细。

## 全量收敛校验

`POST /v1/validate` 异步创建校验批次。校验开始时分别对两实例开启 `REPEATABLE READ READ ONLY` 快照，将两侧会员积分和订单状态写入 SQLite 暂存区，然后关闭数据库快照并比较暂存数据。校验期间业务写入和同步照常进行。

结果分类：

- `unconverged`：差异行对应源站仍有未确认变更，属于尚未收敛。
- `true_conflict`：没有待应用变更可以解释该差异。

每条不一致包含行类型、行号、列名、华东值、华南值和判定依据。已完成批次不可覆盖；进程中断时，同一 `batch_id` 会在重启后继续（若中断发生在快照收集阶段则重新收集该批快照，若已进入比较阶段则从快照继续）。

创建并查询：

```powershell
curl.exe -X POST -H "Content-Type: application/json" -d "{\"scope\":\"all\"}" http://127.0.0.1:8090/v1/validate
curl.exe "http://127.0.0.1:8090/v1/validations/批次ID?limit=1000"
```

## 查询接口

所有 JSON 使用固定键排序。

```text
GET /healthz
GET /metrics
GET /v1/status
GET /v1/trace/member/{member_id}?limit=30
GET /v1/trace/order/{order_id}?limit=30
GET /v1/trace?row_type=members&row_id=1&limit=30
GET /v1/conflicts?table=members&row_id=1&from=2026-01-01T00:00:00Z&to=2026-12-31T23:59:59Z
GET /v1/validations/latest?limit=1000
GET /v1/validations/{batch_id}?limit=1000&category=unconverged
POST /v1/validate
```

追溯响应同时返回：

- 两端当前完整业务行。
- 已应用变更及来源站点、行版本、积分增量/状态采用值。
- 两端本地变更流（含尚未应用的行）。
- 冲突决策、最终字段依据和重复投递历史。

## Prometheus 指标

站点标签固定为 `site="east"` 和 `site="south"`：

```text
sync_applied_changes_total
sync_stale_changes_total
sync_conflicts_total
sync_points_merged_total
sync_status_overridden_total
sync_pending_convergence_rows
sync_confirmed_change_id
sync_source_outbox_change_id
sync_connections_open
sync_scrape_timestamp_seconds
```

`sync_pending_convergence_rows{site=...}` 表示发往该站点、尚未确认的不同业务行数。

## 连接上限与错误码

`limits.max_connections` 是服务 PostgreSQL 连接总预算，按两个站点均分。API 获取连接只等待 `api_acquire_timeout_ms`；超限立即返回 HTTP 503：

```json
{"code":"E_CONN","message":"no database connection available ...","ok":false}
```

同步线程的显式超时由 `sync_acquire_timeout_ms` 控制，不会无限等待。

## 链路故障注入

`config.injection.example.json` 默认开启：

- 10–800 ms 延迟。
- 250 ms 重排窗口。
- 10% 重复投递。
- 启动 10 秒后中断 60 秒。

运行：

```powershell
.\.venv\Scripts\python -m dualwrite --config config.injection.example.json serve
.\.venv\Scripts\python scripts/load_and_verify.py --config config.injection.example.json --rate 30 --duration 70
```

`load_and_verify.py` 会记录写入 P50/P95、等待待收敛行归零、调用全量校验、检查积分总和和追溯时延，并输出 `artifacts/load_report.json`。

## 本地验证

无数据库的单元测试：

```powershell
.\.venv\Scripts\python -m unittest discover -s tests -v
```

双实例端到端：

```powershell
.\.venv\Scripts\python -m dualwrite --config config.example.json init --reset
.\.venv\Scripts\python -m dualwrite --config config.example.json serve
# 另开终端
.\.venv\Scripts\python scripts/load_and_verify.py --config config.example.json --rate 30 --duration 10
```

在本仓库的双实例、20,000 会员、30,000 订单负载验证中，30 次/秒双向变更、60 秒中断和乱序/重复注入后均达到：

- 写入 P95 约 3 ms。
- 中断恢复后待收敛行归零。
- 全量校验 `mismatch_count=0`。
- 单行追溯 P95 约 10 ms。
- 连接池耗尽稳定返回 `E_CONN`。

测试环境的 PostgreSQL 使用 `fsync=off` 仅用于加速沙箱验证；生产环境不要关闭持久化设置。

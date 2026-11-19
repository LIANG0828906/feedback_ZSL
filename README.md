# 库存扣减在线分批迁移服务

面向单台 Windows 11 / 2 核 / 1.25 GiB 主机的 .NET 8 实现。服务把
`inventory_old`（55432）中的 5 万行库存和 12 万行流水按商品 ID 分批迁移到
`inventory_new`（55433），并在旧库持续产生扣减、回补时提供可续传、可校验、
不双写的读写路由。

## 核心保证

- **可续传**：SQLite 账本保存连续的商品范围、批次状态、旧库开始/最终 snapshot、
  outbox cutoff 和校验报告。进程中断后从最后一个 `committed` 范围继续，当前未提交
  批次原地重做。新库写入使用幂等 upsert / `ON CONFLICT`，不会重复流水。
- **单一可见性边界**：每个批次先固定旧库起始库存和 outbox 序号；最终切换时获取
  `inventory` 表的短时 `SHARE ROW EXCLUSIVE` 锁，复制旧库最终库存与流水到新库，
  在新库事务提交并完成校验后，才提交 SQLite 的 committed 路由边界。
- **路由不双写**：普通扣减/回补只写一个库。批次进入 `migrating` 后，该范围返回
  `E_MIGRATING`；最终切换期间的请求等待 `WriteGate` 完成后再按新账本路由。路由只读
  SQLite 中已提交的范围，回滚商品优先生效。
- **CDC 安全网**：旧库 `deduction_flow` 上的触发器把旁路旧库变更写入
  `migration_outbox`。迁移批次内的变更通过最终库存对齐生效；批次 cutoff 之后的旁路
  变更由后台捕获线程按 `seq` 幂等补偿到新库，新库用 `migration_applied_event` 去重。
- **校验**：`旧库起始库存 + (起始序号, cutoff] 内捕获的 signed change_qty` 对比新库
  当前库存。差异清单包含商品 ID、旧库推算值、新库值、旧侧最近 outbox 序号、新侧最近
  flow_id。
- **回滚**：校验差异商品可标记回滚。回滚后路由立即回旧库，新库库存行保留但不再可见；
  账本记录商品、批次、时刻和原因，后续捕获不再作用到新库。

所有业务写请求应通过本服务的 `/api/debit` 路由入口。触发器 CDC 是绕过路由直接写旧库
时的安全网；在切换锁之外发生的旁路写会在 cutoff 后异步补偿，但业务系统仍应遵守单写
路由契约。每个 SQLite 账本文件对应一个服务实例。

## 目录

```text
src/InventoryMigration.Api      迁移、路由、校验、指标和状态服务
src/InventoryMigration.LoadGen  固定种子负载与故障注入场景工具
sql/old_schema.sql              旧库表、CDC outbox 和触发器
sql/new_schema.sql              新库表、分片列和迁移支持表
sql/seed_old.sql                确定性 50,000 库存 + 120,000 流水
scripts/                        Windows PowerShell 与 Bash 初始化/运行脚本
```

## 本地启动

先启动两个 PostgreSQL 16 实例，端口分别为 55432/55433。随后初始化确定性数据：

Windows PowerShell：

```powershell
.\scripts\setup-db.ps1 -PgBin "C:\Program Files\PostgreSQL\16\bin"
.\scripts\run-service.ps1
```

初始化脚本会重建旧库/新库的确定性数据并清理迁移账本，保证从第 1 个商品重新开始。
如需保留账本，传给 PowerShell 脚本 `-KeepLedger`，或给 Bash 脚本设置
`KEEP_LEDGER=1`。

Linux / WSL：

```bash
export PSQL=/path/to/postgresql/16/bin/psql
export CREATEDB=/path/to/postgresql/16/bin/createdb
./scripts/setup-db.sh
./scripts/run-service.sh
```

默认地址为 `http://127.0.0.1:5080`。连接串、分批大小和每个实例连接上限位于
`src/InventoryMigration.Api/appsettings.json` 的 `Migration` 段，也可用环境变量覆盖：

```powershell
$env:Migration__BatchSize = "500"
$env:Migration__ConnectionLimitPerInstance = "4"
$env:Migration__OldConnectionString = "Host=127.0.0.1;Port=55432;Database=inventory_old;Username=postgres;Password=postgres"
```

新库流水表为：

```text
deduction_flow(flow_id, product_id, direction, change_qty, created_at, shard_no)
```

`shard_no = product_id % ShardCount`。旧库和新库的初始 `flow_id` 都从 1 开始；在线写入
分别从 10^12 / 2×10^12 的序列段分配，避免与迁移流水冲突。

## 执行迁移

```bash
curl -X POST http://127.0.0.1:5080/api/migration/start
curl http://127.0.0.1:5080/api/status
```

服务启动时若账本 `started=1` 且尚未完成，会自动续迁。`pause` 在批次间生效：

```bash
curl -X POST http://127.0.0.1:5080/api/migration/pause
curl -X POST http://127.0.0.1:5080/api/migration/resume
```

写入路由示例：

```bash
curl -X POST http://127.0.0.1:5080/api/debit \
  -H "content-type: application/json" \
  -d '{"productId":42,"quantity":2,"direction":"deduct"}'
```

`direction` 只接受 `deduct` / `restock`。主要稳定错误码：

| HTTP | code | 含义 |
| --- | --- | --- |
| 409 | `E_MIGRATING` | 商品所在批次正在切换可见性 |
| 503 | `E_CONN` | 连接不可用、连接池满或连接等待超时 |
| 409 | `E_STOCK` | 扣减后库存为负 |
| 404 | `E_NOT_FOUND` | 商品不存在 |
| 409 | `E_VALIDATION` | 商品不在校验差异清单，拒绝非强制回滚 |

## 校验与回滚

```bash
curl http://127.0.0.1:5080/api/validation/latest
curl http://127.0.0.1:5080/api/validation/mismatches
curl http://127.0.0.1:5080/api/validation/1

curl -X POST http://127.0.0.1:5080/api/rollback \
  -H "content-type: application/json" \
  -d '{"productId":50000,"reason":"validation_mismatch"}'
```

默认只允许回滚对应批次校验差异中的商品。确需人工回滚时可传 `"force":true`。

## 状态与 Prometheus

`GET /api/status` 返回固定字段顺序的 JSON：迁移位点、当前批次、两侧连接池占用/上限、
回滚清单、最近一次校验摘要。`GET /metrics` 暴露：

```text
inventory_migration_migrated_products_total
inventory_migration_migrated_flows_total
inventory_migration_captured_changes_total
inventory_migration_routed_old_debits_total
inventory_migration_routed_new_debits_total
inventory_migration_e_migrating_total
inventory_migration_validation_mismatch_products
inventory_migration_rollback_products
inventory_migration_capture_p95_ms
inventory_migration_connection_pool
inventory_migration_connection_pool_limit
```

库、来源、目标与实例标签固定为 `source` / `target` / `database` / `instance`。

## 确定性负载与故障注入

先启动服务，再执行：

```bash
./scripts/run-scenario.sh --duration 20 --rate 300 --seed 20261119
```

带故障的示例：

```bash
./scripts/run-scenario.sh \
  --duration 20 \
  --rate 300 \
  --fault-old-down-ms 500 \
  --fault-batch-delay-ms 10 \
  --fault-validation-product 50000
```

故障通过运行时管理接口注入，不需要改源码：

```bash
curl -X POST http://127.0.0.1:5080/api/admin/faults \
  -H "content-type: application/json" \
  -d '{"oldDbUnavailable":false,"migrationBatchDelayMs":10,"validationCorruptProductIds":[50000]}'
```

`validationCorruptProductIds` 会在对应批次校验前对新库库存做 +1，从而产生真实差异，便于
验证差异清单和回滚。`DELETE /api/admin/faults` 清除全部故障。

报告默认写入 `artifacts/scenario-report.json`，包含：

- 各阶段已迁移商品数、捕获变更数、路由旧/新与 `E_MIGRATING` 分布
- 最终路由分布和 HTTP 状态码
- 最近校验的差异商品清单
- 回滚记录
- 服务内扣减捕获 P95 与客户端 HTTP P95

本次参考运行（3 类商品混合、300 req/s、批次延迟 10ms、旧库断连 500ms、注入 1 个
校验差异）结果：捕获 P95 为 **52.8ms**，50 批范围连续，12 万初始流水无重复，注入差异
商品准确列出并成功回滚。

## 故障与恢复行为

- 迁移任意阶段被 `SIGKILL`/断电：SQLite 中最后一个 committed 范围不变；重启自动重做
  当前未提交批次，库存使用绝对 upsert，流水使用主键去重。
- 新库事务提交后、SQLite 提交前崩溃：由于路由仍为 `migrating`，新库数据不可见；重做
  同一批次后再次校验并提交。
- 旧库连接中断：接口返回 `E_CONN`，迁移批次保持可恢复状态，故障恢复后自动重试。
- 连接池达到上限：等待最多 `ConnectionAcquireTimeoutMs`，超时返回 `E_CONN`，不无限挂起。

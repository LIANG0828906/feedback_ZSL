# SQL Server 订单在线分片迁移服务

面向单机 Windows + SQL Server Express 2022 的订单迁移服务。旧库保存
`Orders` / `OrderItems`，分片侧按 `tenant_id % 4` 定位 4 个结构相同的分片库。
服务提供双写、可续跑迁移、单调路由、双库核对、受控回滚和 SQLite 审计，不依赖
人工比对。

默认拓扑：

```text
LegacyDSN  -> OrdersLegacy (Orders, OrderItems, WriteReceipts)
ShardedDSN -> OrdersShard0 / OrdersShard1 / OrdersShard2 / OrdersShard3
tenant_id % 4 -> 0 / 1 / 2 / 3
```

## 快速启动（Windows）

前置条件：Python 3.11、SQL Server 2022 Express、两个命名实例
`.\LEGACY`、`.\SHARDED`，混合身份验证和本地 TCP 已启用。

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.txt

# 在“ODBC 数据源管理器（64 位）”中配置 LegacyDSN 与 ShardedDSN。
# config.json 通过 DSN 连接，不内嵌口令。
.\.venv\Scripts\python.exe -m orders_migration --config config.json validate
.\.venv\Scripts\python.exe -m orders_migration --config config.json serve
```

`validate` 会连接旧库和全部 4 个分片库，比较列、主键、外键、索引名称、键序和
包含列。任一连接失败、分片库缺失或结构不一致时返回 `E_TOPOLOGY` 且不监听端口。

已有业务库只需保证连接串中的数据库名正确，不需要运行初始化脚本。若要验证项目
自带的 2 万行种子，可在一次性测试库上运行：

```powershell
.\.venv\Scripts\python.exe scripts\bootstrap_local.py --config config.json --fresh
```

该命令会删除并重建连接串指向的 5 个验证库，禁止对生产库使用 `--fresh`。

## 配置

`config.json` 是 Windows DSN 模板；`config.local.json`（已忽略）是 Linux/WSL
开发配置。连接字符串支持 `${ENV_NAME}` 环境变量展开。

```json
{
  "listen": {"host": "127.0.0.1", "port": 8090},
  "database": {
    "driver": "auto",
    "legacy_connection_string": "DSN=LegacyDSN;DATABASE=OrdersLegacy;",
    "shard_connection_string_template": "DSN=ShardedDSN;DATABASE=OrdersShard{shard};",
    "shard_count": 4,
    "connect_timeout_seconds": 5,
    "command_timeout_seconds": 20
  },
  "connection_pool": {
    "max_connections": 24,
    "acquire_timeout_ms": 250,
    "idle_ttl_seconds": 60
  },
  "migration": {
    "batch_size": 500,
    "read_mode_during_migration": "legacy",
    "resume": true
  },
  "reconciliation": {"batch_size": 500},
  "rollback": {"wait_timeout_seconds": 30, "batch_size": 500},
  "audit": {"sqlite_path": "runtime/audit.db"},
  "load": {
    "target_writes_per_second": 20,
    "target_reads_per_second": 30,
    "write_p95_ms": 250,
    "read_p95_ms": 120
  },
  "fault_injection": {"enabled": false}
}
```

- `read_mode_during_migration` 取 `legacy` 或 `shard`。迁移完成后固定读分片。
- `max_connections` 是全部数据库共享的硬上限，包含空闲连接。达到上限后最多等待
  `acquire_timeout_ms`，超时返回 `E_CONN_POOL`。
- `driver=auto` 时，`DSN=` / `DRIVER=` 连接串走 `pyodbc`；Linux 本地开发可使用
  `driver=pymssql`。生产 Windows 部署使用题目要求的 `pyodbc 5.1.0`。
- `fault_injection.enabled=false` 时管理故障接口关闭。

## 数据模型

`sql/01_schema.sql` 在 5 个库中建立完全一致的结构：

- `dbo.Orders`：订单主表，`OrderId` 为主键，含 `TenantId`、`Status`、`Amount`、
  `Currency`、`UpdatedAt`、`CreatedAt`、单调 `Revision`。
- `dbo.OrderItems`：`(OrderId, LineNumber)` 复合主键，外键指向 `Orders`。
- `dbo.WriteReceipts`：每次双写、迁移或回滚的数据库侧提交位点。
- `IX_Orders_TenantStatus`、`IX_OrderItems_OrderId`、
  `IX_WriteReceipts_OrderId` 等索引在 5 库中逐项一致。

`sql/02_seed_legacy.sql` 使用固定公式生成 20,000 个订单和 40,001 条订单行，覆盖
16 个租户，租户取模后均匀进入 4 个分片。脚本按 5,000 行一批执行，适合共享的低
内存 SQL Server Express。

## 一致性协议

### 启动拓扑检查

`TopologyValidator` 在 HTTP 端口打开前完成全部 5 个库的检查：

1. 数据库可连接且 schema 查询成功；
2. `Orders`、`OrderItems`、`WriteReceipts` 均存在；
3. 必需主键与索引均存在；
4. 列定义、主键键序、索引键序、包含列、过滤条件、外键定义逐项规范化后计算
   SHA-256 签名；
5. 旧库签名和 4 个分片签名完全相等。

失败统一返回 `E_TOPOLOGY`，详细列出连接失败库和结构差异；服务不会部分加载。

### 双写

每笔写入在同一订单的内部互斥锁下执行：

1. 读取旧库与目标分片的当前版本和完整旧快照；
2. 在 SQLite 中先写 `PREPARING` 意图与订单级审计事件；
3. 分片事务内同时写订单、订单行和 `WriteReceipts`，提交；
4. 旧库事务内同时写订单、订单行和 `WriteReceipts`，提交；
5. 同一 SQLite 事务把状态从 `PREPARING` 改为 `COMMITTED` 并追加完成事件。

只有两侧主表和行表作为整体提交成功才返回成功，并返回两侧
`legacyCommitSite`、`shardCommitSite`。任一侧失败时：

- 失败侧事务回滚；
- 已提交分片按写入前快照补偿恢复或删除；
- 写入 `FAILED` 结果和 `E_DUAL_WRITE`；
- 订单查询共用同一订单锁，因此不会读到补偿中途。

若连接在 commit 后、响应前断开，服务通过两侧 `WriteReceipts` 判断真实提交结果。
重启时扫描 `PREPARING` 意图，两侧回执都在则转为 `COMMITTED`，只有一侧则在恢复
旧快照后转为 `FAILED`。

### 迁移

- 每个租户独立串行迁移，目标分片为 `tenant_id % shard_count`。
- 每批按 `OrderId` 游标读取旧库，订单主表和行表整体写分片。
- 分片写入带来源 `Revision`，目标已有相同或更高版本时跳过，因此重复执行不会
  覆盖更新值，也不会产生重复主键。
- 分片事务提交成功后，SQLite 同一事务写入 `migration_progress.last_order_id`、
  批次位点和审计事件。因此进程中断后从最后提交位点继续。
- 只有扫描到租户末尾才将状态置为 `COMPLETE`。

### 路由

- `NOT_STARTED`：读旧库。
- `IN_PROGRESS`：按 `read_mode_during_migration` 读旧库或分片。
- `COMPLETE`：读目标分片。
- `route_state` 为每个租户保存单调递增的 `rank/generation`，查询在订单锁内重新
  判断路由，保证同一订单连续查询不会从分片回退到旧库。
- 迁移中读分片但订单尚未到达时返回 `E_NOT_MIGRATED`，包含 `orderId`、
  `tenantId`。

### 核对

核对按订单号范围或租户分批进行，比较：

- 订单存在性；
- `tenantId`、`status`、`amount`、`currency`；
- 行表数量；
- 行表内容指纹；
- `createdAt`、`updatedAt`、`revision`。

每个批次先按订单加锁，再分别读取旧库与分片，避免与在途双写/迁移交错。双写和
查询无需全局停机。结论以 `(run_id, order_id, field_name)` 唯一写入 SQLite，每批
同时提交游标、累计数量、不一致明细和审计事件。任一侧连接失败时返回
`E_CHECK_UNAVAILABLE`，保留已完成游标和明细；可用同一 `runId` 续跑。

同一 scope 已完成的核对默认直接返回原结论；显式 `force=true` 才会新建 run，
不会覆盖旧 run 的结论。

### 受控回滚

1. 关闭新写入入口；
2. 等待在途双写和迁移计数归零，超时返回 `E_ROLLBACK`；
3. 逐租户扫描两库订单并选择较新来源：
   - 仅一侧存在：复制到另一侧；
   - 旧库 `UpdatedAt` 更新：旧库写分片，绝不覆盖旧库；
   - 分片 `UpdatedAt` 更新：分片写旧库；
   - 时间相同：比较 `Revision`，再比较完整载荷；
4. 每批先提交远端写入，再以同一 SQLite 事务记录动作、游标和审计；
5. 租户完成后执行完整字段核对，零差异才标记该租户完成；
6. 全部完成后把路由强制标记为 `rolled_back` 并恢复新写入。

中途失败返回 `E_ROLLBACK`，给出 `runId`、`completedTenants`、
`pendingTenants`、`failedTenants` 和当前写闸门状态。写闸门保持关闭；再次提交
同一 `runId` 从已提交批次继续。

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/healthz`, `/readyz` | 健康、连接池和写闸门 |
| `GET` | `/topology` | 5 库名称与 schema 签名 |
| `GET` | `/metrics` | 写/读 P95、错误计数、连接池峰值 |
| `POST` | `/api/v1/orders` | 双写订单 |
| `GET` | `/api/v1/orders/{id}?tenantId=2` | 按迁移进度路由读取 |
| `POST` | `/api/v1/migrations` | `{ "tenantId": 2, "maxBatches": 1 }` |
| `GET` | `/api/v1/migrations/{tenantId}` | 迁移位点和当前路由 |
| `POST` | `/api/v1/checks` | `{ "tenantId": 2, "orderIdStart": 1, "orderIdEnd": 20000 }` |
| `GET` | `/api/v1/checks/{runId}` | 核对状态和已完成明细 |
| `POST` | `/api/v1/checks/{runId}/resume` | 继续中断的核对 |
| `POST` | `/api/v1/rollbacks` | `{ "tenants": [2, 3] }`，省略表示全部 |
| `GET` | `/api/v1/rollbacks/{runId}` | 已完成/待重试租户与动作 |
| `POST` | `/api/v1/rollbacks/{runId}/resume` | 继续中断的回滚 |
| `GET` | `/api/v1/trace/{orderId}` | 该订单完整双写、事件、回滚记录 |

请求 `X-Idempotency-Key` 可让同内容双写返回第一次已提交结果。

成功双写示例：

```json
{
  "ok": true,
  "writeId": "f232f8cdc7794a49ac2d1a98c04073ee",
  "orderId": 2,
  "revision": 2,
  "legacyCommitSite": "OrdersLegacy:f232...:2026-11-19T05:53:09.003Z",
  "shardCommitSite": "OrdersShard2:f232...:2026-11-19T05:53:08.975Z"
}
```

稳定业务错误码：

| 错误码 | 场景 |
| --- | --- |
| `E_TOPOLOGY` | 连接失败、分片缺失、结构/主键/索引不一致 |
| `E_DUAL_WRITE` | 双写任一侧失败，已提交侧已补偿 |
| `E_NOT_MIGRATED` | 路由到分片但该订单尚未迁移，含订单号 |
| `E_CHECK_UNAVAILABLE` | 核对期间数据库不可用，保留部分结果 |
| `E_ROLLBACK` | 回滚未完成，带已完成/待重试租户 |
| `E_CONN_POOL` | 连接池等待超时 |
| `E_WRITE_STOPPED` | 回滚排空期间拒绝新写入/迁移 |
| `E_MIGRATION` | 迁移数据库失败，已提交位点保留 |
| `E_MIGRATION_INTERRUPTED` | 分片批次已提交但本地位点未提交 |
| `E_QUERY_UNAVAILABLE` | 查询路由数据库不可用 |
| `E_VALIDATION` | 请求字段非法 |
| `E_NOT_FOUND` | 旧库中不存在该订单 |

## 本地 SQLite 审计

SQLite 启用 WAL。表包括：

- `dual_writes`：双写意图、旧快照、提交位点、错误；
- `migration_progress` / `migration_batches`：租户位点与批次；
- `route_state`：单调路由 rank/generation；
- `check_runs` / `check_results`：核对游标、结论和字段级差异；
- `rollback_runs` / `rollback_progress` / `rollback_actions`：回滚租户和订单动作；
- `trace_events`：按 `order_id` 可追溯的完整事件流。

状态变化和对应事件在同一个 SQLite 事务中提交。进程重启时自动恢复中断的
`PREPARING` 双写、继续未完成核对；存在未完成回滚时先关闭写闸门再自动续跑。

## 验证

Linux/WSL 本地验证可使用 `pymssql` 作为 ODBC 的开发替身：

```bash
export MSSQL_PASSWORD='<runtime-only-password>'
export OM_ADMIN_TOKEN='<local-admin-token>'
.venv/bin/python scripts/make_local_config.py
.venv/bin/python scripts/bootstrap_local.py --config config.local.json --fresh
.venv/bin/python scripts/run_acceptance.py --config config.local.json
```

Windows 使用 DSN 配置时：

```powershell
pwsh -File .\scripts\run_acceptance.ps1 -Config config.json
```

Windows 验收默认不重建数据库。只有连接串明确指向一次性验证库时才加
`-Bootstrap` 重新灌入 2 万行种子。

验收脚本覆盖：

- 5 库拓扑校验与缺失分片拒绝启动；
- 双写成功与一侧失败补偿；
- 迁移批次提交后中断、重启续跑和幂等；
- 路由 `legacy -> shard` 不回退、`E_NOT_MIGRATED`；
- 正常核对、字段漂移发现、核对中断续跑；
- 回滚收敛、旧库较新值不被覆盖、回滚失败续跑；
- 订单级完整追踪；
- 20 写/秒 + 30 读/秒混合负载 P95。

本次共享 SQL Server 验证结果见 `artifacts/acceptance.json`：

- 18 个验收步骤全部通过；
- 写 P95 `124.878ms`，目标 `250ms`；
- 读 P95 `44.767ms`，目标 `120ms`；
- 400 次写、600 次读，0 错误；
- 连接池峰值 24，等于配置上限；
- 迁移中断后从 `lastOrderId=0` 续跑到 `7986`，最终 `19986` 完成；
- 双写故障后金额保持 `321.09`，无单边新值；
- 漂移核对发现旧库 `321.09` / 分片 `328.86`；
- 回滚后再次对该租户全部订单逐字段核对 `mismatchCount=0`。

全量迁移另见 `artifacts/full-migration.json`：16 个租户全部完成，4 个分片均被
使用，旧库 20,000 单迁移后全范围核对 `totalChecked=20000`、
`mismatchCount=0`。

单元测试：

```bash
.venv/bin/python -m pytest
# 10 passed
```

## 目录

```text
orders_migration/
  config.py       严格配置和 ${ENV}
  errors.py       稳定业务错误码
  sqlclient.py    pyodbc/pymssql、受约束连接池
  topology.py     5 库结构与签名校验
  model.py        订单值对象、校验、载荷哈希
  repository.py   订单/行表/回执 SQL
  ledger.py       SQLite 事务账本与恢复查询
  service.py      双写、迁移、路由、核对、回滚
  http_api.py     HTTP 接口
  cli.py          命令行
sql/
  01_schema.sql
  02_seed_legacy.sql
scripts/
  bootstrap_local.py
  make_local_config.py
  loadtest.py
  run_acceptance.py
  run_acceptance.ps1
  run_full_migration.py
tests/
```

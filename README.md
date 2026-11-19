# OrderRouter — 订单冷热分离路由、在线迁移与校验

在单实例 SQL Server 2022（本机共享容器 `ordercutover-sql2022`，`127.0.0.1:1433`，
独立库 `OrdersDb298` 隔离）上实现：按归档截止时间把查询路由到热表 `Orders`、
冷表 `OrdersArchive` 或两侧合并；按主键范围分批在线迁移并记录 SQLite 账本；
连接池上限与稳定错误码 `E_CONN`；受控负载回放；事务快照一致性校验；
Prometheus 指标与 JSON 状态接口。

## 布局

- `src/OrderRouter/` — 单进程 CLI：`init-db | seed | serve | migrate | replay | verify`
- `sql/01_init.sql` — Orders / OrdersArchive / OrderItems 建表与主键索引（幂等）
- `config/appsettings.json` — 连接串、归档截止时间、连接上限、账本相对路径、批大小
- `config/replay.json` — 回放并发、每类请求数、固定种子、报告路径
- `tests/OrderRouter.Tests/` — xunit（路由拆分、重叠去重、迁移续传、E_CONN 闸门）
- `scripts/run-e2e.sh` — 本机端到端验收脚本

## 关键设计

- **路由拆分**（`Router`）：日期条件按配置的 `archiveCutoffUtc` 拆分——
  完全 `to < cutoff` 且迁移完成 → 仅冷表；`from >= cutoff` → 仅热表；跨界 → 两侧合并。
  主键范围条件查 SQLite 账本已提交位点决定是否访问冷表。响应携带
  `route / tablesAccessed / splitCount / dedupDropped`，每行标注 `source`。
- **重叠去重**：合并按 `OrderId` 分组取 `LastUpdated` 较新一侧，平局取热表，
  结果按 `OrderNo` 排序，与不拆分直读两表的合并逐行一致（`RoutingTests` 断言）。
- **在线迁移**（`Migrator`）：每批一个 SNAPSHOT 事务——快照读热表、MERGE  upsert
  归档（仅当源更新）、仅删除“归档副本不旧于热表”的行，单事务提交，读侧不会看到
  迁移了一半的订单；提交后写 SQLite 账本（WAL）。崩溃只可能重跑幂等批次；
  重启从最后提交位点继续，不重复、不跳过。快照更新冲突（3960）按批重试。
- **写入路径**（`Writer`）：更新必落热表（已删除的热行从归档物化回来），
  已迁移订单同步在归档侧产生变更，同一事务提交；连续更新按时间收敛到最新值。
- **连接闸门**（`ConnectionGate`）：总量 `maxConnections`，交互查询保留
  `reservedInteractiveConnections` 个槽位，长查询（`class=batch`）限制在
  `max - reserved`。获取超时（`acquireTimeoutMs`）即返回稳定 `E_CONN`，
  不无限等待；`/connections` 与 `/status` 可查实时占用。
- **快照校验**（`Verifier`）：选取服务观测到“被读且被更新”的订单（默认 500），
  在同一个 SNAPSHOT 事务内同时做路由合并读与两表直接合并读，逐字段比对
  金额/币种/更新时间，输出不一致订单号、字段名与两端取值；结论只反映事务
  开始时刻已提交数据，迁移与写入可并行。
- **指标**：`orderrouter_hot_queries_total`、`orderrouter_cold_queries_total`、
  `orderrouter_merged_queries_total`、`orderrouter_dedup_dropped_total`、
  `orderrouter_migration_committed_ranges_total`、
  `orderrouter_connection_pool_in_use`、`orderrouter_econn_total`（`/metrics`）。
  `/status` 返回归档截止时间、迁移位点、连接池状态与最近校验摘要，
  字段稳定排序且只含相对路径。

## 运行

```bash
source scripts/env.sh                 # .NET 8、libicu、GC 堆上限（≤1.25GiB 预算）
dotnet build src/OrderRouter -c Release
BIN=src/OrderRouter/bin/Release/net8.0/orderrouter
$BIN init-db                          # 建库 + 快照隔离 + 建表
$BIN seed --orders 20000 --items 30000   # 固定种子功能规模（全量 100000/200000）
$BIN serve &                          # http://127.0.0.1:8298/
$BIN migrate                          # 在线迁移，可 kill 后重入续传
$BIN replay                           # 四类混合负载，报告写 data/replay-report.json
$BIN verify --count 500               # 事务快照一致性校验
dotnet test tests/OrderRouter.Tests   # 7 个用例
bash scripts/run-e2e.sh               # 一键端到端验收
```

资源约束：进程以 `taskset -c 0,1` 限制 2 核，`DOTNET_GCHeapHardLimit=0x18000000`
（384MiB）+ SQL Server 容器既有 1GiB 上限，峰值保持在 1.25GiB 预算内；
SQL Server 复用已启动的公共容器，用独立数据库 `OrdersDb298` 隔离。

## 验收口径

- 功能验证规模：2 万订单 / 3 万订单行（`seed` 默认）；性能口径按完整规模
  10 万订单 / 20 万行声明：`seed --orders 100000 --items 200000`。
- `scripts/run-e2e.sh` 实测（2 万/3 万）：迁移中 300 更新 + 300 读取并发下
  快照校验 299 单 0 不一致；kill -9 后续传 34 段连续无重复；连接占满返回
  `E_CONN`（2s 超时）；batch 槽位占满时交互查询 P95 ≈ 5–15ms（阈值 300ms）；
  回放 1600 请求 0 失败，四类 P95 均 ≤ 101ms。

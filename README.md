# SQL Server 索引切换读写兼容网关

该网关在同一个 SQL Server 2022 实例上同时服务两种访问键序：

- 旧键序：`Status + OrderId`，期望索引 `IX_Orders_Status`。
- 新键序：`TenantId + Status + UpdatedAt + OrderId`，期望索引
  `IX_Orders_TenantStatus`。

项目包含确定性的 3 万行数据初始化、8 个可重复验证场景、实际执行计划证据和
阶段报告。所有报告字段只使用逻辑名称与 UTC 时间，不含绝对路径、主机名或账号。

## 环境要求

- Windows Server 2022；SQL Server 2022 Developer，默认实例、TCP 1433、混合认证。
- .NET 8 SDK。
- `sa` 密码由部署脚本写入 `OG_SA_PASSWORD`（也兼容 `SA_PASSWORD`），或直接提供
  `OG_CONNECTION_STRING`。源码中没有内置密码。
- 也支持 WSL/Linux 验证；脚本会优先复用已经监听 1433 的服务，没有服务时可用
  Docker 启动受限的本地 SQL Server。

`sql/01_init.sql` 会重建 `OrdersDb`，只应用于本地验证库。数据固定为：

- 30,000 行订单；
- 12 个租户（每个 2,500 行）；
- 5 种状态（每个 6,000 行）；
- 任意租户/状态组合恰好 500 行；
- `UpdatedAt` 跨度为 3 年。

索引与票据一致：

```sql
CREATE NONCLUSTERED INDEX IX_Orders_Status
    ON dbo.Orders (Status, OrderId)
    INCLUDE (TenantId, Amount, UpdatedAt);

CREATE NONCLUSTERED INDEX IX_Orders_TenantStatus
    ON dbo.Orders (TenantId, Status)
    INCLUDE (Amount, UpdatedAt);
```

## 运行验证

Windows PowerShell：

```powershell
$env:OG_SA_PASSWORD = '<deployment-provided-password>'
pwsh -File .\scripts\verify.ps1
```

WSL/Linux：

```bash
export OG_SA_PASSWORD='<deployment-provided-password>'
bash scripts/run-validation.sh
```

常用环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `OG_SQL_SERVER` | `127.0.0.1,1433` | SQL Server 地址 |
| `OG_DATABASE` | `OrdersDb` | 验证库名 |
| `OG_MAX_CONNECTIONS` | `6` | 网关并发连接硬上限 |
| `OG_ACQUIRE_TIMEOUT_SEC` | `4` | 获取连接槽的等待阈值 |
| `OG_COMMAND_TIMEOUT_SEC` | `15` | 单条命令超时 |
| `OG_HEARTBEAT_EXPIRY_SEC` | `10` | 旧实例心跳过期阈值 |
| `OG_DDL_LOCK_TIMEOUT_SEC` | `30` | 索引 DDL 锁等待阈值 |
| `OG_ARTIFACT_DIR` | `artifacts/run` | 报告与计划证据目录 |

脚本将 MSBuild 并行度固定为 2，运行时可见处理器数固定为 2；本地 SQL Server
容器限制为 1 GiB 内存和 2 CPU，网关运行时 GC 堆上限为 256 MiB。

## 核心接口

`OrderRepository` 提供单页快照读取：

- `ReadPageAsync(KeyOrder, PageQuery)`：正常业务读取。
- `ReadPageWithExpectedIndexAsync(...)`：索引拓扑变化后的显式期望索引读取。
- `ReadPageWhileHoldingSnapshotAsync(...)`：变更窗口内的在途读验证。
- `ReadPageWithAcquireTimeoutAsync(...)`：连接池耗尽边界探测。

`SnapshotPagingSession` 在同一个 SNAPSHOT 事务中完成整轮游标分页，保证混合更新
下的整组分页来自同一个可见集合。

游标分别携带：

- 旧键序：`LastOrderId`；
- 新键序：`UpdatedAt + LastOrderId`。

两种键序始终返回相同的订单多重集合，只允许顺序不同。

## 索引切换协议

1. `LegacyHeartbeat` 中两个旧实例只要有一个心跳年龄小于
   `HeartbeatExpiry`，旧索引就保持在线，`RetireOldIndexAsync` 返回业务上的
   `E_RETIRE_BLOCKED`。
2. 全部旧实例心跳过期后，网关进入写屏障，执行 `DROP INDEX IX_Orders_Status`。
3. 读请求先进入共享屏障，再租用 SQL 连接；写屏障等待所有在途读完成。
   因此 DDL 不会打断已经开始、尚未返回的查询，写屏障等待期间也不会再放行新读。
4. 回退操作删除新索引并重建旧索引；新键序在退化拓扑下显式回退到仍可用的旧
   索引，计划保护按当前拓扑的期望索引校验。回退验证完成后重新建立双索引，
   使套件可重复运行。

## 计划保护与错误码

每次查询在数据返回前会执行 `SET STATISTICS XML ON` 捕获实际计划，解析
`RelOp` 的物理算子以及 `Object.Index`。若实际访问路径不是该键序的期望索引，
或退化为表/聚集索引扫描，则拒绝返回并记录：

- `E_PLAN_MISMATCH`：包含 `tenantId`、`status`、`pageSize`、`cursor`、
  `keyOrder`、期望/实际索引和全部访问路径。
- `E_DEADLOCK`：包含过滤条件、键序、SQL 错误号等上下文。
- `E_CONN_TIMEOUT`：连接槽获取超时或连接打开超时，连接槽会立即释放。
- `E_RETIRE_BLOCKED`：旧实例仍活跃，拒绝下线旧索引。

所有页面查询均显式指定对应索引并使用 `FORCESEEK`；索引缺失的回退路径使用
`FORCESEEK` 但不锁定具体索引。保存的实际计划 XML 是索引与 seek 行为的直接证据。

## 阶段报告

验证完成后写入：

- `artifacts/run/stage_report_full.json`
- `artifacts/run/actual_plan_legacy_status_orderid.sqlplan`
- `artifacts/run/actual_plan_new_tenant_status_updated_orderid.sqlplan`

报告包含：

- 输入 SQL 指纹、配置与进程工作集；
- 每种键序的索引命中次数和访问方式；
- `E_PLAN_MISMATCH`、`E_DEADLOCK`、`E_CONN_TIMEOUT` 计数及错误上下文；
- 连接池配置上限、峰值占用、租用次数与获取超时；
- 索引切换阶段、索引名、UTC 时间点和相对偏移；
- 8 个场景的逐项通过状态与证据。

相同源码与配置下，`inputFingerprint` 保持一致，可直接比较多次运行。

## 验收场景

| 场景 | 验证内容 |
| --- | --- |
| `s1_pagination_equivalence` | 5 组过滤 × 5 种页大小，两种键序多重集合一致 |
| `s2_plan_guard` | 双键序 seek 命中及错误索引被 `E_PLAN_MISMATCH` 拒绝 |
| `s3_dual_index_cutover` | 心跳保护、过期下线、在途查询无错误 |
| `s4_concurrent_consistency` | 双键序并发快照分页 + 同批更新，无重复/缺失/死锁 |
| `s5_connection_pool` | 并发连接不超上限、长短期共享、耗尽返回 `E_CONN_TIMEOUT` |
| `s6_rollback_validation` | 删新恢复旧、两种键序仍 seek、快照逐行一致 |
| `s7_observability` | 报告字段完整且无路径/外部标识泄漏 |
| `s8_performance_boundary` | 6 并发读取 P95 不超过 150ms，全部索引 seek |

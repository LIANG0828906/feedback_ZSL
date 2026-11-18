# 副本组快照一致性网关

该项目在单个 Windows 进程内模拟“一主三只读副本”的 SQLite 订单拓扑，用于
验证覆盖索引切换期间的快照一致性、双键序读取、执行计划校验、并发分页、
原子切换/回滚和审计报告。

## 快速验收

环境要求：

- Windows 10/11 或 Windows Server 2022；也支持 WSL/Linux 进行同等验证。
- .NET 8 SDK。
- SQLite `3.42.0` 原生库。仓库已在 `native/` 固定 Linux x64、Windows x64
  和 Windows x86 的 3.42.0 二进制，构建后会自动覆盖 NuGet 包内原生资产，
  不依赖外部数据库服务。

在仓库根目录执行：

```powershell
dotnet restore SnapshotGateway.sln
dotnet build SnapshotGateway.sln
dotnet run --project src/SnapshotGateway --no-build -- `
  --self-test --report artifacts/report.json
```

或直接运行：

```powershell
pwsh -File scripts/verify.ps1
```

自测会重置 `data/`，生成确定性的 20,000 行订单，创建两个索引和三份副本，
然后验证快照追平、双键序集合一致、计划错配拒绝、切换、回滚、6 并发 P95、
混合读写分页和连接上限。成功时退出码为 0，并在标准输出打印 JSON 报告，
同时写入 `artifacts/report.json`。

> WSL 缺少 ICU 时可加 `DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1`。项目本身已
> 开启 invariant globalization，Windows 正常运行时无需设置。

## 启动 HTTP 网关

```powershell
pwsh -File scripts/run.ps1 -Port 5188
```

首次启动会自动建库。需要重建确定性数据时加 `-Reset`。

查询示例：

```powershell
$old = Invoke-RestMethod `
  "http://127.0.0.1:5188/api/orders?tenantId=T001&status=NEW&keyOrder=old&page=1&pageSize=25"

$new = Invoke-RestMethod `
  "http://127.0.0.1:5188/api/orders?tenantId=T001&status=NEW&keyOrder=new&page=1&pageSize=25"
```

两种键序命中相同过滤集合，但返回顺序不同。响应同时包含：

- `snapshotVersion` 和所用 `replicaId`。
- `plan.expectedIndex`、`plan.actualIndexes`、`plan.accessMethod`。
- 完整 `plan.lines` 与阈值对照结果。
- `replicaVersions`（三个副本的版本）。

常用接口：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/orders` | 按租户/状态分页，`keyOrder=old|new` |
| POST | `/api/index/switch` | 原子切换到新索引代次 |
| POST | `/api/index/rollback` | 在新索引下线前回滚 |
| POST | `/api/orders/update?count=25` | 主库确定性批量更新 |
| GET | `/api/status` | 索引状态、副本版本、计数 |
| GET | `/api/replicas` | 主库/副本快照版本 |
| GET | `/api/audit` | 切换与回滚审计 |
| GET | `/api/report` | 生成并返回 JSON 报告 |
| POST | `/api/report/write` | 生成并落盘 JSON 报告 |
| GET | `/health` | 健康检查 |

## 业务错误

| 错误码 | HTTP | 场景 |
| --- | --- | --- |
| `SNAPSHOT_LAG` | 409 | 副本版本不一致；已触发追平，本请求不返回数据 |
| `E_PLAN_MISMATCH` | 409 | 实际计划未命中当前键序期望索引 |
| `E_CONN_LIMIT` | 429 | SQLite 并发连接达到配置上限 |
| `E_PERF_BUDGET` | 200（warning） | 单页耗时超过阈值，数据仍返回并记录 |
| `E_STATE_CONFLICT` | 200（success=false） | 切换/回滚状态不允许 |
| `E_VALIDATION` | 400 | 参数非法 |

`SNAPSHOT_LAG` 响应包含 `versionsBefore`、`versionsAfter`、主库版本和追平
耗时。提交索引切换的事务不改变数据快照版本，因此回滚前后在相同快照版本
下返回的订单逐条一致，且不会丢失新键序已提交的 `UpdatedAt` 等数据。

## 索引与快照设计

- 旧键序：`Status + OrderId`，索引
  `idx_old_status_orderid`。
- 新键序：`TenantId + Status + UpdatedAt + OrderId`，索引
  `idx_new_tenant_status_updated_orderid`。
- 两个索引始终物理共存；切换只原子更新 `index_catalog`、`app_metadata`
  和 `switch_audit`。
- 每个副本是独立 SQLite 文件，通过在线备份追平主库已提交版本。
- 分页的 count 与 rows 在同一个 deferred 只读事务中执行。
- 主库写入、索引切换共用写门；副本查询与副本追赶使用副本级锁。

更完整的协议与一致性说明见 `docs/ARCHITECTURE.md`。

## 报告内容

`artifacts/report.json` 包含：

- 拓扑、SQLite 实际版本和并发连接上限。
- 旧/新索引命中次数、有效状态和最终代次。
- `SNAPSHOT_LAG`、`E_PLAN_MISMATCH`、`E_CONN_LIMIT`、
  `E_PERF_BUDGET` 计数。
- 结构化失败日志（错误码、键序、租户、状态、分页和计划原因），满足
  计划错配可复核要求。
- 每次副本追赶的目标版本、实际版本、耗时和错误。
- 6 并发读取 P95、阈值、采样明细，以及每条查询计划行的耗时对照。
- 切换/回滚审计记录和最终快照版本。
- `selfTest.checks` 逐项验收结果。

性能预算默认 `200ms`，可用 `--perf-threshold-ms` 调整；SQLite 并发连接上限
默认 `8`，可用 `--max-connections` 调整。

# SQL Server 主备订单一致性服务

这是一个可在单台 Windows 主机、两个 SQL Server Express 命名实例上运行的最小可复核实现。服务只通过配置识别日志传送或事务复制，不自动创建复制拓扑；写入只走当前主库，读请求按 `strong`、`session`、`eventual` 三种会话一致性等级路由。

## 根因与修复思路

原始故障有两个独立问题：

1. `eventual` 读一直连备机，没有把“复制链路断开”或“备机水位落后”变成读路径决策，因此同步中断 12 分钟后仍会返回旧订单状态。
2. 写入成功只代表主库提交，没有可比较的提交水位；会话下一次读备机时无法证明自己的写入已经应用，所以主库已确认的订单可能在备机不可见。

本实现用 `dbo.ConsistencyWatermark` 消除这两个缺口：

- 每笔订单写入先在同一事务中执行一次 `UPDATE ... OUTPUT`，得到唯一 `commit_watermark`，再写入 `Orders` 和 `OrderLines`。订单头和所有订单行使用同一个水位。
- 水位行更新持有同一行的排他锁直到事务提交，因此水位顺序与事务提交顺序一致，可用于比较。
- 水位表必须和 `Orders`、`OrderLines` 一起进入同一个发布/日志传送链路。这样“备机水位 >= 会话水位”意味着该事务之前的订单变更已随事务原子应用。
- `session` 读在超时前等待备机应用水位；未达到则返回 `E_CONSISTENCY`，不偷偷回退。
- 备机连续无进展、不可达或最近 30 秒平均滞后超阈值时，`eventual` 明确改为读主库，并在响应中返回 `degraded=true` 和非空 `degraded_reason`。
- 监控使用独立连接，不占用主库业务连接池。

## 目录

```text
src/ordersync/          Python 服务、路由、水位、监控、切换和报告
sql/001_primary_schema_seed.sql
sql/002_replica_schema.sql
sql/verify_topology.sql
config/example.json      Windows 主备实例配置模板
config/local-test.json   仅用于路由逻辑演示的配置
scripts/                 PowerShell 启动、主库故障注入、切换钩子模板
tools/loadgen.py         本地确定性读写负载和 SLO 检查
tests/                   无 SQL Server 依赖的单元/HTTP 测试
```

## 前置条件

- Windows 10/11 或 Windows Server 2022。
- 两个 SQL Server Express 2022 命名实例，例如 `SQLEXPRESS_MAIN` 和 `SQLEXPRESS_REPLICA`。
- 两个实例启用 TCP/IP，分别固定监听 `1433` 和 `1434`，并启用混合身份验证。使用 SSMS 或 `sqlserver.conf` 设置固定端口后重启实例。
- Python 3.11+、pyodbc 5.x、Microsoft ODBC Driver 18 for SQL Server。
- 复制方式二选一：日志传送或事务复制。复制配置由使用者手工完成。
- 如使用 1433/1434 固定端口，连接串中的服务器写成 `localhost,1433`、`localhost,1434`；这比依赖命名实例动态端口更适合本任务。

### Express 版本约束

SQL Server Express 2022 没有 SQL Server Agent，而且不能作为事务复制的发布端。因此：

- 本仓库默认给出“无 Agent 手工日志传送”路径：Windows 计划任务或前台 PowerShell 循环执行日志备份/还原，服务本身只读取水位，不依赖 Agent。
- 若要使用事务复制，发布端必须是 Standard/Enterprise/Developer 等支持发布的实例，Express 只能作为订阅端；不要假设两个 Express 实例能组成事务复制发布/订阅对。
- `faults.disconnect_command` / `reconnect_command` 和 `sql_agent_job` 两种故障控制都支持；Express 手工日志传送应使用前一种。

安装 Python 依赖：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install --upgrade pip
.\.venv\Scripts\pip install -r requirements-windows.txt
```

不要在仓库中写入真实 `sa` 密码。通过环境变量提供，并记录在受控密码库中：

```powershell
$env:MSSQL_SA_PASSWORD = "<recorded-in-vault>"
$env:ORDERSYNC_ADMIN_TOKEN = "<random-local-admin-token>"
```

## 数据库初始化

### 主库

```powershell
$env:SQLCMDPASSWORD = $env:MSSQL_SA_PASSWORD
sqlcmd -S "localhost,1433" -U sa -C `
  -i sql\001_primary_schema_seed.sql -v PrimaryDatabase="OrderMain"
```

脚本在 `Orders`、`OrderLines` 中确定性生成各 20,000 行，并创建：

- `Orders(OrderNo PK, TenantId, Status, Amount, ItemCount, CreatedAtUtc, UpdatedAtUtc, LastWriteWatermark)`
- `OrderLines(OrderNo, LineNo) PK, TenantId, Sku, Quantity, UnitPrice, LineAmount, WriteWatermark, FK Orders`
- `ConsistencyWatermark(Name='orders', AppliedWatermark, UpdatedAtUtc)`
- `TopologyProbe`，启动时用可回滚更新验证主库可写、备机只读
- `IX_Orders_Tenant_Status_OrderNo`，覆盖按租户和状态分页

### 备机

- 日志传送：先完成主库全量备份/还原，再手工配置日志传送。备机数据库应处于 `STANDBY`，允许只读查询。不要先运行 `002_replica_schema.sql` 覆盖日志传送结构。
- 事务复制：创建发布时选择 `dbo.Orders`、`dbo.OrderLines`、`dbo.ConsistencyWatermark` 和结构探针表 `dbo.TopologyProbe`；前三张表**必须放在同一个发布中并保持事务应用顺序**。初始化快照后，将订阅库设置为只读，并验证 `DATABASEPROPERTYEX(...,'Updateability') = 'READ_ONLY'`。

无 Agent 的双 Express 日志传送可先执行：

```powershell
$env:MSSQL_SA_PASSWORD = "<recorded-in-vault>"
.\scripts\bootstrap_log_shipping.ps1 `
  -PrimaryInstance "localhost,1433" `
  -ReplicaInstance "localhost,1434" `
  -ShareDirectory "C:\ordersync\logship"
```

随后用一个前台窗口或计划任务循环执行：

```powershell
.\scripts\run_manual_log_shipping.ps1 `
  -PrimaryInstance "localhost,1433" `
  -ReplicaInstance "localhost,1434" `
  -ShareDirectory "C:\ordersync\logship" `
  -IntervalSeconds 2
```

两个命名实例的 SQL Server 服务帐号都必须能读写 `ShareDirectory`。该目录同时存放备机数据库 MDF/LDF/undo 文件；生产环境应改用本机专用数据目录，不要把数据库文件放到网络共享。

如果需要一个空结构做拓扑演练，可运行：

```powershell
$env:SQLCMDPASSWORD = $env:MSSQL_SA_PASSWORD
sqlcmd -S "localhost,1434" -U sa -C `
  -i sql\002_replica_schema.sql -v ReplicaDatabase="OrderReplica"
```

该脚本末尾会将备机数据库置为只读；事务复制初始化快照阶段通常应先行完成再置只读。拓扑检查会同时读取 `sys.databases.is_read_only` 并尝试一次可回滚更新，备机可写时启动直接失败并返回 `E_TOPOLOGY`。

## 启动与拓扑校验

```powershell
.\scripts\validate_topology.ps1 -Config config\example.json
.\scripts\run_service.ps1 -Config config\example.json
```

启动过程先完整解析配置，再验证：

1. 主库、备机连接可用，连接串对应配置中的数据库名。
2. 主备位于不同 SQL Server 实例。
3. 两张订单表、水位表和探针表列结构、类型、可空性、identity/computed 属性、主键完全一致。
4. 主库可执行可回滚更新；备机 `READ_ONLY` 且更新探针失败。
5. 配置中的复制方式已识别，能读取备机应用水位。复制注册信息只作为可选诊断，不作为启动必需条件，以兼容无 Agent 的手工日志传送。

任一失败均输出稳定错误码 `E_TOPOLOGY`，CLI 退出码为 2，HTTP 监听不会启动，不存在“加载部分配置后继续服务”的路径。

## API

默认监听 `127.0.0.1:8080`。设置 `admin_token` 后，`/admin/*` 需要 `X-Admin-Token`。

### 写订单

```powershell
$body = @{
  order_no = "ORD-DEMO-0001"
  tenant_id = "T01"
  status = "CONFIRMED"
  created_at_utc = "2026-01-01T00:00:00.000Z"
  lines = @(
    @{ line_no = 1; sku = "SKU-1"; quantity = 2; unit_price = "9.99" },
    @{ line_no = 2; sku = "SKU-2"; quantity = 1; unit_price = "1.00" }
  )
} | ConvertTo-Json -Depth 5

curl.exe -sS http://127.0.0.1:8080/orders `
  -H "Content-Type: application/json" `
  -H "X-Session-Id: session-demo" `
  -d $body
```

响应包含：

```json
{
  "commit_watermark": "1",
  "data": {
    "commit_watermark": "1",
    "order_no": "ORD-DEMO-0001",
    "session_id": "session-demo"
  },
  "session_id": "session-demo"
}
```

订单头和订单行在同一事务提交。主键/唯一键冲突、外键失败会回滚并返回 `E_WRITE_CONFLICT`。主库连接失败返回 `E_WRITE_UNAVAILABLE`，不会在备机写入或伪造成功。

### 读订单

```powershell
curl.exe -sS "http://127.0.0.1:8080/orders/ORD-DEMO-0001?consistency=session" `
  -H "X-Session-Id: session-demo"
```

按租户和状态分页：

```powershell
curl.exe -sS "http://127.0.0.1:8080/orders?tenant_id=T01&status=CONFIRMED&page=1&page_size=50&consistency=eventual"
```

一致性语义：

| 等级 | 读路径 | 水位要求 | 失败/降级 |
|---|---|---|---|
| `strong` | 当前主库 | 无 | 不受备机影响 |
| `session` | 当前备机 | 备机水位 >= 本会话先前写入的最大水位 | 超时/水位不足返回 `E_CONSISTENCY` 和备机当前水位，不读主库；备机池耗尽返回 `E_CONN_POOL` |
| `eventual` | 当前备机 | 不保证 | 滞后/断开/备机不可达时改读主库并标注 `degraded_reason` |

`session` 水位来源为 `X-Session-Id` 对应的服务端会话状态，也可用 `X-Min-Watermark` 提供跨进程恢复所需的显式下限。非法等级返回 `E_CONSISTENCY`。

读响应中的 `consistency`：

```json
{
  "level": "session",
  "served_by": "replica",
  "replication_mode": "log_shipping",
  "primary_watermark": "17",
  "replica_applied_watermark": "17",
  "watermark_gap": "0",
  "required_watermark": "17",
  "degraded": false,
  "degraded_reason": null
}
```

HTTP 响应头同时带 `X-Replica-Applied-Watermark`、`X-Consistency-Served-By`、`X-Consistency-Degraded`；写响应带 `X-Commit-Watermark`。

### 状态、报告与比较

```powershell
curl.exe -sS http://127.0.0.1:8080/health/topology
curl.exe -sS http://127.0.0.1:8080/consistency/status
curl.exe -sS http://127.0.0.1:8080/orders/ORD-DEMO-0001/compare
curl.exe -sS http://127.0.0.1:8080/report
curl.exe -sS -X POST http://127.0.0.1:8080/admin/report/reset `
  -H "X-Admin-Token: $env:ORDERSYNC_ADMIN_TOKEN"
```

报告包含：

- 每种一致性等级的请求数、成功数、`E_CONSISTENCY` 数、降级读主库次数、读失败数。
- 写入请求、失败、按错误码分类的失败数。
- 成功/中止的切换次数和每次切换耗时。
- 水位差和“写入确认到备机可见”的毫秒延迟分位（p50/p95/p99/max）。
- 按 `order_no` 去重并稳定排序的不一致样本，字段差异按字段名排序；分类为 `replica_behind`、`missing_on_replica`、`field_mismatch` 等。
- 不包含绝对路径、主机名、实例名、连接串或密码。除耗时字段外，同一负载/故障序列可重复对比。

## 同步中断与主库短时不可用

### 中断复制作业

无 Agent 手工日志传送时，`config/example.json` 默认通过外部命令写入暂停标志：

```json
{
  "disconnect_command": ["powershell.exe", "-NoProfile", "-File", "scripts\\set_replication_pause.ps1", "-Pause"],
  "reconnect_command":  ["powershell.exe", "-NoProfile", "-File", "scripts\\set_replication_pause.ps1"]
}
```

循环脚本看到标志后停止下一轮备份/还原；服务会在无进展阈值后判定断开并让 `eventual` 改读主库。外部命令收到 `ORDERSYNC_PRIMARY_*`、`ORDERSYNC_REPLICA_*` 和 `ORDERSYNC_REPLICATION_MODE` 环境变量，便于替换成防火墙、计划任务或其他网络故障注入。

使用启用 SQL Server Agent 的实例时，可在 `faults.replication_jobs` 中配置实际作业名。日志传送通常需要 backup/copy/restore 作业；事务复制通常需要 distribution agent 作业。每个作业标明它所在的当前角色：

```json
{
  "replication_jobs": [
    { "endpoint": "primary", "job_name": "LSBackup_OrderMain" },
    { "endpoint": "replica", "job_name": "LSCopy_OrderMain" },
    { "endpoint": "replica", "job_name": "LSRestore_OrderMain" }
  ]
}
```

注入和恢复：

```powershell
curl.exe -sS -X POST http://127.0.0.1:8080/admin/faults/replication `
  -H "Content-Type: application/json" `
  -H "X-Admin-Token: $env:ORDERSYNC_ADMIN_TOKEN" `
  -d '{"action":"disconnect"}'

# ...观察/压测...

curl.exe -sS -X POST http://127.0.0.1:8080/admin/faults/replication `
  -H "Content-Type: application/json" `
  -H "X-Admin-Token: $env:ORDERSYNC_ADMIN_TOKEN" `
  -d '{"action":"reconnect"}'
```

端点优先执行外部命令；否则禁用并停止 SQL Agent 作业，恢复时重新启用并立即启动，使恢复不必等下一次作业计划。`config/local-test.json` 中的 `allow_simulated_replication_stall` 仅用于无复制的路由演示。

### 主库不可用

使用管理员 PowerShell（服务 API 仍在另一个 Python 进程运行）：

```powershell
.\scripts\inject_primary_outage.ps1 -Action stop -Instance SQLEXPRESS_MAIN -Wait
# 写入应返回 E_WRITE_UNAVAILABLE；strong/session/eventual 都不会写备机
.\scripts\inject_primary_outage.ps1 -Action start -Instance SQLEXPRESS_MAIN -Wait
```

恢复后下一次监控采样即恢复主库可达性，写路由无需重启；业务连接池会丢弃已断连接并在后续请求重建。

## 受控切换

`POST /admin/switch` 的顺序固定为：

1. 关闭写闸门并等待已进入的写请求结束，不接受新写。
2. 读取当前主库最后水位。
3. 等待备机应用水位达到该值；超时则中止，保持原路由并重新开放写。
4. 调用 `switch.reverse_command` 完成物理角色反转。
5. 以交换后的主备角色重新执行拓扑校验，失败则调用 `switch.rollback_command`。
6. 交换业务连接池、监控方向和配置中的角色，记录切换次数/耗时，重新开放写。

`scripts/reverse_roles.ps1` 是钩子分发器。通过环境变量设置环境特定的切换脚本：

```powershell
$env:ORDERSYNC_REVERSE_ROLES_COMMAND = "powershell.exe -File C:\ops\reverse-order-replication.ps1"
$env:ORDERSYNC_ROLLBACK_ROLES_COMMAND = "powershell.exe -File C:\ops\rollback-order-replication.ps1"
```

钩子会收到这些环境变量：

- `ORDERSYNC_OLD_PRIMARY_CONNECTION_STRING`
- `ORDERSYNC_OLD_PRIMARY_DATABASE`
- `ORDERSYNC_OLD_REPLICA_CONNECTION_STRING`
- `ORDERSYNC_OLD_REPLICA_DATABASE`
- `ORDERSYNC_REPLICATION_MODE`
- `ORDERSYNC_LAST_COMMIT_WATERMARK`

钩子必须原子地做到：原备机可写、原主库只读、反方向复制已建立；非零退出表示失败。服务在命令返回后按交换后的主备方向重新校验，而不是只相信脚本退出码。`config/local-test.json` 的 `allow_logical_switch` 只用于单元/HTTP 路由测试，不会把物理数据库变成只读，不能用于生产。

## 本地负载与 SLO

负载器只访问 loopback，按种子确定性生成租户/状态混合流量，支持 4 并发、可配置速率和一致性比例：

```powershell
python tools\loadgen.py `
  --base-url http://127.0.0.1:8080 `
  --writes 200 --reads 800 --concurrency 4 --rate 0 `
  --tenant T01 --status CONFIRMED `
  --read-consistency-mix "strong=0.2,session=0.4,eventual=0.4" `
  --seed 271 --run-id R0271 `
  --reset-report `
  --max-write-p95-ms 200 --max-strong-read-p95-ms 100 --enforce-slo
```

`--rate 0` 表示不额外限速，让 4 个 worker 持续施压。`--enforce-slo` 会检查服务报告的写 P95 <= 200ms、strong 读 P95 <= 100ms；失败退出码为 2。结果写入 `artifacts/load-report.json`。

精确重复报告时，先在同一数据库快照/重置状态上使用相同的 `--seed`、`--run-id`、故障注入顺序，并先调用 `--reset-report`。使用 `sql/reset_load_rows.sql` 可清理 `LOAD-` 前缀的压测订单；若还要求水位值和报告完全一致，应恢复数据库快照。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_TOPOLOGY` | 配置不完整、连接失败、结构不一致、水位异常、备机可写或主库不可写 |
| `E_WRITE_CONFLICT` | 订单主键/唯一键冲突或外键失败，事务已回滚 |
| `E_WRITE_UNAVAILABLE` | 当前主库不可达或写事务失败，备机未伪造成功 |
| `E_CONSISTENCY` | 非法一致性等级或备机未达到会话水位 |
| `E_CONN_POOL` | 业务连接池在配置的获取时限内耗尽 |
| `E_READ_UNAVAILABLE` | 路由到的读库不可用且没有可安全回退路径 |
| `E_SWITCHING` | 切换进行中，写闸门关闭 |
| `E_SWITCH_ABORTED` | 切换水位等待、物理反转或反向拓扑校验失败 |
| `E_FAULT_CONTROL` | 复制故障注入命令、作业或暂停标志控制失败 |
| `E_INVALID_REQUEST` | 请求结构、分页或水位头非法 |

## 资源与连接池约束

- 服务本身不启动容器或额外平台；所有进程和 SQL 实例共享单机预算。
- 主库、备机业务连接池各自上限为 `pool_size`。获取超时由 `pool_acquire_timeout_ms` 控制，超时返回 `E_CONN_POOL`，不无限等待。
- 启动校验、复制水位采集和故障作业控制均使用独立直连；采集器不占用主库业务连接池。
- 默认监控间隔 500ms，单轮只做两次短查询，适合 2 核/1.25GiB 预算。
- 订单分页使用 `OFFSET/FETCH` 和租户/状态覆盖索引，单页默认 50、上限 200。

## 测试与验证

当前环境没有 SQL Server 和 pyodbc，SQL 连接部分需在目标 Windows 主机执行。核心逻辑使用可注入数据库接口，在任意 Python 3.11+ 环境运行：

```powershell
$env:PYTHONPATH = "src"
python -m unittest discover -v
```

已覆盖：

- 全量配置校验、缺失环境变量和同端点拒绝。
- 连接池硬上限、获取超时和连接归还。
- 写水位在订单头和订单行一致。
- `session` 水位未达时 `E_CONSISTENCY`，达到后读备机。
- 备机滞后/不可达时 `eventual` 降级读主库且带原因。
- `strong` 在备机故障时仍读主库。
- 受控切换等待最后水位、超时中止、切换后写路由和监控方向反转。
- 报告字段稳定排序、无绝对路径/环境标识、重复事件一致。
- HTTP 写、会话读、错误码和报告重置。

目标机上的最终验收还需运行一次 `validate_topology.ps1`、一次复制中断/恢复场景、一次主库重启场景和一次真实受控切换，并以 `/report` 的计数、降级原因、水位分位和 `compare` 样本为准。

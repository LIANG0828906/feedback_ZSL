# OrderLab 只读订单查询网关

本工作区实现了一个面向 SQL Server Express 2022 的只读查询网关，以及确定性的数据库初始化、双方言回放、索引失效/恢复和结果比较组件。客户端提交原始业务 SQL 与方言标识，网关在内部完成词法安全校验、方言语义归一、覆盖索引选择、计划预检和实际计划复核。

## 已实现能力

- 方言：`mssql-current` 与 `mssql-legacy`，缺失或非法时返回 `E_DIALECT`。
- 语义归一：NULL 排序位置、日期截断到日/秒、字符串与数值比较，以及查询级 `OPTION` hint 清理。
- 只读防线：基于词法 token 拒绝 INSERT、UPDATE、DELETE、MERGE、DROP、EXEC/EXECUTE、SELECT INTO、临时表和多语句；注释、字符串常量、方括号标识符中的关键字不会误判。
- 覆盖索引：只接受 `TenantId + Status` 常量等值前缀且所有输出/谓词列被索引覆盖的查询；同一业务查询在两种方言下选择同一组索引。
- 计划验证：执行前用 `SET SHOWPLAN_XML ON` 预检，执行后用 `SET STATISTICS XML ON` 获取实际计划；拒绝全表/索引扫描、排序溢出、Key Lookup 和未命中选定索引的计划。
- 降级：未覆盖查询可按声明使用 `summary` 读取 `dbo.OrderSummaryDaily`，响应固定标记 `mode=summary`、`aggregate=true`，并返回可复核的 `summary_row_keys`；`reject` 直接返回 `E_INDEX_UNCOVERED`。
- 连接控制：全局与租户级双层信号量，等待有上限；连接不可用或达到上限返回 `E_CONN`。状态接口显示当前占用、拒绝/降级计数和降级追踪。
- 回放：固定样本覆盖索引命中、未覆盖列、需降级、非法写入四类，双方言并发提交；结果记录字段稳定、按 `query_id/dialect` 排序且不含绝对路径。
- 索引可用性变更：禁用覆盖索引后依赖查询以 `E_INDEX_UNAVAILABLE` 拒绝且不执行；恢复后与基线逐行一致。

## 目录

```text
src/orderlab_gateway/   网关、词法、归一、索引分析、计划校验、限流、回放
sql/                    建库、表、固定种子数据、索引、只读账号
config/gateway.json     Linux/共享实例 SQL 登录配置
config/gateway.windows.json  Windows LocalDB 集成身份配置
config/samples.json     固定回放样本与双方言 SQL
scripts/run_evaluation.py   一键端到端评测
tests/                  单元测试与可选真实库集成测试
artifacts/              已生成的回放、比较和最终评测记录
```

## Windows LocalDB 运行

前置条件：Windows 10/11 或 Windows Server 2022、Python 3.11、`pyodbc`、`pytest`、ODBC Driver 18 for SQL Server，以及可通过 `(localdb)\MSSQLLocalDB` 连接的 SQL Server Express 2022。

```powershell
python -m pip install -r requirements.txt
python -m pip install -e .
sqlcmd -S "(localdb)\MSSQLLocalDB" -E -Q "SELECT @@VERSION"
python -m orderlab_gateway.cli --config config/gateway.windows.json init-db
python -m orderlab_gateway.cli --config config/gateway.windows.json serve
```

默认监听 `127.0.0.1:8780`。`gateway.windows.json` 使用本机集成身份；`init-db` 会建立独立数据库 `OrderGateway280`，不会触碰同实例上的其他数据库。

共享实例/SQL 登录方式使用 `config/gateway.json`，先设置环境变量：

```bash
export ORDERLAB_DB_PASSWORD='...'
export ORDERLAB_ADMIN_PASSWORD='...'
python -m orderlab_gateway.cli init-db
python -m orderlab_gateway.cli serve
```

`ORDERLAB_HOST`、`ORDERLAB_PORT` 可覆盖监听地址和端口。

## API

`POST /query`

```json
{
  "sql": "SELECT TOP (5) h.OrderNo FROM dbo.OrderHeader h WHERE h.TenantId=1 AND h.Status='PAID' ORDER BY h.OrderDate",
  "dialect": "mssql-current",
  "query_id": "HDR_PAGE_DETAIL",
  "fallback": "reject"
}
```

明细成功响应包含 `mode=detail`、`columns`、`rows`、`result_digest`、`index_names`、`access`、`plan_rows`。汇总成功响应包含 `mode=summary`、`aggregate=true`、`degradation`、`summary_row_keys`。

`POST /replay` 请求体支持 `concurrency`（默认 2、上限 2）和 `repeats`。`GET /status` 返回连接占用、错误/降级计数和查询标识到汇总行的追踪。`GET /health` 用于存活检查。

业务错误统一使用 HTTP 200 的 `{"ok":false,"error":{...}}`，关键错误码：

| 错误码 | 含义 |
|---|---|
| `E_DIALECT` | 方言缺失或非法 |
| `E_READONLY` | 非只读/多语句/临时表 |
| `E_CONN` | 连接不可用、全局或租户连接上限耗尽 |
| `E_INDEX_UNCOVERED` | 无覆盖索引且声明拒绝 |
| `E_INDEX_UNAVAILABLE` | 选定索引被禁用或不存在 |
| `E_PLAN_SCAN` | 计划含扫描 |
| `E_PLAN_INDEX` | 未命中选定索引或出现 Key Lookup |
| `E_PLAN_SORT_SPILL` | 排序溢出到 tempdb |
| `E_SUMMARY_UNAVAILABLE` | 降级缺少租户/状态键 |

## 索引边界

| 表 | 覆盖索引 | 键 | INCLUDE |
|---|---|---|---|
| OrderHeader | `IX_OrderHeader_Tenant_Status_OrderDate` | TenantId, Status, OrderDate, OrderId | OrderNo, CustomerName, Amount, Currency, UpdatedAt |
| OrderHeader | `IX_OrderHeader_Tenant_Status_OrderNo` | TenantId, Status, OrderNo | OrderDate, OrderId, CustomerName, Amount, Currency, UpdatedAt |
| OrderLine | `IX_OrderLine_Tenant_Status_OrderId` | TenantId, Status, OrderId, LineNum | Sku, Quantity, UnitPrice, LineAmount, UpdatedAt |
| OrderLine | `IX_OrderLine_Tenant_Status_Sku` | TenantId, Status, Sku | OrderId, LineNum, Quantity, UnitPrice, LineAmount, UpdatedAt |

另有非覆盖索引 `IX_OrderHeader_CustomerName` 与 `IX_OrderLine_ProductNote`。网关选择候选索引后会添加内部 `WITH (INDEX(...))` 表提示，确保两种方言实际使用同一索引，再以预检和实际计划双重验证。

兼容边界：归一器不是完整 T-SQL 解析器，针对样本中的 TOP/OFFSET-FETCH、CROSS APPLY、查询级 hint、日期截断和已知字符串/数值列比较做显式转换。无法证明覆盖或语义安全的查询会失败关闭，不会猜测执行。

## 数据与评测

`sql/20_seed.sql` 固定生成 `OrderHeader` 20,000 行、`OrderLine` 20,000 行；汇总表按租户/状态/日期聚合，当前为 5,840 行。数据值只由行号、固定日期基准和取模运算决定，不依赖随机数。

一键评测：

```bash
python scripts/run_evaluation.py --config config/gateway.json
```

脚本会依次生成：

- `artifacts/replay_baseline.json`
- `artifacts/replay_index_disabled.json`
- `artifacts/replay_restored.json`
- `artifacts/replay_comparison.json`
- `artifacts/evaluation_report.json`

也可以手工构造索引可用性变更：

```bash
python -m orderlab_gateway.cli index --name IX_OrderHeader_Tenant_Status_OrderDate --state disabled
python -m orderlab_gateway.cli replay --output artifacts/replay_index_disabled.json --concurrency 2
python -m orderlab_gateway.cli index --name IX_OrderHeader_Tenant_Status_OrderDate --state enabled
python -m orderlab_gateway.cli replay --output artifacts/replay_restored.json --concurrency 2
python -m orderlab_gateway.cli compare
```

单元测试不依赖数据库：

```bash
pytest -m "not integration"
```

真实库集成测试：

```bash
ORDERLAB_INTEGRATION=1 pytest -m integration
```

## 已执行结果

本地 SQL Server 2022 实例上，16 条回放记录（8 个查询 × 2 方言）结果如下：索引命中类 10/10 成功且均为 Index Seek；汇总类 2/2 以 `mode=summary` 降级并带汇总行键；未覆盖类 2/2 被 `E_INDEX_UNCOVERED` 拒绝；非法写入类 2/2 被 `E_READONLY` 拒绝。禁用 `IX_OrderHeader_Tenant_Status_OrderDate` 后 8 条依赖记录以 `E_INDEX_UNAVAILABLE` 拒绝，恢复后与基线逐行一致；非法写入前后 `OrderHeader` 均为 20,000 行。最终检查见 `artifacts/evaluation_report.json`，`passed=true`。

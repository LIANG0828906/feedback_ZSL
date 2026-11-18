# 数仓变更影响判定服务

这是一个仅依赖 Python 3.11 标准库（`sqlite3`、`http.server`、`json`）的本地服务。服务读取任务 SQLite 元数据，在启动时完成列级血缘校验，并提供变更提交、影响查询、数值漂移核对、回滚计划和审计接口。

## 快速启动

工作区已经包含一个由固定种子生成、没有运行时测试记录的 `metadata.db`。也可以随时重新生成：

```bash
python generate_metadata.py --db metadata.db
```

启动服务（默认地址 `http://127.0.0.1:8008`）：

```bash
python lineage_service.py --db metadata.db --host 127.0.0.1 --port 8008
```

Windows 可直接运行 `run_service.bat`；Linux/macOS 可运行 `./run_service.sh`。

监听端口在启动时显式输出。默认使用 `8008`，可用 `--port` 改为 `8000` 至 `8010` 中任一空闲端口。服务使用 `HTTP/1.1`，响应均为 JSON。

## 固定数据模型

生成器使用固定种子 `20261119`，默认生成 200 个客户、48 个商品、600 个订单和约 1500 条订单行。可在生成时通过 `--seed`、`--customers`、`--products`、`--orders` 缩小样本。

- 12 张表：4 张原始层、2 张明细层、6 张汇总层。
- 40 个顺序 SQL 任务：原始校验、明细构建/刷新、六类汇总构建/刷新/发布。
- 7 个报表配置，其中执行报表映射到两个汇总任务。
- 8 个汇总指标：净销售额、毛销售额、销量、订单数、客单价、税额、折扣率、区域集中度。
- 任务定义、最近一次成功运行元数据、表结构快照、行数和确定性数据都在同一个 SQLite 库中。

明细层复合变更的验收样例：

```json
{
  "table_name": "detail_order_items",
  "column_name": "unit_price",
  "change_type": "type_narrow",
  "new_type": "TEXT(2)",
  "threshold_pct": 0.5
}
```

该样例默认库上会得到 22 个受影响任务（1 个直接、21 个传导）、7 个受影响报表，并使 5 个指标超过阈值。删除同列时漂移更大；因为仍有任务直接引用该列，回滚计划会按规范返回 `E_ROLLBACK_BLOCKED`，且不会输出不完整的任务列表。

## HTTP 接口

### 1. 提交变更

`POST /api/changes`

请求字段：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `table_name` | 是 | 表名，也接受 `table` |
| `column_name` | 是 | 列名，也接受 `column` |
| `change_type` | 是 | `delete_column` 或 `type_narrow` |
| `new_type` | 类型收窄时可选 | 目标 SQL 类型；省略时按当前类型推导 |
| `threshold_pct` | 否 | 漂移阈值，0 到 100 的百分数；省略时为 0，即任何非零偏差均标记漂移 |
| `task_name` | 否 | 若提供则校验任务存在 |

返回包括 `change_id`、`change`、`impact`、受影响任务、受影响报表、直接/传导数量和回滚顺序。相同表、列、变更类型和目标类型会生成相同 `change_id`，重复提交不会覆盖第一次的影响判定。

### 2. 查询影响

`GET /api/changes/{change_id}/impact`

任务报告中每项包含：

- `impact_type`：`direct` 或 `transitive`。
- `trigger_columns`：触发该任务的受影响列。
- `lineage_paths`：从变更列到触发列的列级路径。
- `depth`：任务依赖深度。
- `rollback_order`：按依赖深度逆序排列，数值越大越先回滚。

直接读取变更列的任务优先标记为 `direct`，即使它同时存在传导路径也不会重复计数。

### 3. 漂移核对

`POST /api/changes/{change_id}/drift-check`

可选请求体：

```json
{"threshold_pct": 0.5}
```

服务在内存数据库中分别重放变更前和变更后任务链，执行全部 8 个指标查询：

- 返回 `before`、`after`、`absolute_drift`、`relative_deviation_pct`。
- 相对偏差为 `abs(after-before) / abs(before) * 100`；变更前为 0 且变更后非 0 时记为 100%。
- 超过阈值的指标状态为 `E_DRIFT`。
- 每个漂移指标返回 `first_drift_table`、`first_drift_layer`、`lineage_trace` 和 `drift_tables`，可沿血缘定位最早出现差异的中间表。

相同变更和阈值重复核对返回同一个 `drift_check_id`。不同阈值会新增审计历史，不覆盖旧记录。

漂移核对不会修改元数据库中的表结构快照。删除列在隔离内存库中以 `NULL` 值掩码保留原列名，以便继续执行下游任务并量化数值影响；真实回滚仍会识别所有直接引用该列的任务并阻断计划。类型收窄同样在隔离副本中执行确定性取值变换后重放任务。

### 4. 回滚计划

`GET /api/changes/{change_id}/rollback-plan`

返回按依赖逆序排列的完整任务列表、`dependency_depth`，以及回滚后必须重新核对的指标集合。

若受影响任务依赖图存在环，或删除列后的回滚任务仍引用该列，返回 HTTP `409` 与 `E_ROLLBACK_BLOCKED`。响应只包含阻断原因和待核对指标，`rollback_tasks` 固定为空，避免误执行不完整计划。

### 5. 审计

`GET /api/changes/{change_id}/audit`

返回变更声明、影响判定历史、不同阈值的漂移核对历史、回滚计划历史和完整审计事件。服务重启后可直接继续查询。处于 `COMPUTING` 的变更在查询影响时会继续完成确定性判定。

### 其他查询

- `GET /health`：健康状态、快照摘要和实体数量。
- `GET /api/tasks/{task_name}`：任务读写列、依赖和最近成功运行信息。
- `GET /api/tables/{table_name}`：列类型、行数、读取任务和写入任务。
- `GET /` 或 `GET /openapi.json`：接口摘要。

为便于调用，同时支持 `/changes/{id}/impact`、`/impact/{id}`、`/changes/{id}/drift`、`/drift/{id}`、`/rollback/{id}`、`/audit/{id}` 等别名，以及带查询参数 `change_id` 的扁平路由。

## 业务错误码

| 错误码 | 含义 |
| --- | --- |
| `E_UNKNOWN_REF` | 任务 SQL 引用了快照中不存在的表或列 |
| `E_UNKNOWN_TABLE` | 请求中的表不存在 |
| `E_UNKNOWN_COLUMN` | 请求中的列不存在 |
| `E_UNKNOWN_TASK` | 请求中的任务不存在 |
| `E_INVALID_CHANGE_TYPE` | 变更类型非法 |
| `E_INVALID_TYPE` | 类型收窄目标类型非法 |
| `E_THRESHOLD_RANGE` | 阈值不是 0 到 100 内的有限数值 |
| `E_ROLLBACK_BLOCKED` | 回滚依赖成环或仍引用已删除列 |
| `E_NOT_FOUND` | 变更标识或历史记录不存在 |
| `E_INVALID_REQUEST` | 请求字段缺失或请求体非法 |
| `E_INVALID_JSON` | JSON/UTF-8 解析失败 |
| `E_INTERNAL` | 未预期服务端异常，响应不包含 SQLite 文件路径 |

SQL 血缘生成是“全有或全无”的：任一任务引用未知对象，服务会记录该任务名和缺失对象，所有业务接口返回 `E_UNKNOWN_REF`，不会跳过任务生成部分血缘。

## 事务与并发

- 每次变更声明先以 `COMPUTING` 落库；影响结果、状态更新和 `impact_completed` 审计事件在同一 `BEGIN IMMEDIATE` 事务中提交。
- `change_requests.fingerprint` 唯一，影响结果主键为 `change_id`，相同变更不会覆盖已提交判定。
- 漂移结果按 `(change_id, threshold_pct)` 唯一，回滚计划按 `change_id` 唯一。
- HTTP 层最多同时处理 6 个请求；漂移核对使用进程内 SQLite 内存副本，不创建大临时文件。
- 单进程内存和数据库远低于 1.25 GiB，不依赖第三方数据库或网络访问。

## 验收

```bash
python acceptance_test.py
```

脚本使用独立临时库和 `8010` 端口，覆盖：

- 12 表/40 任务/8 指标启动校验。
- 列级血缘、直接/传导分类和去重计数。
- 相同变更与并发提交的幂等结果。
- 8 指标漂移、E_DRIFT 和首个漂移表定位。
- 逆依赖回滚顺序和删除列回滚阻断。
- 阈值、表、列、任务和变更类型的业务错误。
- 影响与审计事务持久化、服务重启后查询。
- 元数据注入未知列后的 `E_UNKNOWN_REF` 全服务阻断。

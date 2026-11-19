# iovflow

面向车联网分钟批 JSONL 事件流的小型事件时间 SQL 引擎。它将一条受支持的 SQL
编译成可查询的有向无环算子图，在同一个单机进程模型里同时提供批模式和流模式，
并用 SQLite 保存阶段提交点、窗口累计、水印、迟到计数和作业报告。

目标环境是 Windows 11、2 核 CPU、1.25 GiB 内存、Python 3.11。运行时只依赖
标准库和 DuckDB（DuckDB 只用于 SQL 解析，不用于执行）。

## 快速开始

```bash
python -m pip install -r requirements.txt

# 固定种子生成 96 批、80,000 行 JSON Lines 与清单
python -m iovflow gen --out data --rows 80000 --batches 96

# 查看 SQL 被拆分出的 DAG
python -m iovflow plan --sql sql/window_5m.sql

# 批模式
python -m iovflow run --sql sql/window_5m.sql --input data --mode batch \
  --job-id demo-batch --allowed-lateness 300

# 流模式
python -m iovflow run --sql sql/window_5m.sql --input data --mode stream \
  --job-id demo-stream --allowed-lateness 300

# 查询阶段、并行度、水印、接管次数和结果计数
python -m iovflow status --job-id demo-stream

# 逐窗口比较两个作业
python -m iovflow verify --job-a demo-batch --job-b demo-stream \
  --state-db state/iovflow_state.db --results-db results/results.db
```

默认路径：

- 输入：`data/`
- 状态库：`state/iovflow_state.db`
- 结果库：`results/results.db`
- 临时目录：`tmp/`
- 报告：`reports/<job-id>.json` 与 `reports/latest.json`

## 算子 DAG

SQL 只接受以下形状：

```sql
SELECT
    vehicle_id,
    window_start(event_time, 300) AS window_start,
    COUNT(*) AS event_count,
    COUNT(DISTINCT trip_id) AS trip_count,
    SUM(mileage_delta) AS mileage_sum,
    SUM(battery_delta) AS battery_sum
FROM events
WHERE event_type IN ('payment', 'backfill') AND speed >= 0
GROUP BY vehicle_id, window_start
```

编译后的阶段链固定为：

```text
source
  -> filter              (logical shards = 4, max concurrent = 2)
  -> group_by_vehicle    (logical shards = 4, max concurrent = 2)
  -> window_aggregate    (logical shards = 4, max concurrent = 2)
  -> merge_skew          (parallelism = 1)
  -> sink                (parallelism = 1)
```

`stages` 表记录每个阶段的 `input_from`、`output_to`、`stage_type`、
`parallelism`、`max_concurrent`、`consumed_batch`、`committed_batch` 和水印。
`status` 命令直接返回这些记录。

### 支持的窗口写法

```sql
window_start(event_time, 300)
time_bucket(INTERVAL '5 minutes', event_time)
to_timestamp(floor(epoch(event_time) / 300) * 300)
```

支持的聚合：`COUNT(*)`、`COUNT(1)`、`COUNT(col)`、`COUNT(DISTINCT col)`、
`SUM`、`MIN`、`MAX`、`AVG`（含 `DISTINCT` 形式）。过滤支持比较、`AND`/`OR`、
`IN`/`NOT IN`、`BETWEEN`、`IS [NOT] NULL`、`LIKE` 和常见确定性标量函数。

### 拒绝而非串行降级

无法归入“过滤 / 按车辆分组 / 事件时间窗口聚合”的结构会在计划阶段被拒绝，
错误响应包含稳定业务码和 `fragment` 片段。例如：

```json
{
  "ok": false,
  "error": {
    "code": "IOV_SQL_UNSUPPORTED_JOIN",
    "message": "JOIN sources are not supported; the plan is a single event stream",
    "fragment": "join vehicles v on e.vehicle_id=v.vehicle_id"
  }
}
```

覆盖的拒绝码：

- `IOV_SQL_UNSUPPORTED_JOIN`
- `IOV_SQL_UNSUPPORTED_SUBQUERY`
- `IOV_SQL_UNSUPPORTED_CTE`
- `IOV_SQL_UNSUPPORTED_SET_OPERATION`
- `IOV_SQL_UNSUPPORTED_WINDOW`（`OVER`、排名/偏移窗口函数等）
- `IOV_SQL_UNSUPPORTED_GROUP`
- `IOV_SQL_UNSUPPORTED_AGGREGATE`
- `IOV_SQL_UNSUPPORTED_STRUCTURE`
- `IOV_SQL_UNSUPPORTED_EXPRESSION`

## 输入与去重

`gen` 生成 `data/manifest.json` 和 `data/events/batch_000000.jsonl` 等 96 个
文件。清单中每批包含声明事件时间 `declared_event_time`，它用于推进水印，而不是
用到达顺序推进。

默认自然去重键为：

```text
vehicle_id,trip_id,event_time,event_type
```

可通过 `--dedupe-key` 修改。去重与过滤在同一阶段完成；重复上报只计一次。
固定种子样本的校验值约为：80,000 行、96 批、4.00% 重复、2.99% 延迟事件、
批内和批间均存在乱序。

如果输入目录没有清单，引擎会按文件名排序，并以每批最大事件时间作为声明时间，
仍然使用同一套窗口和水印实现。

## 水印、迟到和输出

水印定义为：

```text
watermark = max(已消费批次的 declared_event_time) - allowed_lateness
```

窗口为左闭右开的 5 分钟事件时间窗口。批次处理完成后，`window_end <= watermark`
的窗口被关闭。关闭后到达的同车同窗口事件只写入 `late_windows`，不修改累计值，
也不修改已最终输出的结果。输入结束后会以无穷大水印执行一次 flush，使所有剩余
窗口最终关闭；因此批模式和流模式的最终窗口集合和值逐字段一致。

流模式在每批结束后发布新增或更新的窗口版本：

```sql
SELECT * FROM window_results
WHERE job_id = 'demo-stream' AND is_final = 1;
```

`window_results` 每个 `(job_id, vehicle_id, window_start_epoch)` 只有一行。
未关闭窗口的临时版本为 `is_final = 0`；下游结算应过滤 `is_final = 1`。
`result_versions` 保存发布版本审计，但当前结果行不会重复。常用聚合别名会作为
动态列加入 `window_results`，同时完整结果保存在 `result_json` 中。

批模式在全部输入消费并 flush 后一次发布最终结果；流模式逐批发布，但两种模式的
最终 `result_json`、迟到计数和水印历史一致。

迟到计数查询：

```sql
SELECT * FROM late_windows WHERE job_id = 'demo-stream';
```

## 分区、倾斜和合并

预扫描阶段在去重后按车辆统计事件数，然后用确定性的 LPT 分配把车辆放到 4 个逻辑
分片。正常情况下同一车辆（包括支付侧和补录侧事件）始终落在同一分片。

若某车辆超过分片上限（默认总事件数的 40%），其行程被拆成多个子分片；同一行程的
支付/补录事件仍进入同一子分片。若单个行程本身仍是热键，则再按事件指纹切分。
`merge_skew` 按 `(vehicle_id, window_start)` 汇总所有子分片，保证只有一份窗口
结果。路由计划是输入的函数，不依赖批次完成顺序，因此恢复后不会改变分片。

报告中可查询 `routing.shard_counts`、`routing.shard_ratios`、
`routing.max_shard_ratio` 和 `routing.split_vehicles_sample`。

## 提交、恢复与故障接管

提交单位：

- `filter` + `group_by_vehicle`：过滤结果、去重键、路由文件和阶段批次在同一
  SQLite 事务中提交。
- `window_aggregate`：每个逻辑分片在一个事务中提交窗口累计、水印、已消费批次、
  已关闭窗口和部分聚合；全部逻辑分片提交后，阶段批次才推进。
- `merge_skew`：完成窗口、最终结果和 outbox 在同一事务中提交。
- `sink`：结果库使用主键 upsert，outbox 和版本审计保证重复发布幂等。

上游过滤阶段会预取下一批，因此窗口分片接管期间上游仍可推进；`merge_skew` 必须
等到本批所有窗口分片提交后才会读取累计值，不会读取缺失或置零状态。

作业被外部终止后，用同一个 `--job-id` 重跑（或显式 `--resume`）即可从提交点继续：

```bash
python -m iovflow run ... --job-id demo-stream --resume
```

已完成窗口不会重新计算或二次输出；重复回到同一提交点只做幂等发布。
报告的 `checkpoint.last_committed_batch`、`checkpoint.resumed_from_batch` 和
`fault_recovery.events` 记录恢复阶段与提交点。

### 阶段进程终止与无进展超时

`job_workers` 表记录每个作业的 worker PID、当前任务和心跳；可以用任务管理器、
`kill` 或 `TerminateProcess` 杀死某个 worker。编排器检测到进程死亡后，把该分片
重新排给存活 worker 或重启实例，并增加 `stage_shards.takeover_count`。

```bash
# 列出当前作业 worker
python scripts/fault_inject.py --job-id demo-stream --list

# 按当前任务或 worker id 终止一个真实阶段进程
python scripts/fault_inject.py --job-id demo-stream --task window_aggregate:40
```

也可用配置注入真实进程故障（不修改引擎源码）：

```bash
# 在 window_aggregate 第 40 批 shard 0 上杀死执行进程
python -m iovflow run ... \
  --inject-kill "stage=window_aggregate,batch=40,shard=0" \
  --inject-delay-ms 0

# 暂停进程，触发无进展超时后由编排器终止并重派
python -m iovflow run ... --no-progress-timeout 2 \
  --inject-hang "stage=window_aggregate,batch=40,shard=0"
```

接管次数可通过 `status`、报告中的 `fault_recovery.takeover_count` 和
`stage_shards.takeover_count` 查询。

## 资源限制、溢写和稳定性

- 计算 worker 数硬限制为 2；窗口阶段的 4 个分片是逻辑分片，最多 2 个并发执行。
- 默认内存上限 `--memory-limit-mb 1024`，采样父进程和 worker 的总 RSS。
- 默认临时数据上限 `--temp-limit-mb 2048`。
- 窗口累计缓存超过 `--state-memory-mb`（默认 32 MiB）时，缓存驱逐到本地
  SQLite 状态存储并计入 `resources.spill_bytes`；作业成功后删除临时路由和
  已消费窗口状态。
- 超出内存或临时存储上限时分别返回 `IOV_RESOURCE_MEMORY` /
  `IOV_RESOURCE_TEMP`，保留已完成窗口和状态，作业报告标记 `failed`。

实测（本工作区、80,000 行、96 批、2 worker、4 个逻辑窗口分片）：

| 指标 | 结果 | 验收上限 |
|---|---:|---:|
| 批模式端到端 | 约 24.5 s | 180 s |
| 流模式单批 P95 | 约 0.33 s | 2 s |
| 峰值进程树 RSS | 约 212 MiB | 1024 MiB |
| 临时数据峰值 | 约 50 MiB | 2048 MiB |
| 最大分片占比 | 约 25.0% | 40% |
| 最终窗口数 | 22,059 | — |

## 结果库和报告

主要表：

- `window_results`：每车每窗口一行，包含 `is_final`、`version_batch`、
  `publish_seq`、规范时间列、动态聚合列和 `result_json`。
- `result_versions`：发布版本审计。
- `late_windows`：窗口关闭后到达的事件计数。
- `job_reports`：完整 JSON 报告。

报告包含：

- `stages` / `shards`：DAG、并行度和各分片提交位置。
- `routing`：分片计数、占比、倾斜拆分明细。
- `watermark`：配置容忍度、最后真实水印、flush 水印和历史条数。
- `checkpoint`：最后提交批次、恢复起点。
- `fault_recovery`：接管次数和每次接管的事件、阶段、批次、旧 PID。
- `results`：完成窗口、最终输出行、发布版本、迟到窗口/事件。
- `resources`：峰值内存、溢写字节、临时数据峰值。
- `performance`：批次耗时 P95、最大值和端到端耗时。

## 可查询接口

```bash
# 作业与阶段状态
python -m iovflow status --job-id demo-stream

# 任意已消费批次后的确定性快照：水印、开放窗口累计、已完成窗口
python -m iovflow state --job-id demo-stream --batch 40

# 单车最终窗口与迟到计数
python -m iovflow query --job-id demo-stream --vehicle V0001
```

`--metrics-port 9109` 可启动 Prometheus 指标端点（作业状态、阶段提交批次、
接管数、完成窗口、迟到事件、溢写字节、峰值内存和批次 P95）。

## 测试与验收

```bash
# 21 个单元/集成测试
python -m unittest discover -s tests -t .

# 完整验收：批/流一致性、独立参考核对、阶段进程终止、迟到语义、
# 外部终止 + 恢复、无重复输出
python scripts/run_acceptance.py

# 快速样本
python scripts/run_acceptance.py --quick
```

验收脚本把结果写入 `artifacts/acceptance-<timestamp>/acceptance_report.json`，
其中逐项列出 `checks`。

## 代码结构

```text
iovflow/
  cli.py            命令行入口
  generator.py      固定种子输入
  sql_parser.py     DuckDB AST -> 三段式逻辑计划
  expr.py           确定性标量表达式解释器
  routing.py        去重后的倾斜感知路由
  stages.py         过滤与窗口分片执行
  worker.py         2 进程持久 worker 池、接管和超时
  orchestrator.py   DAG 编排、合并、发布、恢复、报告
  storage.py        SQLite 状态/结果事务
  verify.py         逐窗口核对
  accum.py          聚合状态与确定性合并
sql/window_5m.sql   固定 SQL 模板
scripts/            生成器与验收脚本
tests/              unittest 测试
```

## Windows 11 说明

- 使用 `python -m iovflow` 运行；`multiprocessing` 在 Windows 上使用 `spawn`，
  状态全部通过 SQLite 传递，不依赖 fork。
- worker 进程由父进程通过 `Process.kill()`/任务管理器终止，恢复路径与 POSIX
  相同。
- 临时目录、状态库和结果库应放在同一台本地磁盘；不要放在网络共享上。
- 2 核限制由 worker 池大小强制为 2；4 个窗口分片仍可查询，但最多 2 个并发。


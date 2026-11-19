# 车队遥测夜间能耗结算执行器

这是一个单机、可续跑、本地两阶段发布的 SQL 结算执行器。它把按天分区的
Parquet 遥测分片与 96 个 JSON Lines 充电补录批次统一为逻辑关系
`vehicle_events`，按 `vehicle_id + trip_id` 关联，再按 SQL 指定的时间窗口和
车辆聚合。

实现不启动数据库服务、消息系统或集群。状态、检查点和结算结果都在本地
SQLite 中；临时溢写目录在作业结束、失败或被按批次终止后清理。

## 安装

要求 Python 3.11+、DuckDB 1.x/2.x、PyArrow 14+。

```bash
python -m pip install -e .
```

也可以直接在本仓库运行，`python -m settlement` 不需要额外安装步骤。

## 快速验证

```bash
# 1. 固定种子数据：8 万遥测行、3 万补录行、96 批、约 5% 重复、约 3% 延迟
python -m settlement generate --out data --overwrite

# 2. 查看保守下推计划
python -m settlement plan \
  --sql examples/settlement.sql \
  --data data \
  --out plan.json

# 3. 同时运行 Parquet 下推路径和不做扫描下推的回放路径，逐车比较
python -m settlement verify \
  --sql examples/settlement.sql \
  --data data \
  --db settlement.db \
  --temp-dir .settlement_tmp \
  --report-out job_report.json

# 4. 查看已发布结果
python -m settlement inspect --db settlement.db --mode pushdown --limit 20
python -m settlement report --db settlement.db --mode pushdown
```

在本机 WSL 环境实测默认数据两路完整执行约 20 秒，峰值 RSS 约 360 MiB；
Windows 主机仍受 `--memory-limit-mb 1024`、`--temp-limit-mb 2048` 和
`--cpu-workers 2` 约束。

## SQL 契约

SQL 必须从逻辑关系 `vehicle_events` 读取，按车辆和窗口分组，并投影车辆、
窗口、里程增量、电量增量及百公里能耗。支持：

- `date_trunc('hour'|'day'|'minute', event_time)`；
- `time_bucket('15 minutes', event_time)`；
- `CAST(event_time AS DATE)` / `DATE(event_time)`；
- `WHERE` 中由 `AND` 连接的比较、`IN`、`LIKE`、`BETWEEN`、`IS NULL`；
- 车辆、窗口、里程、电量别名；
- `SUM(mileage_delta)`、`SUM(energy_delta)` 和
  `SUM(energy_delta) * 100 / NULLIF(SUM(mileage_delta), 0)` 的结算口径。

示例见 `examples/settlement.sql`。顶层 `OR`、`HAVING`、`LIMIT`、未列出的
逻辑源和额外分组键会被明确拒绝，避免静默产生与提交 SQL 不一致的结果。

字段：

```text
vehicle_id, trip_id, event_time, mileage_delta, energy_delta,
partition_date, source_type, event_id/record_id
```

## 保守下推边界

只有与窗口关闭、水位和迟到判定无关的谓词可以下推到 Parquet 扫描侧：

- `partition_date`/分区列；
- `vehicle_id`；
- `trip_id`；
- 来源等逻辑键（经引擎过滤，不伪装成 Parquet 物理列）。

`event_time`、`window_start`、迟到标志和任何依赖窗口关闭的条件一律保留到
水位推进后再判断。这样迟到补录不会在扫描侧被错误地挡在窗口外。计划和报告
都会分别列出 `pushed_predicates` 与 `retained_predicates`。

同一 SQL 的两条路径：

- `pushdown`：Parquet 数据集使用安全分区/键过滤；
- `replay`：全量读取后在引擎内应用同一谓词；

两者进入完全相同的去重、水位、窗口和聚合流程。所有里程、电量以百万分之一
为单位的整数累计，合并顺序不会造成浮点漂移。

## 去重、迟到与超时语义

- 去重键：`source_type + vehicle_id + trip_id + window_start + source_id`；
  无来源 ID 时退回事件时间/里程/电量业务键。首次确定性到达者保留。
- 补录只有在其 `vehicle_id + trip_id` 已出现在遥测侧时参与结算；孤立补录
  计数并记录。
- 水位：`batch.available_at - allowed_lateness_seconds`。
- `event_time < watermark` 的补录计为迟到并丢弃，明细写入 `event_issues`，
  可按车辆、行程号和窗口定位。两路迟到集合必须一致。
- 窗口在 `window_end <= watermark` 后关闭；已关闭窗口不再累积。

## 并行、倾斜与资源

- 默认 5 个逻辑处理槽，`--cpu-workers` 最大为 2；逻辑槽下限为 3，以便
  满足单槽事件数不超过 40% 的约束。
- 遥测和补录按车辆/行程/窗口的确定性路由进入槽；普通车辆的两侧事件在
  同一槽。
- 超过 `--skew-threshold`（默认 0.08）的车辆按
  `(vehicle_id, trip_id, window_start)` 单元拆分到多个槽，同时保证同一单元的
  两侧事件路由一致，合并后再按车辆/窗口精确汇总。
- 批次缓冲超过 `--spill-threshold-mb` 时溢写到临时 JSON Lines，字节数进入
  报告；临时目录在 `finally` 中清理。
- 工作集超过 `--memory-limit-mb` 时以稳定错误码 `E_MEMORY` 终止，已提交
  窗口和检查点保留，可直接重跑续行。

## 检查点与外部终止

每个批次在同一 SQLite 事务中提交：

- 消费到的批次号；
- 水位；
- 去重集合；
- 未关闭窗口的局部预聚合；
- 已关闭窗口的预提交明细和窗口清单；
- 提交序号和批次尝试记录。

模拟外部终止（同一 SQLite 库中该批次只停止一次，`--reset` 会清除该标记）：

```bash
python -m settlement run \
  --sql examples/settlement.sql --data data --db settlement.db \
  --mode pushdown --stop-after-batch 37

# 返回码 75。重新执行同一命令即可从批次 37 的已提交检查点继续；
# 已提交窗口不会重算，最终结果与不间断运行逐车一致。
python -m settlement run \
  --sql examples/settlement.sql --data data --db settlement.db \
  --mode pushdown --stop-after-batch 37
```

模拟批次失败：

```bash
python -m settlement run \
  --sql examples/settlement.sql --data data --db settlement.db \
  --mode pushdown --fail-batch 50

# 第一次返回 E_INJECTED_BATCH_FAILURE，可见结果仍为空；
# 再执行一次，该批次第二次尝试成功，失败与重试都留在 batch_attempts 中。
python -m settlement run \
  --sql examples/settlement.sql --data data --db settlement.db \
  --mode pushdown --fail-batch 50
```

## 两阶段发布

窗口关闭时只写 `stage_vehicle_energy` 和 `stage_window_manifest`。所有 96 批
都消费完、所有窗口都关闭后，单个事务把暂存数据写入：

- `vehicle_energy`：每车每窗口明细；
- `vehicle_energy_total`：每车跨窗口汇总与百公里能耗；
- `window_manifest`：已发布窗口清单。

发布完成前，下游查询 `vehicle_energy`/`vehicle_energy_total` 看不到本 run 的
任何行。批次失败会回滚该批次事务，不改变先前成功窗口；重试使用唯一主键，
不会产生同一车辆同一窗口的第二条明细。

审计表包括：

- `batch_attempts`：每次批次尝试、状态和错误码；
- `event_issues`：重复、迟到、孤立补录明细；
- `checkpoints`：每批水位和提交序号；
- `resume_events`：恢复所用提交点；
- `verification_results`：下推/回放逐窗、逐车比较和迟到/去重差异；
- `job_reports`：稳定排序的 JSON 报告。

## 报告

`job_report.json` 包含输入文件与批次计数、下推/保留谓词、局部预聚合粒度、
去重丢弃数、迟到明细、倾斜拆分、槽位事件占比、检查点与恢复次数、溢写字节、
峰值内存，以及两路逐车差异。相同输入和配置的 `stable_hash` 在排除
`runtime`、时间戳和运行时 RSS 后保持一致。

单模式报告直接位于 JSON 顶层；`verify`/`--mode both` 的报告使用
`pushdown`、`replay` 和 `verification` 三个顶层键，便于复核两路差异。

## 测试

```bash
python -m unittest discover -s tests -v
```

测试覆盖：SQL 安全下推、两路逐字段一致、5%/3% 去重与迟到、槽位 40% 上限、
倾斜拆分、两阶段不可见、失败重试唯一性、按批恢复、溢写清理和 `E_MEMORY`。

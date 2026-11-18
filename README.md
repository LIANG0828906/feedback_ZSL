# 车联网遥测统一分析引擎（telemetry_engine）

将批量回算（按小时分区的 Parquet）与流式增量（逐批到达的 NDJSON）统一到
同一套按车辆键重分布、窗口对齐去重、双租户配额隔离的分析引擎中。
纯 Python 3.11+ 实现，仅依赖 `pyarrow` 与 `numpy`，状态使用标准库 SQLite，
无需额外数据库或消息服务；在 Windows 10/11 / Server 2022 与 Linux 上均可运行。

## 安装

```bash
python -m venv .venv
.venv/Scripts/python -m pip install -r requirements.txt   # Windows
# .venv/bin/python -m pip install -r requirements.txt     # Linux
```

## 快速开始

```bash
# 1. 确定性生成本地工作负载（批量 5 万条 Parquet + 48 批 JSON，含倾斜车辆）
python run.py generate --config config.json

# 2. 双租户并发执行统一分析（2 个工作进程 = 2 逻辑 CPU）
python run.py run --config config.json

# 3. 故障后从已提交状态增量恢复（跳过已提交任务）
python run.py run --config config.json --resume

# 4. 一键验收（去重对齐 / 倾斜拆分 / 配额隔离 / 故障恢复 / 可复现性）
python acceptance.py --regen
```

单租户端到端分析在 2 核预算下实测约 3 秒（限额 5 分钟）。

## 数据布局与 Schema 清单

```
data/
  manifest.json                  # Schema、生成器参数、随机种子、分区/批次声明
  batch/hour=00..23/part-000.parquet
  stream/batch_000..047.json     # NDJSON，每批约 1 万条
```

记录字段（清单中声明，引擎启动时校验，不符返回 `E_SCHEMA_VIOLATION`）：
`vehicle_id`(string)、`event_time`(int64, epoch ms)、`speed_x10`、`fuel_ml`、
`distance_m`(int32)。度量字段全部为整数，聚合用整数求和，保证批量/流式
两侧逐字段精确一致（平均速度 = 总和/计数，输出时计算）。

## 关键语义

- **统一去重键**：`(vehicle_id, event_time)`；重复记录按内容哈希取最小者，
  与到达顺序无关，批量/流式两侧判定口径完全一致。
- **窗口与迟到**：固定 1 小时事件时间窗口；水位线 = 已见最大事件时间 -
  `allowed_lateness_seconds`。窗口关闭后到达的记录计入 `late_records`，
  不修改已输出结果；生成器保证每个键的首次到达不迟到，因此批量回算与
  流式增量在相同完整数据上的最终聚合逐字段一致，否则作业以
  `E_CONSISTENCY_MISMATCH` 失败并在报告中列出差异窗口。
- **倾斜拆分**：车辆记录数超过 `skew_threshold_rows` 时按加盐子键
  `sha1(vehicle_id|event_time) mod n_chunks` 拆到多个处理单元，部分聚合
  （计数+整数和）可交换合并，拆分后结果与不拆分逐字段一致（验收 s1b 验证）。
- **租户配额**：每租户独立 `memory_quota_bytes` 与 `max_units`（并发处理
  单元上限），调度器在分发时仲裁；单任务估值超配额即中止该租户并返回
  `E_MEMORY_QUOTA_EXCEEDED`，其他租户不受影响；租户间状态（SQLite）与
  中间结果完全隔离。
- **故障恢复**：工作进程异常退出即检测并重调度其在途任务；已提交任务
  （SQLite `COMMITTED`）永不重算；重试超过 `max_retries` 该租户以
  `E_TASK_RETRY_EXHAUSTED` 失败并输出已完成车辆清单，不产出部分结果。

## 输出与查询

- `out/<tenant>/results.json`：每车辆-窗口的 `record_count`、`avg_speed`、
  `total_fuel_ml`、`total_distance_m`（仅在一致性核对通过时写出）。
- `out/report.json`：覆盖报告，字段稳定排序，重复执行除 `duration_sec`
  外完全一致。包含输入记录数、去重丢弃数、迟到记录数、倾斜拆分次数、
  各处理单元峰值内存、故障重试次数、一致性核对结论、最重处理单元行占比。
- 中间状态可查询：`sqlite3 state/<tenant>/state.db`，表 `tasks`（任务提交
  状态）与 `partials`（已提交部分聚合）。

## 配置说明（config.json）

- `engine.worker_processes`：物理工作进程数（本任务限额 2 逻辑 CPU → 2）。
- `engine.units_per_tenant`：每租户逻辑处理单元数（生产多节点拓扑的逻辑副本）。
- `engine.skew_threshold_rows` / `max_retries` / `window_seconds` /
  `allowed_lateness_seconds`：倾斜阈值、重试上限、窗口语义。
- `tenants[]`：`memory_quota_bytes`、`max_units` 按租户独立声明。
- `fault_injection`：验收用故障注入（`crash` 指定任务崩溃 N 次、
  `always_fail` 指定任务持续失败以触发重试耗尽）。

## 验收场景（acceptance.py）

| 场景 | 验证点 |
| --- | --- |
| s1_normal | 双租户成功、去重窗口对齐、迟到计数、倾斜拆分、单元行占比 ≤25%、峰值内存 ≤ 配额、端到端 < 5 分钟 |
| s1b | 倾斜拆分后聚合与单线程不拆分参考逐字段一致 |
| s2_repro | 报告除耗时外完全可复现 |
| s3_crash | 工作进程崩溃后增量恢复，结果与无故障运行一致 |
| s4_exhaust | 重试耗尽 → `E_TASK_RETRY_EXHAUSTED` + 已完成车辆清单，另一租户不受影响 |
| s5_resume | `--resume` 跳过已提交任务完成失败作业 |
| s6_quota | 内存配额超限 → 仅该租户 `E_MEMORY_QUOTA_EXCEEDED` |

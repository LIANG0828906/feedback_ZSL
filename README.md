# risklake：风控湖仓窗口 Join 执行器（单机轻量拓扑）

针对"夜间作业扩缩容后分钟级风控指标对不上"的事故，本仓库提供一个可在
单台 Windows 主机（本仓库在 Linux 容器内开发验证，代码仅依赖跨平台库）
运行的轻量执行器：从本地数据目录读取按小时分区的 Parquet 历史分片与
JSON Lines 增量批次，按事件时间分钟窗口对订单/退款事件做 Join 与聚合，
状态与提交点保存在标准库 `sqlite3` 数据库中，支持任意批次后被外部终止
并从已提交状态恢复，最终结果与不间断运行逐字段一致。

## 环境

- Python 3.11+，`venv` 安装 `duckdb`、`pyarrow`、`pytest`
- 无需数据库/消息服务；状态库为标准库 `sqlite3`
- 资源预算：2 逻辑 CPU / 1.25GiB 内存 / 2GB 临时空间（单进程实现，远低于预算）

```bash
python -m venv .venv
.venv/bin/pip install duckdb pyarrow pytest   # 或 uv pip install --python .venv/bin/python ...
```

## 快速开始

```bash
# 1. 生成默认配置（种子、重复比例、乱序时长均写入配置文件）
.venv/bin/python -m risklake.cli make-config --out config/job_config.json

# 2. 确定性生成样本：12 个小时 Parquet 分片（约 10 万行）+ 128 批 JSONL（约 4 万行）
.venv/bin/python -m risklake.cli generate --config config/job_config.json

# 3. 运行窗口 Join 作业（每提交一批打印 BATCH_COMMITTED <n>）
.venv/bin/python -m risklake.cli run --config config/job_config.json

# 4. DuckDB 批处理基准（与流式路径在相同完整数据上逐字段一致）
.venv/bin/python -m risklake.cli baseline --config config/job_config.json --out artifacts/baseline.json
```

报告写入 `artifacts/report.json`（键稳定排序，重复执行同一输入除
`generated_at`/`duration_seconds` 外字段值一致），包含：输入分片与批次数、
去重丢弃数、迟到事件数、每窗口金额与笔数、倾斜键拆分记录、恢复提交点与
次数、溢写字节数与峰值内存。

## 关键语义

- **去重**：批/流两侧统一按 `event_id` 首次出现保留（同一业务键同一事件
  时间窗口的重复上报只计一次），跨批次生效。
- **窗口 Join**：1 分钟滚动窗口，按 `(window_start, order_id)` 聚合订单/
  退款金额与笔数；金额以整数分存储，聚合完全确定。
- **处理槽**：按 `order_id` 哈希重分布，Join 两侧同键同槽；任一槽累计
  负载超过总事件数 40% 时，对该槽最热键按 `event_id` 加盐拆分（两侧同一
  盐函数，拆分合并后结果与不拆分逐字段一致）。
- **水印/迟到**：水印 = 已见最大事件时间 - 容忍时长，单调不回退（批次
  缺失也不回退）；窗口关闭后到达的事件计入迟到计数，不修改已输出结果。
- **状态**：SQLite 保存窗口累计状态、每窗口提交标志与水印；每批次一个
  事务，窗口状态更新整体可见；已提交窗口不重算、不二次输出。
- **稳定业务错误码**：状态库被独占或损坏 → `E_STATE`（退出码 70，损坏
  文件隔离为 `*.corrupt-<ts>`，后续运行不会读到污染状态）；超出内存上限
  → `E_MEM`（退出码 71，不静默丢窗口）；输入与状态指纹不一致 →
  `E_INPUT`（退出码 72）。

## 故障注入（外部进程操作，不修改引擎源码）

```bash
# 终止恢复：另起进程运行作业，看到 BATCH_COMMITTED <k> 后 kill -9，
# 再次执行同一 run 命令即从提交点 k 继续
# 状态库独占：
.venv/bin/python -m risklake.cli lock-db --db state/job_state.db --seconds 30
# 状态库损坏：
.venv/bin/python -m risklake.cli corrupt-db --db state/job_state.db
```

## 测试

```bash
.venv/bin/python -m pytest tests/ -q
```

覆盖：批/流路径一致、去重计数、4/8 槽结果一致、倾斜拆分与 40% 上限、
拆分/不拆分一致、容忍 0 与大值的迟到语义、缺失批次、kill 恢复一致、
E_STATE（锁定/损坏/隔离）、E_MEM、溢写清理、报告确定性、全量规模
（10 万行 + 128 批，实测约 2 秒、峰值内存约 20MB，远低于 6 分钟 / 1GiB 预算）。

# 统一事件时间窗口聚合引擎

单台 Windows 主机上对支付事件做离线复盘的统一批/流窗口聚合引擎。批处理与
增量流处理在**同一个作业、同一套语义函数**内完成，消除两套脚本口径漂移。

## 1. 目录结构

```
window_engine/
  config.py      作业配置与故障注入声明（JSON，不改引擎源码）
  datagen.py     固定种子的确定性数据生成（Parquet 历史 + JSONL 增量）
  semantics.py   窗口分配、单调水印、稳定分区与倾斜键拆分（批流共用）
  state.py       独占状态后端：文件锁、校验和、原子快照、溢写
  engine.py      批流混合引擎：去重、水印推进、迟到、并行聚合、提交
  batch.py       纯批路径：相同语义 + DuckDB SQL 独立交叉核对
  io_sources.py  Parquet / JSON Lines 读取与 Schema 校验
  errors.py      稳定业务错误码 E_STATE / E_INPUT / E_LIMIT / E_CONFIG
  report.py      字段稳定排序的确定性报告
  cli.py         命令行入口
verify.py        一键七项核对（批流一致、恢复、故障码、分区、资源）
tests/           unittest 端到端用例
```

## 2. 统一语义（批与流完全相同）

- **窗口**：事件时间滚动分钟窗口，`window_start = event_time // 60000 * 60000`。
- **去重口径**：去重键 `(payment_id, window_start, event_id)`，首次出现生效，
  后续同键事件计入 `dedup_dropped`。批路径与流路径调用同一规则。
- **水印**：`watermark = max(已声明事件时间) - allowed_lateness_ms`，单调不减，
  每个源单元（历史分片或流批次）消费完成后推进一次。
- **窗口关闭**：`watermark >= window_start + window_size`。
- **迟到事件**：窗口关闭后到达的同键同窗口事件只计入迟到计数，**永不修改已
  输出的窗口结果**；迟到事件输出为独立的零金额行。
- **输入结束**：水印推进到无穷大，关闭所有剩余窗口。

容忍度为 0 时窗口立即关闭、迟到最多；容忍度增大时迟到减少、输出窗口数减少，
该关系由 `tests/test_engine.py` 与 `verify.py` 断言。

## 3. 状态后端与故障恢复

- 作业目录下 `LOCK` 为跨平台独占文件锁（Windows `msvcrt` / POSIX `fcntl`）。
- 每个分片快照带 SHA-256 校验和，`meta.json` 最后原子替换，崩溃只留下一个
  一致检查点。
- 状态超 `state_memory_bytes` 时，最大分片溢写到磁盘（与状态目录同盘），
  作业结束清理溢写并保留最终检查点。
- 每消费一个源单元提交一次：已消费批次编号、水印、分片累计状态与已输出窗口
  全部持久化。外部终止后重启跳过已消费批次，已提交窗口不重算、不二次输出。
- 状态文件被占用或校验失败：以 `E_STATE`（退出码 71）终止；损坏检查点在终止
  前被删除，后续作业不会误认为已提交。

## 4. 并行与倾斜键

- 支付键经 FNV-1a 稳定哈希分配到 `partitions` 个处理单元，`ThreadPoolExecutor`
  按 `workers` 并发，单元内加锁保证同窗口不会部分写入。
- 预扫描统计每个支付单事件量；任一单元超过 `skew_ratio`（默认 40%）时，对最大
  热点键拆成虚拟子单元并映射回物理单元，输出时按窗口合并。
- 任意分区数下窗口结果逐字段一致，输出按 `window_start` 确定排序；拆分记录写入
  报告 `split_plan.split_records`。

## 5. 故障注入（声明式，不改引擎源码）

故障配置 JSON 中 `rules` 列表支持：

| fault | 行为 | 结果 |
| --- | --- | --- |
| `kill_after_commit` | 指定批次提交后模拟外部终止 | 重启从该提交点继续 |
| `state_locked` | 模拟状态后端被其他进程独占 | `E_STATE` (71) |
| `state_corrupt` | 破坏分片快照校验和 | `E_STATE` (71)，检查点丢弃 |
| `early_watermark` | 当批事件按窗口已关闭处理 | 迟到计数上升，报告记录 |
| `batch_read_failure` | 指定批次读取失败 | `E_INPUT` (72) |

真实跨进程占用可用 `python -m window_engine.lock_holder <LOCK路径> <秒数>`。

## 6. 作业报告

报告 JSON 键稳定排序，除 `elapsed_ms` 与 `generated_at_ms` 外，同一输入重复
执行字段值完全一致。包含：输入分片/批次数与行数、去重丢弃数、迟到事件数、
每窗口事件数与金额、倾斜键拆分记录、恢复次数与提交点、已消费批次、溢写字节、
峰值 RSS、故障类型与受影响批次。

## 7. Windows 离线安装

需要 Python 3.11。联网机器上准备 Windows 轮子：

```
python scripts/fetch_wheels.py --out wheels --platform win_amd64 --python-tag cp311
```

拷贝到离线 Windows 主机后：

```
py -3.11 -m venv .venv
.venv\Scripts\python -m pip install --no-index --find-links wheels -r requirements.txt
```

## 8. 运行

```
# 生成确定性样本（默认 12 万历史 + 128 批增量）
.venv/Scripts/python -m window_engine.cli generate --data-dir data/full

# 纯批路径 / 批流混合路径
.venv/Scripts/python -m window_engine.cli run --config configs/full.json

# 一键核对（小样本；--full 跑全量）
.venv/Scripts/python verify.py
```

## 9. 错误码

| 码 | 退出码 | 含义 |
| --- | --- | --- |
| `E_STATE` | 71 | 状态后端被占用或损坏 |
| `E_INPUT` | 72 | 输入分片/批次不可读或 Schema 不符 |
| `E_LIMIT` | 73 | 内存或临时空间硬上限将被突破 |
| `E_CONFIG` | 74 | 作业/故障配置非法 |

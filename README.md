# MixRec 本地赛事混流切片工作台

MixRec 用统一时基把多路本地 16-bit PCM WAV 混流成一条输出轨道，并按固定样本数切片。项目面向 Windows 10/11 本地运行，Python 3.11 或 3.12；音频计算只依赖 NumPy，可视化界面使用可选依赖 PySide6。

核心特性：

- 16 kHz / 48 kHz、单声道 / 双声道输入，确定性 Catmull-Rom 三次多项式重采样。
- 每轨增益、静音区间、延迟补偿、逐样本混合与限幅。
- 固定样本数切片、连续边界校验、WAV 内容哈希与文件哈希。
- 受限并行度、单切片内存预算、全局内存预算和确定性提交顺序。
- 增益 / 静音 / 延迟修改的增量重算与一次原子索引发布。
- 每片“临时文件 + 原子重命名”，索引“临时文件 + 原子替换”。
- 强制终止后的索引、哈希链、版本封印和从提交前缀恢复。
- PySide6 工作台：波形包络、进度、切片网格、内存曲线、等待队列、暂停 / 取消 / 恢复。
- 固定种子与操作序列下，WAV、切片、索引和轨迹逐字节可复现。

## 环境准备

Windows 命令提示符或 PowerShell：

```bat
python --version
python -m pip install -r requirements.txt
```

只运行引擎和测试时可只安装：

```bat
python -m pip install numpy pytest
```

所有示例均使用本地相对路径，不需要网络素材或外部服务。

## 快速开始

生成三组确定性样本、配置和基线：

```bat
python -m mixrec generate --out local_samples --seed 20261121
```

等价的独立脚本入口：

```bat
python scripts\generate_samples.py --out local_samples --seed 20261121
```

生成内容：

```text
local_samples/
  manifest.json
  jobs/mixed_rates_mono/config.json
  jobs/mixed_rates_mono/tracks/*.wav
  jobs/mixed_rates_stereo/...
  jobs/short_timeline/...
  baseline/*.json
```

渲染并校验：

```bat
python -m mixrec render local_samples\jobs\mixed_rates_mono\config.json
python -m mixrec verify local_samples\jobs\mixed_rates_mono\config.json
```

只提交前 3 片，然后从快照恢复：

```bat
python -m mixrec render local_samples\jobs\mixed_rates_mono\config.json ^
  --output-dir local_run --trace local_run\trace.jsonl --no-resume --max-slices 3
python -m mixrec resume local_samples\jobs\mixed_rates_mono\config.json ^
  --output-dir local_run --trace local_run\trace.jsonl
```

增量修改轨道：

```bat
python -m mixrec update local_samples\jobs\mixed_rates_mono\config.json ^
  --track ambience --gain-db -1.5 --mute 0.4:0.44
```

打开工作台：

```bat
python -m mixrec gui --config local_samples\jobs\mixed_rates_mono\config.json
```

运行测试和完整验收：

```bat
pytest -q
python scripts\acceptance.py --out artifacts\validation --seed 20261121 --force
```

## 配置格式

```json
{
  "schema_version": 1,
  "job_id": "mixed_rates_mono",
  "output": {
    "sample_rate": 48000,
    "channels": 1,
    "bit_depth": 16,
    "slice_samples": 4096,
    "total_samples": 60960
  },
  "tracks": [
    {
      "id": "commentary",
      "path": "tracks/commentary.wav",
      "sample_rate": 16000,
      "channels": 1,
      "gain_db": 1.5,
      "mute_ranges": [],
      "delay_samples": 0,
      "version": 1,
      "max_delay_samples": 48000
    }
  ],
  "scheduler": {
    "max_workers": 2,
    "per_slice_budget_bytes": 16777216,
    "memory_budget_bytes": 33554432,
    "block_samples": 4096,
    "task_priorities": {"5": 10}
  },
  "output_dir": "output",
  "trace_path": "trace.jsonl",
  "max_delay_samples": 48000
}
```

约定：

- `path`、`output_dir`、`trace_path` 默认相对配置文件所在目录；命令行显式给出的 `--output-dir` / `--trace` 相对当前工作目录。
- `mute_ranges` 使用秒，`[start, end)`；实现时转换为输出样本，并加上该轨延迟补偿。
- `delay_samples` 以输出采样率为单位；负值或超过轨道 / 全局 `max_delay_samples` 返回 `E_MIX_INPUT`。
- 单声道可上混到双声道，双声道可平均下混到单声道；超过双声道或声明布局与实际 WAV 不一致返回 `E_MIX_INPUT`。
- `task_priorities` 只写入轨迹，不能改变切片提交顺序。

## 输出与轨迹

默认输出目录：

```text
output/
  index.json
  config_snapshot.json
  slices/v000001/slice_000000.wav
  slices/v000001/slice_000001.wav
  ...
```

`index.json` 记录：

- 切片序号、起止样本、样本数；
- `content_sha256`（原始 PCM 数据）与 `file_sha256`（完整 WAV）；
- 每片参数版本、产物版本、前链哈希与当前链哈希；
- 轨道参数、来源 WAV 哈希、索引版本、参数指纹、索引封印和完成状态。

JSONL 轨迹不含时间戳、线程号或绝对路径，字段按字典序输出。主要事件包括：

- `job_start` / `run_complete` / `run_failed` / `run_stopped`
- `memory_reserved` / `memory_released` / `task_waiting`
- `budget_degraded` / `budget_rejected`
- `task_started` / `task_completed` / `task_failed`
- `slice_committed`
- `incremental_start` / `incremental_published`
- `recovery_resume`

## 失败语义

| 错误码 | 含义 |
| --- | --- |
| `E_MIX_INPUT` | WAV、采样率、声道布局或延迟范围不合法，带 `track_id` |
| `E_SLICE_GAP` | 切片边界不连续 / 渲染长度不符，带 `slice_index` |
| `E_BUSY` | 同一输出目录的混流任务已在运行 |
| `E_BUDGET_EXCEEDED` | 降级内部块后仍超过单切片或全局内存预算 |
| `E_RECOVERY_INVALID` | 索引封印、版本、参数、哈希链或切片内容校验失败 |
| `E_CONFIG` | 配置字段或路径不合法 |
| `E_IO` | 文件读写、临时文件或原子替换失败 |

预算拒绝、渲染失败或哈希校验失败后，只保留失败位置之前的连续索引前缀；该切片及后续不会成为索引引用的正式产物。恢复会重新校验整个前缀，失败时不会覆盖已有切片。

## 实际验证结果

本机运行：

```text
python -m pytest -q
21 passed

python scripts/acceptance.py --out artifacts/validation --seed 20261121 --force
```

验收摘要（`artifacts/validation/summary.json`）：

- 初始混流：`mixed_rates_mono`，15 片，60960 样本，`index_version=15`。
- 确定性回放：内容哈希、WAV 文件哈希、轨迹 SHA-256 三项全部一致。
- 预算拒绝：`short_timeline` 第 0 片返回 `E_BUDGET_EXCEEDED`；没有正式切片目录，也没有索引。
- 增量更新：只重算第 4/5/6 片，复用 12 片；发布 `index_version=16`，未受影响内容哈希不变。
- 受控调度：单切片预算触发内部块降级，全局预算使后续任务进入等待队列，轨迹记录降级与等待事件。
- 恢复：从第 3 片之后继续，最终 15 片、哈希链和内容哈希与初始完整运行一致。
- 恢复防覆盖：篡改切片后返回 `E_RECOVERY_INVALID`，索引字节保持不变。

证据目录：

- `artifacts/validation/traces/`: 五组确定性轨迹。
- `artifacts/validation/indexes/`: 初始与增量发布的完整索引。
- `artifacts/validation/baseline/`: 三组样本的切片与轨迹基线哈希。
- `artifacts/validation/runs/`: 实际渲染、回放、预算拒绝、恢复和损坏检测输出。

更详细的算法、事务与恢复设计见 `docs/DESIGN.md`。

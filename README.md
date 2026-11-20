# Broadcast Loudness Workbench

本地、确定性的多档广播素材响度与真峰值工作台。核心链路为：

```text
WAV / 声道掩码校验
  -> BS.1770 加权声道下混
  -> K 加权滤波（跨块连续状态）
  -> 固定观测块切片
  -> 短期响度 / 积分响度 / 4x 过采样真峰值
  -> 不可变计值报告 + 哈希链
  -> 原子 JSON 快照
```

同时提供有界并行调度、内存预算降级、在线/离线逐块一致性校验、参数增量重算和播放跳转控制。全部样本、基线、轨迹与报告均由本地确定性代码生成，不访问网络或外部服务。

## 环境与安装

要求 Python 3.10+ 和 NumPy；Tkinter 仅用于可选的桌面 UI。

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
source .venv/bin/activate
```

当前验收环境为 Python 3.14.4、NumPy 2.5.5。该 Linux 镜像没有安装 `tkinter`，因此核心、CLI、测试和验收均以无 GUI 方式完成；`loudness_workbench/gui.py` 在具备 Tk 的标准桌面 Python 上可直接使用。

## 快速运行

重新生成确定性多档样本、真值和完整配置：

```bash
python scripts/make_samples.py
```

运行三档素材的并行计量、在线/离线比对、快照恢复校验：

```bash
python -m loudness_workbench run --config samples/config.json
```

只输出验收摘要可查看 `artifacts/run/result.json`；详细逐块值在：

- `artifacts/run/report_program_*.json`
- `artifacts/run/offline_program_*.json`
- `artifacts/run/snapshots/*.json`
- `artifacts/run/trace.jsonl`

仅校验一个磁盘快照：

```bash
python -m loudness_workbench verify \
  --snapshot artifacts/run/snapshots/program_a_5_1.json \
  --parameter-version 3
```

运行测试：

```bash
python -m unittest discover -v
```

打开可选 Tk 工作台：

```bash
python -m loudness_workbench gui --config samples/config.json
```

## Python API

```python
from dataclasses import replace

from loudness_workbench.config import load_workbench_config
from loudness_workbench.metering import AssetMeter, analyze_offline, compare_reports
from loudness_workbench.scheduler import MeterScheduler
from loudness_workbench.snapshot import SnapshotStore

workbench = load_workbench_config("samples/config.json")
meter = AssetMeter(
    "samples/program_b_stereo_16bit.wav",
    config=workbench.meter,
    asset_id="program_b_stereo",
)

# 在线推进一个观测块，报告只在整块计算完成后整体替换。
block = meter.process_next()

# 参数变化：目标响度只改变派生字段，原始短期/真峰值块缓存复用。
update = meter.update_params(replace(meter.config, target_lufs=-20.0))
assert update.affected_raw_blocks == ()
assert len(update.reused_raw_blocks) == meter.published.block_count

# 块长变化：重新切片并返回新旧边界差异摘要。
resliced = meter.update_params(replace(meter.config, block_samples=2400))
print(resliced.reslice.to_dict())

# 离线整段分析必须和在线结果按 sync_marker 逐值对齐。
offline = analyze_offline(
    "samples/program_b_stereo_16bit.wav",
    config=meter.config,
    asset_id="program_b_stereo",
)
print(compare_reports(meter.published, offline, meter.config))

SnapshotStore("artifacts/manual/program_b_stereo.json").save(meter.published)
```

并行调度：

```python
scheduler = MeterScheduler(
    workbench.meter,
    trace_path="artifacts/manual/trace.jsonl",
)
scheduler.start_asset("program_b_stereo", meter_b)
scheduler.start_asset("program_c_mono", meter_c)
result = scheduler.execute()
print(result.to_dict())
```

重复启动仍处于活动状态的档位会立即返回 `E_BUSY`。同一档位只提交一个可运行队首任务，跨档位任务按全局提交序号稳定排序。

## 配置契约

配置文件必须包含全部字段；缺字段、字段类型非法、并行度或预算非法时在启动前返回 `E_CONFIG_INVALID`。样例见 `samples/config.json`。

| 字段 | 含义 |
| --- | --- |
| `sample_rate` | 固定 `48000`；其它值返回 `E_CHANNEL_UNSUPPORTED` |
| `block_samples` | 固定观测块样本数，默认 `4800`（100 ms） |
| `short_term_samples` | 短期响度窗口，默认 `144000`（3 s） |
| `oversample_factor` | 真峰值过采样倍数，默认 `4` |
| `target_lufs` | 目标响度，仅影响派生增益/对齐信息 |
| `true_peak_limit_dbtp` | 真峰值上限，仅影响合规派生字段 |
| `absolute_gate_lufs` | 积分响度绝对门限，默认 `-70 LUFS` |
| `relative_gate_lu` | 相对门限，默认 `-10 LU` |
| `channel_weights` | 可选自定义声道权重；`null` 时按布局取 BS.1770 默认值 |
| `scheduler.parallelism` | 全局并行度 |
| `scheduler.memory_budget_bytes` | 全局内存预算 |
| `scheduler.per_task_memory_budget_bytes` | 单块任务预算，不得超过全局预算 |
| `scheduler.oversample_batch_samples` | 过采样工作批大小 |
| `scheduler.internal_buffer_samples` | 内部历史/缓冲大小 |
| `scheduler.degrade_sequence` | 降级顺序，支持 `increase_oversample_batch`、`shrink_internal_buffer` |
| `lufs_tolerance` / `true_peak_tolerance` | 在线离线一致性容差 |

## 计量口径

### 声道布局与下混

支持 16-bit、24-bit PCM 和以下声道掩码：

| 布局 | 掩码 | 默认权重 |
| --- | ---: | --- |
| mono | `0x0004` | `1.0` |
| stereo | `0x0003` | `1.0, 1.0` |
| 3.0 | `0x0007` | `1.0, 1.0, 1.0` |
| 2.1 | `0x000B` | `1.0, 1.0, 0.0` |
| quad | `0x0033` | `1.0, 1.0, 1.41, 1.41` |
| 5.0 | `0x0037` | `1.0, 1.0, 1.0, 1.41, 1.41` |
| 5.0-side | `0x0607` | `1.0, 1.0, 1.0, 1.41, 1.41` |
| 5.1 | `0x003F` | `1.0, 1.0, 1.0, 0.0, 1.41, 1.41` |

下混保持 BS.1770 的功率加权语义：对每个声道乘以 `sqrt(weight)` 后保留为加权声道，计算时对 K 加权功率求和。没有把多声道直接相加成一路，避免相位抵消改变计量口径。LFE 在响度权重中为 0；真峰值默认仍包含 LFE 原始声道，可通过 `true_peak_include_lfe` 控制。

声道数、掩码、采样率、位深或 WAV 结构不受支持时返回 `E_CHANNEL_UNSUPPORTED` / `E_AUDIO_FORMAT`。已发布报告不会被修改。

### 观测块与计值

- 每块记录 `window_start`、`window_end`、`sync_marker = "<asset_id>:<6位块序号>"`、原始 PCM 内容哈希和完整计值。
- 短期响度使用以当前块末尾为止、最长 3 s 的 K 加权窗口；开头不足 3 s 时使用已有样本。
- 积分响度对当前已处理块按绝对/相对门限计算，在线前缀与离线整段分析采用同一函数、同一门限和同一块能量。
- 真峰值使用确定性 4 倍过采样低通插值；块内计算，不引用未来样本。
- 所有报告是 frozen dataclass，并以 tuple 保存块序列。`get_published()` 返回完整旧版或完整新版引用。

### 在线/离线校验

`compare_reports()` 以 `sync_marker` 对齐块，依次比较：

- `short_term_lufs`
- `integrated_lufs`
- `true_peak_dbtp`
- 块能量
- 原始 PCM `content_hash`

首个超差返回 `E_LUFS_MISMATCH`，上下文含 `asset_id`、`sync_marker`、`block_index`、`field`、双方值和容差，不使用时间戳代替计量值。

## 调度与预算

- 每个档位维护独立 FIFO 队列；同一轮每档位最多提交一个队首块，因此同档位严格有序。
- 可运行队首按全局 `submit_seq` 稳定排序，跨档位并发执行。
- 每轮并发任务完成后，按 `submit_seq`（而非真实完成时间）排序写轨迹，保证相同种子/配置的轨迹字节一致。
- 单块估算包含输入/滤波后块、保留的历史窗口、内部缓冲、过采样工作区和滤波器状态。
- 超过单任务预算时先记录 `budget_wait`，再按配置序列逐级降级并记录每一级的 `before_bytes`、`after_bytes`、`fits`。
- 降级仅改变过采样分批和内部缓冲策略；数值计算函数不变。预算受限块与不受限参考块计值逐字段相等。
- 全部降级后仍超限时拒绝该块，返回 `E_BUDGET_EXCEEDED`，上下文定位 `asset_id`、`block_index`、估算字节、预算字节和尝试过的阶段。

轨迹事件包含 `trace_meta`、`asset_start`、`budget_wait`、`degrade`、`block_start`、`block_complete`、`asset_complete`、`trace_end` 及错误事件。事件自身使用 `prev_hash -> event_hash` 链。

## 增量重算

`AssetMeter.update_params()` 区分两层缓存：

- 原始块缓存：短期响度、块能量、真峰值、内容哈希。
- 派生报告：目标增益、真峰值合规、积分门限与版本字段。

目标响度、真峰值上限、门限变化时复用全部原始块；声道权重、块长、短期窗口、过采样倍数等计量口径变化时重算受影响块。块长变化同时返回 `ResliceDiff`，含旧/新边界、增加/删除边界、变化块集合和最大边界偏移。

重算在锁内完成，最终只执行一次报告引用替换。读者在重算期间始终能读到完整旧版；完成后只能读到完整新版。失败时恢复原配置、缓存和最后一次已发布报告。

## 播放控制

`PlaybackController` 支持 `play()`、`pause()`、`seek_to_block()`、`seek_to_sample()`、`restart()` 和按块 `advance()`。

- 暂停和跳转都会释放块样本句柄。
- 跳转通过从头重放因果前缀重建 K 加权状态，避免直接从目标窗口起算造成滤波器状态不连续。
- 取消或异常保留最后一次成功发布的报告，控制器进入 `failed` 状态。
- 强制终止后的恢复入口为 `recover_from_snapshot()`，只读磁盘 JSON 快照，不使用进程内缓存。

## 快照与哈希链

每个块先对规范化 JSON 计 `block_hash`，再以 `sha256(prev_chain_hash || block_hash)` 计算 `chain_hash`；报告头保存最终 `head_hash` 和 `report_hash`。快照包含：

- schema 版本、参数版本、配置哈希；
- 声道布局、掩码、位深、块长；
- 每块窗口、同步标记、内容哈希、全部计值、降级阶段和内存用量；
- 块哈希、链哈希、报告哈希。

保存流程为同目录临时文件、`fsync`、`os.replace`，再做目录 `fsync`。载入时验证 schema/参数版本、每块哈希、链哈希、头哈希和报告哈希；不一致返回 `E_REPORT_MISMATCH`，不返回部分报告。

运行期间每完成一个已发布块都会原子更新对应快照，因此进程被强杀后仍可从最近一次完整发布点恢复。

## 错误码

| 错误码 | 场景 |
| --- | --- |
| `E_CHANNEL_UNSUPPORTED` | 声道布局、声道数、掩码、采样率或位深不受支持 |
| `E_AUDIO_FORMAT` | RIFF/PCM 结构、块对齐或文件读取错误 |
| `E_CONFIG_INVALID` | 配置字段缺失/非法、降级阶段未知 |
| `E_BUSY` | 重复启动活动档位或重复执行同一调度器 |
| `E_BUDGET_EXCEEDED` | 全部降级后仍超过单块内存预算 |
| `E_LUFS_MISMATCH` | 在线/离线首个同步块字段超差 |
| `E_REPORT_MISMATCH` | 快照版本、块哈希、链哈希或报告哈希不一致 |
| `E_BLOCK_NOT_READY` | 请求块越界、未处理或播放位置非法 |
| `E_TASK_FAILED` | 未归类的任务/桌面环境错误 |

## 确定性样本

`loudness_workbench/generator.py` 生成：

| 档位 | 格式 | 时长 | 布局 |
| --- | --- | ---: | --- |
| `program_a_5_1` | 24-bit PCM | 4.2 s | 5.1 |
| `program_b_stereo` | 16-bit PCM | 3.2 s | stereo |
| `program_c_mono` | 16-bit PCM | 2.6 s | mono |

波形由固定频率正弦、固定分段幅度和由 `(seed, asset_id, channel)` SHA-256 派生的相位组成。`samples/truth.json` 用解析双二阶频响独立给出声明响度和声明真峰值。生成过程不使用时间、系统随机源或全局随机状态；同种子重复生成的 WAV 和 truth JSON 逐字节一致（测试覆盖）。

## 实际验证结果

环境：Python 3.14.4、NumPy 2.5.5、Linux x86_64，无网络访问。

自动化测试：

```text
Ran 14 tests
OK
```

覆盖：16/24-bit 读取、5.1 掩码与权重、非法掩码/采样率、在线离线逐块一致、首个 `E_LUFS_MISMATCH` 定位、增量缓存与重切片、稳定并行顺序、`E_BUSY`、预算降级透明性、`E_BUDGET_EXCEEDED` 定位、快照往返/篡改、磁盘恢复、播放跳转/重启、种子与轨迹字节确定性。

完整三档运行：

```text
parallelism = 3
completed_blocks = 100
degraded_blocks = 18       # program_a_5_1 的历史窗口增长后触发
waiting_events = 18
error_count = 0
online/offline compared = 42 + 32 + 26 = 100 blocks
first_mismatch = null
repeated_run_trace_byte_identical = true
```

计值与解析真值：

| 档位 | 块数 | 积分响度 LUFS | 最大真峰值 dBTP | 响度误差 LU | 真峰值误差 dB |
| --- | ---: | ---: | ---: | ---: | ---: |
| `program_a_5_1` | 42 | -19.518688 | -21.412617 | +0.000082 | -0.000995 |
| `program_b_stereo` | 32 | -20.403133 | -19.575305 | +0.000026 | +0.000909 |
| `program_c_mono` | 26 | -22.563980 | -18.805242 | +0.000222 | -0.019199 |

快照头：

```text
program_a_5_1  head=72753298fa7384f18988326bf76ba755784189aefaadfcb28a435598b8b087de
program_b_stereo head=82c2b4209ba6d13fc69346bfd6d261216d907399397167dab9c680827496877c
program_c_mono  head=94d4d38cc8994b800c89f65e8378dba8d23e62304a2e3bc40b49298e22e95d3f
```

轨迹日志：`artifacts/run/trace.jsonl`。完整汇总：`artifacts/run/result.json`。

## 目录

```text
loudness_workbench/
  wav.py          RIFF/WAVE、PCM 解码、声道路由、确定性写出
  metering.py     K 加权、块切片、短期/积分响度、真峰值、增量发布
  budget.py       内存估算与降级序列
  scheduler.py    稳定并行调度与轨迹
  models.py       不可变报告、块与哈希链
  snapshot.py     原子快照、校验、磁盘恢复
  playback.py     播放/暂停/跳转/重启
  generator.py    确定性样本与解析真值
  gui.py          可选 Tk 工作台
  workbench.py    端到端运行和在线/离线验收
samples/          多档 WAV、truth.json、config.json
scripts/          样本生成入口
tests/            14 项自动化测试
artifacts/run/    报告、轨迹、快照、验收摘要
```

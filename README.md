# Haptic Waveform Studio

一个本地运行的 Tkinter + Matplotlib 桌面应用，用于编辑分段触觉波形、增量重算左右马达驱动信号，并以不可变版本发布预览。项目同时提供确定性样本生成器、JSONL 轨迹日志、原子快照恢复和完整自动化测试。

实现仅使用本机 Python、NumPy、Matplotlib 和 Tkinter，不使用 Docker 或其他容器。

## 快速开始

```bash
# Python 3.10+
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# 生成 5 个本地确定性样本
python -m haptic.cli generate-samples --outdir samples

# 启动桌面应用
./run_gui.sh
```

也支持当前验证机使用的 `uv` 方式：

```bash
uv venv .venv --python 3.14
uv pip install --python .venv/bin/python -r requirements.txt
```

Tk 检查：

```bash
./run_gui.sh --check
./run_gui.sh --smoke-test
```

在普通 Ubuntu 桌面环境中，如果 Python 缺少 Tk，执行 `sudo apt install python3-tk`。当前验证机的系统 Python 没有 `tkinter`，且没有可用的 sudo；为了完成本机验证，使用了 `apt-get download` 将 Tk 8.6 相关 `.deb` 解压到项目内的 `.deps/`（不是容器），`run_gui.sh` 会自动设置 `PYTHONPATH`、`LD_LIBRARY_PATH`、`TCL_LIBRARY` 和 `TK_LIBRARY`。正式部署仍推荐直接安装系统 Tk。

## 桌面界面

界面包含：

- 左侧段列表：起始时刻、时长、频率、幅度、包络、左右分配比。
- 编辑区：开始/时长/频率/幅度/包络、左右分配比与极性。
- `Apply & Recompute`：提交后台增量重算。
- `Cancel`：在采样、叠加或哈希检查点协作取消；旧版本继续可读。
- 右侧上图：各段左马达贡献波形。
- 右侧下图：左右马达合成驱动信号，红色 `x` 为限幅样本。
- 左下：最近发布的确定性轨迹日志。
- `Save Snapshot` / `Recover Snapshot`：原子快照与校验恢复。

界面逻辑与无界面引擎共用同一套 `Publisher`、`SnapshotManager` 和 `RecomputeTaskManager`，不存在 GUI 专用旁路。

## 确定性样本与演示

```bash
python -m haptic.cli generate-samples --outdir samples
python -m haptic.cli demo --samples samples --out artifacts/demo
```

样本覆盖：

| 样本 | 段数 | 特性 |
|---|---:|---|
| `single_short.json` | 1 | 短矩形脉冲 |
| `three_overlap.json` | 3 | 三段重叠、硬限幅 |
| `mixed_envelopes.json` | 5 | 三种包络、左右不对称分配 |
| `dense_soft.json` | 8 | 高重叠、软限幅 |
| `cancel_long.json` | 3 | 8 kHz、长序列取消测试 |

演示执行两次真实编辑并输出：

- `artifacts/demo/trace.jsonl`：发布版本、修改段、依赖段、重算/复用区间、块统计、哈希。
- `artifacts/demo/signal-latest.bin`：左右驱动信号的 little-endian `float64` 原始字节。
- `artifacts/demo/snapshot*.json`：版本 1、最终版本、缺失块恢复检查点。
- `artifacts/demo/summary.json`：可复现摘要和验证断言结果。

相同样本和操作序列会得到逐字节相同的 `trace.jsonl` 与 `signal-latest.bin`。

## JSON 格式

最小示例：

```json
{
  "sample_rate": 2000,
  "device": {
    "min": -1.0,
    "max": 1.0,
    "limiter": "hard",
    "soft_knee_ratio": 0.8
  },
  "motor_map": {
    "left_ratio": 0.9,
    "right_ratio": 0.65,
    "left_polarity": 1,
    "right_polarity": 1
  },
  "chunk_size": 64,
  "segments": [
    {
      "start_ms": 0,
      "duration_ms": 220,
      "frequency_hz": 55,
      "amplitude": 0.65,
      "envelope": "sine"
    },
    {
      "start_ms": 60,
      "duration_ms": 260,
      "frequency_hz": 72,
      "amplitude": 0.8,
      "envelope": "rectangular",
      "left_ratio": 0.7,
      "right_ratio": 1.0,
      "right_polarity": -1
    }
  ]
}
```

字段约束：

- `sample_rate`：整数，范围 `1..192000`。
- `start_ms`：非负有限数。
- `duration_ms`：非负有限数。
- `frequency_hz`：大于 0 且不超过 Nyquist 频率 `sample_rate / 2`。
- `amplitude`：非负有限数。
- `envelope`：`rectangular`、`linear_decay`、`sine`；也接受 `rect`、`linear`、`sin` 等别名并归一化。
- `left_ratio` / `right_ratio`：非负有限数；段字段缺省时继承 `motor_map`。
- `left_polarity` / `right_polarity`：`1`/`-1`，也接受 `+`/`-`。
- `limiter`：`hard` 或 `soft`。
- `soft_knee_ratio`：`[0, 1)`。

字段缺失、负时长、频率越界、包络非法等情况抛出 `E_WAVE_INVALID`，异常对象包含 `segment_index`。

## 采样与包络公式

采样区间采用半向上取整：

```text
start_sample = floor(start_ms * sample_rate / 1000 + 0.5)
sample_count = floor(duration_ms * sample_rate / 1000 + 0.5)
interval = [start_sample, start_sample + sample_count)
```

段内本地时间 `t = k / sample_rate`，载波为 `sin(2π f t)`。包络按声明的秒级时长 `d` 计算，而不是按实际离散样本数计算：

```text
rectangular:  env(t) = 1
linear_decay: env(t) = 1 - t / d
sine:         env(t) = 0.5 - 0.5 * cos(2π t / d)
```

因此不同采样率下的时长边界保持一致。左右马达贡献：

```text
left(t)  = sin(2π f t) * env(t) * amplitude * left_ratio  * left_polarity
right(t) = sin(2π f t) * env(t) * amplitude * right_ratio * right_polarity
```

每段缓存贡献数组，并输出：

```json
{
  "index": 1,
  "sample_interval": [120, 640],
  "sample_hash": "a8c3..."
}
```

段哈希覆盖规范化段参数、采样区间以及左右贡献的 little-endian `float64` 字节。

## 叠加与限幅

所有段按 `(start_sample, segment_index)` 稳定排序后逐样本叠加，保证相同输入得到相同浮点累加顺序。

硬限幅：

```text
limited = clip(raw, device_min, device_max)
```

软限幅对膝点外的信号使用 `tanh` 压缩：

```text
high_knee = device_max * soft_knee_ratio
high_span = device_max - high_knee
limited = high_knee + high_span * tanh((raw - high_knee) / high_span)   # raw > high_knee
```

负半轴对称处理。输出两个统计量：

- `out_of_range_samples`：原始合成信号超出 `[device_min, device_max]` 的样本数。
- `limited_samples`：限幅函数实际修改的样本数。硬限幅时两者相同；软限幅时膝点压缩也算修改。

## 增量重算

每个段的影响因子为：

```text
start_ms, duration_ms, frequency_hz, amplitude, envelope,
left_ratio, right_ratio, left_polarity, right_polarity
```

增量判定：

1. 比较新旧段影响因子，自动找出修改段；调用方传入的 `modified_indices` 会与自动检测结果合并，不能用来错误跳过变化。
2. 受影响样本区间取该段新旧支持区间的并集，并向左右各扩展 1 个样本作为过渡点。
3. 只重新采样修改段，重新叠加受影响区间；其余样本沿用旧发布版本中的值。
4. 每个 `chunk_size` 样本块有独立哈希。只重建与受影响区间相交的块哈希，其余块哈希复用。
5. 如果采样率、分配配置、段数量、总样本数或段索引发生变化，无法局部证明安全时执行保守全量重算。

轨迹会输出：

- `affected_intervals`
- `reused_intervals`
- `affected_interval_count` / `reused_interval_count`
- `recomputed_samples` / `reused_samples`
- `recomputed_chunks` / `reused_chunks`
- `dependencies`：修改段及其采样区间、段哈希

公开版本内容哈希覆盖配置、全部段参数、逐段采样哈希、全部块哈希、限幅统计、版本号和父哈希。增量结果与从零全量重建的信号字节、段哈希和块哈希一致。

## 版本发布隔离

`VersionedSignal` 是不可变发布对象，内部 NumPy 数组设置为只读。重算线程在独立局部结构中完成全部采样、叠加、限幅和哈希后，才在 `Publisher` 锁内整体替换当前版本引用。

预览查询：

```python
version = publisher.snapshot()          # 完整旧版本或完整新版本
version = publisher.get_version(7, expected_hash)
```

若查询版本号不是当前版本，或内容哈希不匹配，抛出 `E_VERSION_STALE`。已经取得的不可变版本引用仍可安全读取。

## 原子快照与恢复

快照包含：

- 完整版本哈希链：从 genesis 哈希开始，每个版本的 `parent_hash` 必须等于前一版本 `content_hash`。
- 规范化项目配置和全部段参数。
- 每段采样区间与内容哈希、段贡献数组。
- 每个块的信号字节、信号哈希、限幅统计、限幅/越界掩码。
- 已发布版本号、父哈希、内容哈希和限幅总数。

写入流程：

```text
同目录临时文件 -> write -> flush -> fsync(file) -> os.replace -> fsync(directory)
```

恢复流程：

1. 校验 schema、字段、连续版本号、genesis 起点和父哈希链。
2. 校验段采样区间、段哈希和块元数据。
3. 对完整块直接恢复；对缺失或损坏的块，仅从已恢复段贡献重新叠加、限幅和重建该块。
4. 校验重建块哈希、限幅统计、最终内容哈希。
5. 任何断链、缺字段或无法重建的内容抛出 `E_RECOVERY_INVALID`。

`SnapshotManager.load_latest(directory)` 会扫描 `snapshot*.json`，跳过损坏文件并返回版本号最高的有效快照，覆盖强制终止后残留坏文件的情况。

## 并发与取消

- `RecomputeTaskManager` 对同一波形序列只允许一个后台重算任务；运行中重复提交返回 `E_BUSY`。
- 采样、区间叠加和块哈希阶段均设置检查点。
- 取消或异常不会发布候选版本，释放临时结果并保留最后一次已发布版本。
- 发布和查询共用锁边界；读者只能看到完整旧版本或完整新版本。
- 任务完成前，`Publisher` 中的旧版本仍可正常预览。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_WAVE_INVALID` | JSON/字段/段参数非法，尽量定位 `segment_index` |
| `E_VERSION_STALE` | 查询版本号非当前版本，或版本与内容哈希不匹配 |
| `E_RECOVERY_INVALID` | 快照断链、缺字段、哈希校验或重建失败 |
| `E_BUSY` | 已有重算任务运行 |
| `E_CANCELLED` | 任务在检查点协作取消 |
| `E_IO` | 本地文件读写或原子写入失败 |

## 测试与验证

```bash
./scripts/run_tests.sh
```

当前验证结果：

```text
Ran 25 tests
OK
```

覆盖项包括：

- 字段缺失、负时长、频率越界及段序号定位。
- 三种包络在 1 kHz / 2 kHz 下时长和逐点值一致。
- 频率、幅度、包络、分配比编辑的增量范围。
- 增量输出与全量重建的信号、段哈希、块哈希逐字节一致。
- 配置或总长度变化时保守全量重算。
- 硬/软限幅范围及统计。
- 同一操作序列的日志和驱动信号逐字节一致。
- 并发发布期间读者只看到完整版本。
- 过期版本查询返回 `E_VERSION_STALE`。
- 原子快照往返、缺块局部重算、坏块修复、断链/缺字段拒绝、最近有效快照扫描。
- `E_BUSY`、取消、异常后旧版本保留。
- Tk 窗口实际创建与关闭冒烟测试。

本机演示的实际结果：

```text
incremental_equals_full: true
recovery_equal: true
partial_recovery_equal: true
partial_recovery_recomputed_chunks: 1
stale_query_rejected: true
```

两次编辑后的最终信号 SHA-256：

```text
signal-latest.bin 68dbf49ff8ec2020f7e3937504447090e6c539159efda46c0459ee8efceee746
trace.jsonl       16a72c37ac51df984d4275c0770e101f368309a692b92d516564dc19c1bd4fe4
```

示例最终版本哈希链：

```text
v1 75278b0adbabf0ce413bc2929d6ac450f4768fcafe6c9454a2668c7cb3684c96
v2 6eac9e3a87bc56b0c321524d2153a112eb334222b79321ed7a05cd9d10528686
v3 5e33983ad26ad79913a0c206816f43c3165d4615a1753f934e49ca3df6077e8b
```

GUI 曲线实际渲染样例位于 `artifacts/gui-visual/chart.png`。

## 项目结构

```text
haptic/
  model.py        JSON 解析、字段校验、规范化模型
  engine.py       采样、包络、叠加、限幅、增量重算
  versioning.py   不可变版本、内容哈希、原子发布
  snapshot.py     原子快照、哈希链、局部恢复
  tasks.py        单任务后台线程、取消与任务状态
  trace.py        确定性 JSONL 轨迹
  samples.py      确定性样本生成
  cli.py          无界面演示与验证入口
  gui.py          Tkinter + Matplotlib 界面
tests/
  test_haptic.py  25 项自动化验收测试
scripts/
  generate_samples.py
  run_demo.py
  run_tests.sh
samples/          生成的本地 JSON 样本
artifacts/demo/   轨迹日志、快照、信号二进制、摘要
```

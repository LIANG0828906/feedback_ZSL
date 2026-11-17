# 鼓组离线母线渲染器

`drum_bus` 是一个只依赖 Python 标准库与 NumPy 的离线母线渲染组件。它面向板鼓、梆子、堂鼓和环境底噪四路确定性素材，完成清单校验、瞬态对齐、厅堂早期反射估计与补偿、立体声下混、BS.1770 响度/真峰值处理，以及按固定采样块提交快照的断点续算。

所有测试素材均由代码确定性合成，不访问网络，也不依赖外部媒体。

## 快速开始

要求 Python 3.10+、NumPy 1.24+。

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -e .

.venv/bin/python -m drum_bus generate --output artifacts/demo_case
.venv/bin/python -m drum_bus render \
  --manifest artifacts/demo_case/manifest.json \
  --output artifacts/demo_render \
  --block-size 4096
```

`generate` 会写入 `board_drum.wav`、`bangzi.wav`、`tanggu.wav`、`ambient.wav` 和 `manifest.json`。`render` 会写入立体声 `bus.wav`、`render_report.json` 和 `.render_state/` 快照目录。

也可以安装后使用命令行入口：

```bash
drum-bus generate --output artifacts/demo_case
drum-bus render --manifest artifacts/demo_case/manifest.json --output artifacts/demo_render
```

## 清单格式

相对路径以清单所在目录为基准。时间单位除特别标注外均为秒。

```json
{
  "schema_version": 1,
  "sample_rate": 48000,
  "bit_depth": 24,
  "seed": 241,
  "synthesis": {
    "duration_seconds": 5.0,
    "generator": "drum_bus.synth/v1"
  },
  "reflection": {
    "delays_ms": [9.5, 18.0],
    "gains_db": [-11.5, -16.5]
  },
  "channels": [
    {
      "channel": "board_drum",
      "instrument": "板鼓",
      "path": "board_drum.wav",
      "synth_profile": "board_drum",
      "start_offset_samples": 37,
      "reference_times": [0.42, 0.80, 1.18],
      "reflection_window": {"start_ms": 5.0, "end_ms": 40.0},
      "early_reflections": true,
      "mix_weight": 0.92,
      "pan": -0.32
    },
    {
      "channel": "bangzi",
      "instrument": "梆子",
      "path": "bangzi.wav",
      "synth_profile": "bangzi",
      "start_offset_samples": -23,
      "reference_times": [0.42, 0.80, 1.18],
      "reflection_window": {"start_ms": 6.0, "end_ms": 44.0},
      "early_reflections": true,
      "mix_weight": 0.68,
      "pan": 0.27
    },
    {
      "channel": "tanggu",
      "instrument": "堂鼓",
      "path": "tanggu.wav",
      "synth_profile": "tanggu",
      "start_offset_samples": 61,
      "reference_times": [0.42, 0.80, 1.18],
      "reflection_window": {"start_ms": 6.0, "end_ms": 46.0},
      "early_reflections": false,
      "mix_weight": 0.78,
      "pan": 0.04
    }
  ],
  "ambient": {
    "channel": "ambient",
    "instrument": "环境底噪",
    "path": "ambient.wav",
    "synth_profile": "ambient",
    "start_offset_samples": 0,
    "reference_times": [],
    "reflection_window": {"start_ms": 0.0, "end_ms": 1.0},
    "early_reflections": false,
    "mix_weight": 0.60,
    "pan": 0.0
  },
  "master": {
    "target_lufs": -16.0,
    "true_peak_db": -1.0
  },
  "render": {
    "block_size": 4096
  }
}
```

`master.target_lufs` 必填；`master.true_peak_db` 缺省为 `-1.0 dBTP`。`channels` 必须恰好三项，`ambient` 必须存在。`reflection_window` 使用相对每个参考点的毫秒偏移。`early_reflections` 为真的通道会由合成器加入清单指定的早期反射，并参与反射补偿。

## 校验与错误码

校验失败时进程返回 `2`，写出 `validation_report.json`，并在标准错误中输出含通道名和业务码的告警；不会写出母线。

| 业务码 | 含义 |
| --- | --- |
| `FILE_MISSING` | 清单或其引用的 WAV 不存在 |
| `WAV_FORMAT_ERROR` | WAV 头、数据块或格式不受支持 |
| `SAMPLE_RATE_MISMATCH` | WAV 采样率与清单不一致 |
| `BIT_DEPTH_MISMATCH` | WAV 位深与清单不一致 |
| `DUPLICATE_INSTRUMENT` | 乐器名称重复 |
| `DUPLICATE_CHANNEL_NAME` | 通道 ID 重复 |
| `REFERENCE_TIMES_NOT_ASCENDING` | 锣鼓经参考时刻不严格升序 |
| `CHANNEL_COUNT_MISMATCH` | 打击乐通道数不是三 |
| `MANIFEST_INVALID` | 字段缺失、类型错误或参数越界 |

渲染阶段的告警写入 `render_report.json`：

| 告警码 | 含义 |
| --- | --- |
| `DETECTION_FAILED` | 通道存在无法检测的瞬态；该通道不做对齐 |
| `REFLECTION_COMPENSATION_FAILED` | 压降、峰值保持或窗口外不变性未达阈值 |
| `MASTER_COMPLIANCE_FAILED` | 响度偏差超过 ±0.5 LU 或真峰值超限 |

## 处理流程

1. **确定性合成**：四路波形由清单整数种子和通道名派生的 SHA-256 子种子生成。板鼓、梆子、堂鼓使用不同瞬态和衰减包络；板鼓与梆子叠加清单给出的早期反射。全部通道按 `start_offset_samples` 做零填充平移，按清单位深进行 PCM 量化并生成 WAV 头。
2. **瞬态检测**：在每个参考时刻附近使用一阶差分高频瞬态函数定位点击起点。每个通道用所有点的中位偏差求一个整数采样平移量，平移后要求每点残差不超过 1 个采样。检测失败时记录 `DETECTION_FAILED`，平移量为空。
3. **反射估计与补偿**：用 12 kHz 高通突出点击成分，以环境底噪高通功率作为噪声底并从模板/候选能量中扣除，再在声明的延迟附近做模板回归，估计每个反射的整数延迟与 dB 增益。补偿使用清单窗口内 `-20 dB`、边界 1 ms 余弦渐变的增益曲线；窗口外样本保持逐样本不变。
4. **下混**：各通道先转为单声道，按等功率声像和 `mix_weight` 生成左右声道贡献，再对齐帧号相加。母线长度固定为最长输入通道帧数。报告同时给出各通道贡献能量占通道总能量的比例、占母线能量的比例和峰值。
5. **响度与真峰值**：48 kHz 使用 BS.1770 标准 K 加权系数，其他采样率由双线性设计系数生成。积分响度采用 400 ms 块、100 ms 步进、绝对 `-70 LUFS` 门限和相对 `-10 LU` 门限。真峰值通过 4 倍 Kaiser-sinc 过采样估计。限制器使用 2 ms 前瞻和 80 ms 释放，离线预计算增益，因此不引入输出延迟且采样数不变。
6. **分块提交**：完整处理后的 PCM 先原子写入状态文件，再按固定块号写 WAV。每写完一块，执行 `flush`/`fsync` 后原子替换 `snapshot.json`，其中记录块数、清单摘要、瞬态偏移、反射参数、状态文件哈希和已提交输出前缀哈希。

## 快照与中断

`render` 的状态目录结构：

```text
<output>/.render_state/
  snapshot.json
  state.npz
  state.json
```

- 清单摘要、块大小或已确定参数不匹配时，旧快照和状态整体丢弃并重算。
- `Ctrl+C` 只会设置中断标志；当前块写完后提交快照，写出 `recovery_report.json`，返回整数退出码 `0`。
- `SIGKILL` 不写恢复报告；下次启动从快照记录的已提交块继续，未提交的尾部字节会被截断。
- 续算和完整渲染对同一输入产生逐字节一致的母线。
- `--block-delay-ms` 仅用于中断测试，默认 `0`。

## 输出报告

`render_report.json` 的主要字段：

- `channels[].alignment_shift_samples`、`post_alignment_errors_samples`：整数对齐量与逐点残差。
- `channels[].reflection`：声明/估计延迟、估计增益、置信度、窗口能量压降、瞬态峰值损失、窗口外不变性。
- `channels[].contribution_ratio`、`bus_energy_ratio`、`peak_dbfs`：混音贡献和峰值。
- `master.measured_lufs`、`deviation_lu`、`true_peak_dbtp`、`limiter_triggered_samples`：母带合规指标。限制器触发样本按立体声帧计数，另提供按声道样本计数的字段。
- `block_render`：块大小、已完成块数、总块数和输出总帧数。

## 测试

```bash
.venv/bin/python -m unittest discover -v
```

测试覆盖：

- 24-bit WAV 读写和合成字节确定性；
- 缺失文件、采样率/位深不符、重复乐器、非升序参考时刻及 CLI 中止报告；
- 三路瞬态对齐、反射延迟/增益估计、至少 12 dB 压降、峰值保持和窗口外不变；
- BS.1770 参考响度、真峰值和限幅长度保持；
- 完整渲染与两次运行的字节一致性；
- `Ctrl+C` 提交当前块、退出码 0、恢复报告及续算一致性；
- `SIGKILL` 无报告、快照恢复及续算一致性；
- 清单变化时丢弃旧进度并重算。

实际验收数据和分母见 [VERIFICATION.md](VERIFICATION.md)。

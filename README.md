# 确定性童声合唱离线母带渲染器

本项目从空工作区实现一条可独立运行的 48 kHz 离线母带流水线：读取 JSON 工程清单、校验八路单声道 WAV、按固定种子生成可复算素材、检测男高换声点、补偿频带音色、校正四声部能量、渲染立体声母带，并支持固定采样块快照与中断续算。

运行环境只需要 Python 3.10+、NumPy 与 SoundFile；不依赖网络、GPU、容器或外部媒体。

## 安装

```bash
uv venv --python 3.10 .venv
uv pip install --python .venv/bin/python -r requirements.txt
```

若已有虚拟环境，也可使用：

```bash
python -m pip install -r requirements.txt
```

## 快速开始

生成确定性工程清单、八路 24-bit/48 kHz WAV、换声点真值与参考通道复算报告：

```bash
.venv/bin/python -m choir_master demo --dir artifacts/demo
```

独立复算四个声部的参考通道：

```bash
.venv/bin/python -m choir_master verify-references \
  --manifest artifacts/demo/project.json
```

完整渲染立体声母带：

```bash
.venv/bin/python -m choir_master render \
  --manifest artifacts/demo/project.json \
  --output artifacts/demo/master.wav \
  --snapshot artifacts/demo/snapshot.json \
  --report artifacts/demo/render_report.json \
  --fresh
```

`--fresh` 会删除已有快照并从第 0 块开始。省略它时，渲染器读取并校验快照后续算。

## 工程清单

示例清单由 `choir_master.synth.default_manifest()` 生成。核心字段如下：

```json
{
  "schema_version": 1,
  "seed": 2532026,
  "sample_rate": 48000,
  "bit_depth": 24,
  "channels": 1,
  "duration_samples": 384000,
  "transition_band_hz": [1200.0, 3000.0],
  "passaggio_hz": 329.63,
  "target_loudness_lufs": -16.0,
  "true_peak_limit_dbtp": -1.0,
  "block_size": 8192,
  "voices": [
    {
      "voice": "tenor",
      "label_zh": "男高",
      "target_energy_ratio": 0.24,
      "downmix_weights": [0.65, 0.76],
      "tracks": [
        {
          "id": "tenor_m1",
          "path": "audio/tenor_m1.wav",
          "mic_position": {"distance_m": 1.3, "azimuth_deg": -22.0},
          "reference": true
        }
      ]
    }
  ]
}
```

- 工程必须声明 `soprano`、`alto`、`tenor`、`bass` 四个声部且各两路麦克风。
- 四声部 `target_energy_ratio` 之和必须为 1。
- 每声部必须恰好有一个 `reference: true` 通道。
- 输入 WAV 必须是清单声明的 48 kHz、位深和单声道格式。
- `mic_position` 同时决定同源素材的传播延迟与衰减。

## 处理链

1. **校验**：缺失文件、采样率/位深/声道不匹配、声部缺失或重复、非有限样本等均形成带文件路径和业务码的 JSON 告警，并在写入任何渲染产物前终止。
2. **确定性生成**：按整数种子为每声部生成持续元音式谐波、颤音和交替停顿；两路麦克风共享同一源信号。男高在 F4/A4 音高段注入 26 dB 的高频音色衰减。
3. **换声点检测**：在 1200–3000 Hz 频带计算双麦克风帧能量比，按同一活跃乐句内的局部参考电平检测突变，避免 E4/A4 音区差异造成误报。
4. **音色补偿**：仅修改检测区间内的男高。以区间两侧频带能量为基准，对频带分量施加平滑增益包络；区间外样本逐位不变，并迭代校正到频带突变下降至少 8 dB。
5. **声部平衡**：以目标比例反算各声部线性增益，按每声部 `downmix_weights` 合成立体声；全部通道等长补零，不做时间平移。
6. **响度与真峰值**：实现 ITU-R BS.1770-4 K 加权、400 ms/100 ms 分块、绝对与相对门限积分响度；真峰值采用 4 倍过采样测量。必要时使用 48 样本前瞻限幅并补偿延迟，保持总采样数不变。
7. **分块快照**：每块完成后原子覆盖 `snapshot.json`。快照含清单摘要、输入 WAV 摘要、已完成块数、确认换声点、声部增益以及已渲染母带浮点字节，续算不依赖未提交内存状态。

## 中断语义

- `Ctrl+C` / `SIGINT`：完成当前块、原子提交快照、写恢复报告，进程以退出码 `0` 结束。后续同一命令自动续算。
- `SIGTERM` / 强制终止：不安装处理器，不保证退出报告；下一次启动从最后一次完整快照继续。
- 快照损坏、清单摘要变化、WAV 输入摘要变化或已确认检测/增益结果不一致：删除旧快照，输出 `E_RESUME_MISMATCH`，退出码 `3`。

## 业务错误

常见错误码：

- `E_VALIDATION_FAILED`：校验汇总；`issues[]` 中分别带 `E_FILE_MISSING`、`E_SAMPLE_RATE_MISMATCH`、`E_BIT_DEPTH_MISMATCH`、`E_CHANNEL_COUNT_MISMATCH`、`E_VOICE_MISSING`、`E_VOICE_DUPLICATE` 等具体码与文件路径。
- `E_RESUME_MISMATCH`：快照不再可信，旧进度已丢弃。
- `E_COMPENSATION_INSUFFICIENT`：补偿后突变下降不足 8 dB。
- `E_LOUDNESS_UNACHIEVABLE` / `E_TRUE_PEAK_EXCEEDED`：无法同时满足响度与真峰值目标。
- `E_FINAL_BALANCE_FAILED`：最终母带声部比例偏差超过 0.02。

CLI 的最终状态、恢复状态和错误状态均输出为单行 JSON，便于外部程序解析。

## 测试

```bash
.venv/bin/python -m unittest discover -s tests -v
```

测试覆盖生成字节确定性、参考通道复算、结构化校验告警、检测 RMSE、补偿 8 dB 与区间外不变性、补偿确定性、BS.1770 校准、限幅长度保持、响度/真峰值/声部比例、`SIGTERM` 续算、`SIGINT` 恢复报告、续算逐字节一致和 `E_RESUME_MISMATCH`。实际测量结果见 [VALIDATION.md](VALIDATION.md)。

## 代码结构

- `choir_master/manifest.py`：清单解析、音频格式校验、结构化问题收集。
- `choir_master/synth.py`：确定性八轨生成、真值、参考通道复算。
- `choir_master/detection.py`：男高换声点检测与非目标声部误报复核。
- `choir_master/compensation.py`：频带平滑衔接、对比指标、区间外不变性检查。
- `choir_master/loudness.py`：BS.1770 响度、4 倍真峰值、前瞻限幅。
- `choir_master/snapshot.py`：校验和快照、原子持久化、恢复一致性检查。
- `choir_master/render.py`：分块状态机、声部平衡、母带写出和最终报告。


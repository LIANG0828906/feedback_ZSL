# 离线广播剧情母带渲染器

这是一个仅依赖 Python 3.11 与 NumPy 的离线母带组件。输入为 JSON 工程清单和清单声明的 WAV 分轨；组件可确定性地合成测试素材，并完成校验、采样级对齐、立体声/单声道兼容折叠、解说闪避、BS.1770 响度与真峰值合规、分块快照和中断恢复。

渲染过程不访问网络、不需要音频设备，也不依赖外部录音。

## 快速开始

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
.\.venv\Scripts\python -m mastering.cli synthesize --manifest manifest.example.json
.\.venv\Scripts\python -m mastering.cli render --manifest manifest.example.json --fresh
```

Linux/macOS：

```bash
python3.11 -m venv .venv
.venv/bin/pip install -r requirements.txt
.venv/bin/python -m mastering.cli synthesize --manifest manifest.example.json
.venv/bin/python -m mastering.cli render --manifest manifest.example.json --fresh
```

示例产物：

- `deliverables/master_stereo.wav`：24 bit / 48 kHz 立体声母带。
- `deliverables/master_mono.wav`：按清单折叠规则生成、与立体声母带等长的单声道母带。
- `deliverables/mastering_report.json`：逐轨偏移/时长、折叠能量、闪避区间、限幅与响度报告。
- `state/render.snapshot.json`：最近一个完整渲染块的原子快照。

## CLI

```text
python -m mastering.cli validate   --manifest PATH
python -m mastering.cli synthesize --manifest PATH
python -m mastering.cli render     --manifest PATH --fresh
python -m mastering.cli render     --manifest PATH --resume
```

- `--fresh` 是默认行为，删除旧快照后从第 0 块渲染。
- `--resume` 只从已提交快照续算；快照不存在时从第 0 块开始。
- `validate` 同时检查 JSON 声明和实际 WAV 的采样率、位深、声道数。

退出码：

- `0`：成功，或 Ctrl+C 可处理中断且当前块与恢复报告已提交。
- `2`：清单/WAV 校验失败，错误码前缀通常为 `E_*`。
- `3`：续算快照与当前清单摘要、素材哈希或已确定参数不一致，旧快照已整体删除。
- `4`：渲染、响度、折叠、闪避或内部处理失败。

失败时 stdout 输出单行结构化 JSON，例如：

```json
{"event":"render_failed","exit_code":2,"errors":[{"code":"E_SAMPLE_RATE_MISMATCH","message":"actual WAV sample rate differs from declaration","track":"dialogue_a","details":{"actual":44100,"declared":48000}}]}
```

## 清单契约

`manifest.example.json` 是可运行模板。所有相对路径均相对清单文件所在目录。

主要字段：

| 字段 | 含义 |
| --- | --- |
| `seed` | 非负整数合成种子；同一清单重复生成逐字节一致。 |
| `sample_rate`, `bit_depth` | 全局母带参数，输出支持 16/24/32 bit PCM。 |
| `dialogue_points_seconds` | 严格升序的画面对白点，必须在素材和成片时间轴内。 |
| `mono_fold.rule` | `sum_0.5`、`equal_power` 或 `weighted_sum`。 |
| `mono_fold.energy_tolerance` | 立体声/单声道中频能量占比允许的绝对偏差。 |
| `mono_fold.band_hz` | 中频能量统计频带，示例为 300–3400 Hz。 |
| `loudness.target_lufs` | ITU-R BS.1770 积分响度目标。 |
| `loudness.true_peak_dbtp` | 真峰值上限，默认 `-1.0 dBTP`。 |
| `ducking.depth_db` | 解说区间音乐增益，示例 `-12 dB`。 |
| `ducking.attack_ms`, `recovery_ms` | 区间内部的平滑压低/恢复时长。 |
| `ducking.sir_min_improvement_db` | 解说频段信干比最低提升量。 |
| `render.block_frames` | 固定渲染块大小。 |

每个分轨声明：

- `name`：唯一分轨名，也是报告和错误中的 `track`。
- `kind`：`dialogue`、`narration` 或 `music`。
- `path`：WAV 相对路径。
- `sample_rate`, `channels`, `bit_depth`：必须与素材实际值一致。
- `offset_seconds`：清单声明的起点偏移；报告保留其整数采样值。
- `synth`：确定性合成参数。对白使用 `cue_indices`、`jitter_samples`、`f0_hz` 等；解说使用 `segments`；音乐使用持续和弦参数。

## 处理口径

1. **校验优先**：所有文件元数据和对白点先在内存中验证。任何错误都不会写入母带；错误对象包含业务码和分轨名。
2. **确定性合成**：每个分轨使用由全局 seed、轨序号和用途派生的独立 `PCG64` 随机流。对白在清单对白点附近加入可复算采样抖动、爆破音和元音式谐波；解说与持续音乐铺底重叠。
3. **采样级对齐**：在预期对白点附近检测首个显著瞬态，只允许整数采样平移。所有同步事件对齐后的误差必须不超过 1 sample；母带长度严格取最长平移后分轨长度。
4. **兼容折叠**：按清单规则生成独立单声道 WAV。报告在总体和每个关键区间比较对白/解说在中频段的能量占比，并检查折叠后真峰值。
5. **动态闪避**：只在解说区间内对音乐应用平滑曲线；区间外增益严格为 1。报告记录每个区间的采样边界、时刻、增益、实际淡入/恢复长度和 SIR 提升。
6. **响度与峰值**：实现 BS.1770-4 K-weighting、400 ms 块、75% 重叠及绝对/相对门限。真峰值采用 4× 窗化 sinc 多相插值估计。限幅器无延迟、不改变采样数，并以 0.1 dB 内部保护余量复测。
7. **分块与快照**：每个块完成后，用同目录临时文件、`fsync` 和 `os.replace` 整体替换快照。快照包含清单摘要、素材 SHA-256、已完成块数、对齐/闪避参数和已完成浮点混音前缀。
8. **中断语义**：SIGINT 只设置中断标志，当前块计算和快照提交完成后写 `recovery_report.json` 并以 0 退出。SIGKILL/崩溃不写恢复报告，下次 `--resume` 从最后完整快照继续。摘要、素材哈希或参数变化时删除旧快照并以 `E_RESUME_MISMATCH`/退出码 3 结束。

## 测试

```bash
.venv/bin/python -m unittest discover -s tests -v
```

测试覆盖：

- 合成 WAV 的逐字节确定性。
- 实际 WAV 元数据错误、分轨业务码和无部分母带写出。
- 对白点超出实际素材时长的分轨级错误。
- 全链路采样级对齐、折叠能量、SIR、响度、真峰值和重复报告一致性。
- 强制中断后快照续算与一次性渲染逐字节一致。
- Ctrl+C 分支完成当前块、提交恢复报告和续算。
- 快照摘要变化时 `E_RESUME_MISMATCH` 与旧进度删除。
- BS.1770 基准正弦、跨采样真峰值及无延迟限幅器。

实际执行结果与产物哈希见 `VERIFICATION.md`。

## 测试钩子

下列环境变量仅用于自动化验收，不参与正常渲染：

- `MASTERING_TEST_CRASH_AFTER_BLOCK=N`：提交第 N 块快照后直接 `os._exit(137)`。
- `MASTERING_TEST_INTERRUPT_AFTER_BLOCK=N`：提交第 N 块后走可恢复中断路径。

生产环境的 SIGINT/SIGKILL 行为不依赖这些钩子。

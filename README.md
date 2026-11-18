# stringmaster

确定性的四路弦乐换弦修复与母带渲染组件。组件只依赖 Python 标准库与 NumPy，不访问网络、不依赖外部媒体或图形环境。

## 环境与入口

- Python 3.10 或 3.11
- NumPy 1.24 或更高
- Windows 10/11 或 Linux

交互入口为 `python -m stringmaster`。

```text
python -m stringmaster demo --root demo --duration 8 --seed 20261118
python -m stringmaster validate --manifest demo/manifest.json
python -m stringmaster generate --manifest demo/manifest.json
python -m stringmaster render --manifest demo/manifest.json \
  --output demo/render/master.wav \
  --state demo/render/state.json \
  --report demo/render/report.json \
  --recovery-report demo/render/recovery.json
```

`demo` 会生成 `manifest.json`、四路 WAV 和 `score_truth.json`。`render` 默认写到清单目录下的 `render/`。

## 清单格式

```json
{
  "schema_version": 1,
  "seed": 20261118,
  "sample_rate": 48000,
  "bit_depth": 16,
  "duration_seconds": 8.0,
  "block_size": 8192,
  "crossover_band": {"low_hz": 2500.0, "high_hz": 5200.0},
  "target_loudness_lufs": -20.0,
  "true_peak_limit_dbtp": -1.0,
  "tracks": [
    {
      "path": "tracks/violin_1.wav",
      "instrument": "第一小提琴",
      "distance_m": 0.65,
      "azimuth_deg": -28.0,
      "target_energy_ratio": 0.27,
      "downmix_weights": [1.0, 0.82]
    }
  ]
}
```

必须恰好包含 `第一小提琴`、`第二小提琴`、`中提琴`、`大提琴` 各一路。`downmix_weights` 是左右声道权重；单声道输入会复制到左右声道后再乘权重。四路声道的数量必须一致。母带为双声道，帧数取四路中最长通道的帧数。

支持的清单位深为 16/24/32-bit；读取端同时支持 IEEE float 32/64-bit WAV，且清单声明的位深必须与实际文件一致。

## 处理流程

1. **确定性素材合成**  
   由整数种子和清单参数建立每件乐器的音符时间线、约 4.6–6.2 Hz 的揉弦、弓法瞬态、交替停顿与谐波。第一小提琴在换弦频带内注入 2–3 段固定频点的音色突变。每条分轨再按距离、方位、左右路径差和到达时差做衰减及分数延迟。计时只使用 `SeedSequence`/`PCG64`，同参数重复生成逐字节一致。

2. **换弦区间检测**  
   对第一小提琴做 2048 点 Hann STFT。以帧内“换弦频带功率 / 有效总功率”的 dB 比为特征，以全曲有效帧中位数为基线；超过基线 7 dB、持续至少 180 ms 的连续区间即为候选。区间边界取 STFT 帧时间，避免额外平滑把边界扩展数十毫秒。

3. **频带局部补偿**  
   对检测区间做 75% 重叠的 STFT/OLA。只在换弦频带内按帧能量比降低增益，并把区间外样本直接保留为原始样本。区间首尾各做短交叉淡化，因此区间外第一小提琴样本零修改。报告逐段记录补偿前后频带能量、突变量和最大修改幅度。

4. **声部平衡**  
   设第 `i` 路下混后能量为 `E_i`、目标比例为 `r_i`，声部增益为

   ```text
   g_i = sqrt(r_i / E_i)
   ```

   四路贡献相加后，对全部声部应用同一个母带增益/限幅包络。公共包络不会破坏声部时间对齐。

5. **响度与真峰值**  
   积分响度按 ITU-R BS.1770 的 K 加权、400 ms 块、100 ms 步进、绝对门限与相对门限计算。真峰值使用 4 倍 FFT 过采样。若标称增益后真峰值超限，则使用 4 ms 前瞻峰值限幅包络；限幅器零额外延迟，母带帧数不变。若限幅导致声部比例偏差超过 0.02，渲染以业务错误结束而不输出不合格母带。

6. **分块与快照**  
   每个固定采样块编码后立即写入母带、`flush` 并 `fsync`，随后用同目录临时文件加 `os.replace` 原子覆盖 `state.json`。快照包含清单全文与摘要、已确认换弦区间、补偿报告、声部增益、母带公共校准、源文件 SHA-256、已完成块数、块大小、累计 PCM 摘要和计划指纹。

7. **恢复**  
   重启时先读取快照，再按当前清单与 WAV 重建计划。清单摘要、源文件摘要或计划指纹不一致时，删除旧母带、状态和报告，并以 `E_RESUME_MISMATCH` 退出。一致时先把母带截断到快照确认的长度，再验证 PCM 前缀 SHA-256，从下一块继续。

`Ctrl+C` 通过 SIGINT 标志处理：当前块完成后才检查标志，提交该块快照、写出恢复报告并以退出码 0 结束。强制终止不写退出报告；下次启动只使用最后一次已提交快照。测试可设置 `STRINGMASTER_TEST_BLOCK_SLEEP_MS` 在每次快照后加入延时，便于稳定构造终止窗口；该变量不影响音频结果和报告。

## 业务码

清单和素材验证失败时不进入渲染，CLI 在 stderr 输出 JSON `errors` 数组，每项都有 `code`、`file`、`message`。

| 业务码 | 含义 |
| --- | --- |
| `E_FILE_MISSING` | 清单或 WAV 不存在 |
| `E_SAMPLE_RATE_MISMATCH` | WAV 实际采样率与清单不同 |
| `E_BIT_DEPTH_MISMATCH` | WAV 实际位深与清单不同 |
| `E_CHANNEL_MISMATCH` | 四路声道数不一致 |
| `E_CHANNEL_UNSUPPORTED` | 声道数不是单声道或双声道 |
| `E_INSTRUMENT_MISSING` | 四件乐器有缺失 |
| `E_INSTRUMENT_DUPLICATE` | 乐器重复 |
| `E_RATIO_SUM` | 目标能量比例之和偏离 1 超过 `1e-6` |
| `E_BAND_INVALID` | 换弦频带顺序或 Nyquist 边界非法 |
| `E_RESUME_MISMATCH` | 快照与当前清单、源文件或确认结果不一致 |
| `E_STATE_CORRUPT` | 快照或已提交母带前缀损坏 |
| `E_COMPENSATION` | 补偿后频带突变下降不足 8 dB |
| `E_LOUDNESS_TARGET` | 积分响度无法达到 ±0.5 LU |
| `E_TRUE_PEAK_LIMIT` | 真峰值无法达到声明上限 |
| `E_ENERGY_RATIO` | 最终声部能量占比偏差超过 0.02 |

CLI 退出码：`0` 成功或可处理中断，`2` 清单/素材校验失败，`3` 续算不一致，`4` 渲染业务错误。

## 报告

完整报告字段包括：

- `transition_intervals` 与 `transition_intervals_by_instrument`
- `compensation`：每段补偿前后频带 dBFS、基线、前后突变量、下降量和最大修改幅度
- `voice_gain_db`、目标/实测声部比例及最大绝对偏差
- `loudness`、`true_peak`、`limiter`、`channel_peaks_dbfs`、`voice_peaks_dbfs`
- `blocks`、`snapshot_commits`、`master.sha256`、帧数、采样率、位深

报告不写时间戳或耗时字段，相同输入和参数重复运行逐字节一致。

## 测试

```text
python -m unittest discover -v
```

测试覆盖清单错误码与文件路径、三类素材元数据不一致、生成逐字节一致性、检测真值误差、其他乐器误报、区间外零修改、补偿下降量、母带比例/响度/真峰值、两次渲染一致、块大小无关性、限幅分支、SIGKILL 续算、SIGINT 退出语义和 `E_RESUME_MISMATCH`。

本机实测（Python 3.14.4、NumPy 2.5.5；代码仅使用 3.10 可用语法与 NumPy 1.24 可用 API）：

- `python -m unittest discover -v`：12 项全部通过，约 9.3 s。
- 8 s、48 kHz、16-bit、seed `20261118`：检测出 2 段注入区间，四端点相对真值的 RMS 误差约 6.5 ms；两段补偿分别下降 18.57 dB、19.22 dB。
- 声部实测占比为 `0.27 / 0.24 / 0.24 / 0.25`，最大绝对偏差 `0.0`。
- 母带积分响度 `-20.000002 LUFS`，目标偏差 `-0.0000015 LU`；真峰值 `-10.695 dBTP`，未触发限幅，帧数 `384000`。
- 母带 SHA-256：`03956fc96603657a93191be03cd8ebf4bbe7aa17f80213cfe13893551ec0d166`。
- 将真峰值上限改为 `-12.0 dBTP` 时触发 372280 个限幅样本，最大衰减约 1.78 dB，最终真峰值 `-12.160 dBTP`，声部比例最大偏差约 0.0009。
- SIGKILL 后从已提交块续算、SIGINT 后完成当前块并退出 0；两条路径的最终母带和完整报告均与一次性渲染逐字节一致。

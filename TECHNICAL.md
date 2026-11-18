# 技术实现说明

## 流水线

```
manifest + 8 WAV
      |
      +-- schema/file validation
      |
      +-- STFT band/total energy -> breakpoint intervals
      |
      +-- zero-phase band split -> interval-only compensation
      |
      +-- per-voice mic downmix -> target-energy gain solve
      |
      +-- equal-power stereo pan -> BS.1770 normalization
      |
      +-- 4x true-peak measurement -> look-ahead safety limiter
      |
      +-- 24/16-bit quantization -> atomic PCM block snapshot
      |
      +-- RIFF/WAVE assembly + stable report
```

## 确定性素材生成

每个声部的共享源由 `manifest.seed`、声部序号和清单中的 `synthesis` 参数决定：

- 持续元音式谐波：固定基频、确定性相位、轻微 4.7 Hz 颤音。
- 交替停顿：固定的短语包络段和 25 ms 余弦淡入淡出。
- 男高突变：仅对落在该声部 `breakpoint_band_hz` 内的谐波乘上清单 `mutation_gain_db`，过渡为 20 ms 余弦包络。
- 两路麦克风共享同一源，分别按 `delay_ms` 做整数样本延迟、按 `gain_db` 衰减。
- 不使用时间种子、全局随机数或未播种 RNG。

突变起始时刻由整数种子确定：

```
start = (0.320 + (seed % 50) / 1000) * duration
end   = start + 0.100 * duration
```

## 换声点检测

检测只读取音频和清单频带，不读取 `generation.mutation_intervals_s`。

1. 对每个声部所有麦克风做 2048 点 Hann STFT，帧移 240 点（5 ms）。
2. 汇总总功率和频带功率，计算 `10*log10(band/total)`。
3. 用总功率中位数的 10% 区分有效演唱帧与停顿。
4. 以有效帧的频带/宽带比中位数作为基线。
5. 低于基线 4 dB 的连续帧候选段，经 3 帧中值滤波和小间隙合并。
6. 沿 1.5 dB 肩部扩展边界；最短事件 120 ms。
7. 报告绝对频带能量落差、归一化比例落差和起止时间。

没有注入突变的声部不会因停顿或包络淡入淡出产生候选段。

## 频带补偿

对男高两路麦克风联合信号做四阶 Butterworth 零相位带通：

```
band = bandpass(x)
gain_env = 1 + (g - 1) * raised_cosine_mask(interval)
y = x + (gain_env - 1) * band
```

- 补偿增益由检测区间中央 50% 的频带能量与有效演唱基线求解。
- 增益在区间内 25 ms 余弦过渡，区间边界严格回到 1。
- 区间外完全不执行加法，因此男高检测区间外样本逐位不变。
- 女低等其他声部不经过补偿函数，样本逐位不变。
- 报告记录补偿前后频带能量、改善量、实际增益和最大修改幅度。

## 声部平衡与立体声

每路麦克风先按声道均值转单声道，再按清单 `downmix_weight` 求和为声部总线。若声部原始能量为 `E_i`、目标比例为 `t_i`，校正系数为：

```
g_i = sqrt(t_i / E_i)
```

所有声部使用同一最终响度增益和同一限幅增益包络，因此不会破坏声部比例。声部总线经等功率声像：

```
L_i = bus_i * cos((pan_i + 1) * pi / 4)
R_i = bus_i * sin((pan_i + 1) * pi / 4)
```

母带总帧数取所有输入通道最大长度。混音仅做零填充和逐样本加法，不引入额外延迟。

## 响度、真峰值与限幅

- 积分响度按 ITU-R BS.1770-4：48 kHz K 加权、400 ms 块、100 ms 步进、-70 LUFS 绝对门限和 -10 LU 相对门限。
- 真峰值按 4 倍多相过采样测量。
- 若峰值超限，使用零输出延迟的前瞻限幅：由未来 8 ms 峰值构造所需增益，再做 40 ms 平滑和未来最小值封顶。
- 限幅不插入额外样本；母带总帧数保持不变。
- 内部留 0.55 dB 真峰值裕量，最后再按量化后母带复测，必要时做整体安全缩放。
- 若目标响度与真峰值上限在物理上不可同时满足，返回非零处理错误，而不是输出不合规母带。

## 快照原子性

`state.json` 与 PCM 块均采用“同目录临时文件 + flush/fsync + `os.replace`”：

1. 写块临时文件并 `fsync`。
2. 原子改名到 `block-XXXXXXXX.pcm`。
3. 原子替换新的 `state.json`，此时该块才算已提交。

因此强制终止最多留下未纳入状态的孤儿块，不会被续跑采用。恢复时逐块核对文件大小和 SHA-256。

状态中的 `analysis` 是检测区间与最终声部校正系数的规范化快照。当前输入重新分析后若与快照不一致，按 `E_RESUME_MISMATCH` 整体丢弃。

## 可复算性

- JSON 使用固定字段插入顺序和固定小数位。
- WAV 使用自实现的确定性 RIFF/PCM 写入器，不依赖 libsndfile 的元数据默认值。
- 报告不含时间戳、绝对路径或运行耗时。
- 量化后再计算最终响度、真峰值与声部贡献，报告与母带一致。


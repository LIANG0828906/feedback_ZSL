# Rainforest Loop Master

面向开放世界雨林声景的离线、确定性母带渲染工程。工程从零按固定整数种子合成 7 路单声道分轨，再完成循环交叉淡化、声像稳定化、鸟鸣/雨幕掩蔽解耦、立体声与单声道母带、BS.1770 响度与真峰值合规检查，以及可中断续算的原子进度快照。

## 交付内容

- `src/rainforest_master/`：合成、清单校验、DSP、分块渲染、快照、恢复和差异核验代码。
- `rainforest_master.py`：无需安装包即可运行的命令行入口。
- `tests/test_pipeline.py`：8 项端到端与故障注入测试。
- `artifacts/rainforest_600s/`：固定种子 `249` 的 10 分钟实际验证工程与母带。
- 全部音频为 48 kHz IEEE float32 WAV；输入分轨为单声道，输出为立体声和单声道。

## 环境与运行

支持 Python 3.11+、Windows 10/11 和 Linux。运行时只依赖 NumPy 与 soundfile/soundfile 自带 libsndfile。

```powershell
py -3.11 -m pip install -r requirements.txt

# 生成清单 -> 合成七轨 -> 渲染
py -3.11 rainforest_master.py demo --out .\out\demo

# 或分步执行
py -3.11 rainforest_master.py make-demo --out .\out\demo --duration 600
py -3.11 rainforest_master.py synth --manifest .\out\demo\manifest.json
py -3.11 rainforest_master.py render --manifest .\out\demo\manifest.json --out-dir .\out\demo\master
```

```bash
python3 -m unittest discover -s tests -v
```

`demo`/`synth` 使用的所有随机数均来自清单种子与每轨固定偏移。WAV 由工程内确定性 RIFF writer 写出，不使用带时间戳的 `PEAK` 块，因此同一清单重复合成、重复渲染的输出逐字节一致。

## 目录与产物

每次 `render` 在输出目录生成：

- `rainforest_stereo_master.wav`：立体声母带，采样数严格等于 `duration_seconds * 48000`。
- `rainforest_mono_master.wav`：按清单折叠权重得到的单声道兼容版本。
- `render_report.json`：循环、声像、掩蔽、响度、真峰值、限幅、折叠与文件哈希统计。
- `.render_state/snapshot.json` 与 `.render_state/blocks/`：已提交块快照与原始分块数据。
- `recovery_report.json`：仅 Ctrl+C 等可处理中断后生成。
- `render_differences.json`：使用 `--verify-reference` 且发现差异时生成。

## 清单格式

完整示例见 `artifacts/rainforest_600s/project/manifest.json`。核心字段：

```json
{
  "seed": 249,
  "sample_rate": 48000,
  "duration_seconds": 600.0,
  "output_bit_depth": 32,
  "loop": {
    "crossfade_seconds": 2.0,
    "max_seam_sample_delta": 0.08,
    "seam_energy_length_seconds": 0.02,
    "endpoint_seal_seconds": 0.02
  },
  "panning": {"stability_threshold_db": 0.01},
  "masking": {
    "reference_scene": "雨幕",
    "target_scenes": ["鸟鸣"],
    "windows": [[5.48, 5.82]],
    "residual_energy_ratio": 0.15,
    "required_snr_improvement_db": 6.0
  },
  "mono": {
    "fold_weights": [0.5, 0.5],
    "mid_band_hz": [300.0, 5000.0],
    "energy_ratio_tolerance_db": 1.0
  },
  "loudness": {
    "target_lufs": -18.0,
    "max_abs_deviation_lu": 0.5,
    "true_peak_limit_dbtp": -1.0
  },
  "rendering": {"block_frames": 48000, "snapshot_every_blocks": 1},
  "tracks": [
    {
      "name": "rain_drops",
      "scene_name": "雨滴",
      "path": "stems/01_rain_drops.wav",
      "channels": 1,
      "sample_rate": 48000,
      "bit_depth": 32,
      "surround_weights": [0.82, 0.56],
      "mix_weight": 0.19,
      "transient_reference_times": [0.35, 0.91],
      "pan_motion": {"depth_radians": 0.16, "cycles": 1.0, "phase": 0.37},
      "synthesis": {"kind": "rain_drops", "seed_offset": 101}
    }
  ]
}
```

七轨的 `scene_name` 必须恰好覆盖：`雨滴`、`雨幕`、`溪流`、`虫鸣`、`鸟鸣`、`雷声`、`脚步`，且不得重复。每轨分轨文件必须实际为 48 kHz 单声道，位深与 `bit_depth` 一致，并包含完整循环长度加交叉淡化尾部，即至少 `round(duration_seconds * 48000) + round(loop.crossfade_seconds * 48000)` 帧。

校验失败时不进入渲染。stderr 输出：

```json
{
  "code": "STRUCTURED_ERROR",
  "errors": [
    {
      "code": "STRUCTURED_ERROR",
      "channel": "rain_drops",
      "scene": "雨滴",
      "field": "sample_rate",
      "message": "actual sample rate must be 48000, got 44100"
    }
  ]
}
```

## 处理模型

### 确定性合成

- 雨滴：清单参考时刻起音的短噪声/音调混合瞬态。
- 雨幕：按种子生成的持续宽带噪声，经确定性 FIR 整形。
- 溪流：窄带噪声，叠加极慢的确定性幅度起伏。
- 虫鸣：参考时刻开始的规律脉冲串。
- 鸟鸣：短促、带频率扫描与谐波包络的瞬态。
- 雷声：稀疏低频噪声与正弦轰隆叠加。
- 脚步：相隔约 0.16 秒的成对溅水脉冲。

所有事件从 `round(reference_time_seconds * 48000)` 采样帧开始注入，合成报告记录每个参考时刻对应的采样位置，可由清单复算。

### 循环

源素材长度为 `N + F`，`N` 是母带帧数，`F` 是交叉淡化帧数。输出前 `F` 帧采用等功率：

```text
theta = pi/2 * (n + 0.5) / F
y[n] = x[n] * sin(theta) + x[N+n] * cos(theta)
```

因此输出末尾自然接到源素材 `N` 处。最后再在接缝两侧用短余弦斜坡做端点匹配，使 `y[N-1]` 与 `y[0]` 的差趋近于 0；报告同时记录交叉淡化前的源接缝差、声像/掩蔽后的接缝差、首尾短时能量与接缝能量跳变。

### 声像

每轨的 `surround_weights = [left, right]` 被转换为等功率基础角：

```text
theta0 = atan2(sqrt(right), sqrt(left))
theta(n) = clamp(theta0 + depth * sin(2*pi*cycles*n/(N) + phase), 0.12, pi/2-0.12)
L(n), R(n) = cos(theta(n)), sin(theta(n))
```

声像函数是采样级解析式，没有随机跳变或侧向切换。鸟鸣、雨滴的每个参考时刻都记录期望与实测耳间差异；全部七轨记录平均耳间差异、相邻采样最大变化量和稳定性结论。

### 鸟鸣/雨幕解耦

清单 `masking.windows` 声明需要处理的窗；若未声明，则按 `window_before_seconds`/`window_after_seconds` 围绕鸟鸣参考时刻生成。参考雨幕通道提供噪声本底，鸟鸣通道在瞬态核心窗内检测。检测后只对雨幕参考轨应用梯形衰减：

```text
core_gain = sqrt(residual_energy_ratio)
核心窗残余能量比例 = core_gain^2
SNR 提升 = -10*log10(residual_energy_ratio)
```

当前参数 `residual_energy_ratio = 0.15`，故理论提升 `8.239 dB`。窗边界有短 attack/release 斜坡；窗外增益恒为 1，报告记录窗外修改采样数为 0。每个目标通道输出修改采样数、最大修改幅度、估计噪声本底、解耦前后 SNR 与提升量。

### 母带、限幅与折叠

- 各轨按 `mix_weight` 与采样级左右增益叠加，所有处理保持零采样延迟。
- ITU-R BS.1770 K-weighting 用 48 kHz 两级 biquad 冲激响应和分块 FFT 卷积实现；积分响度按 400 ms 窗、100 ms 步进、绝对与相对门限计算。
- 真峰值采用 4 倍窗化 sinc 过采样测量。
- 超限时使用立体声联动、零净延迟、带短前瞻和释放保护的离线限幅器；处理前后总帧数不变，报告记录触发采样数、前后真峰值与延迟补偿量。
- 单声道按 `mono.fold_weights` 从最终立体声母带折叠。
- Welch 中频能量统计分别核对整条母带和“虫鸣+鸟鸣”场景子混音在立体声/单声道间的一致度。

## 快照、Ctrl+C 与恢复

每块渲染完成后：

1. 立体声与单声道块先写临时文件、`fsync`，再原子替换到已提交块路径。
2. 快照 JSON 以临时文件 + `os.replace` 整体提交。
3. 快照包含已完成块数、总块数、原始清单 SHA-256、规范化清单摘要、解耦参数、解耦参数摘要、每块帧数和 SHA-256。

Ctrl+C 处理器只设置停止标志；当前块完成并提交后写出 `recovery_report.json`，退出码为 `0`。强杀不保证恢复报告，重启会从最后一个已提交快照继续。

启动时若清单内容摘要或当前重新计算的解耦参数与快照不一致，会删除旧快照和块数据，输出 `E_RESUME_MISMATCH` 并退出 `3`。

可用以下命令比较续算结果与一次性结果：

```bash
python3 rainforest_master.py verify \
  --reference out/oneshot --candidate out/resumed --block-frames 48000
```

差异报告包含 `block`、`channel`（`left`/`right`/`mono`/`report`）和 `difference_type`（`byte_mismatch`、`length_mismatch`、`format_mismatch`、`missing_file`），非零退出码为 `4`。

## 退出码

| 退出码 | 含义 |
|---:|---|
| `0` | 渲染成功、输出一致性校验成功，或 Ctrl+C 已提交恢复快照 |
| `2` | 清单/素材校验失败，`STRUCTURED_ERROR` |
| `3` | 快照与当前清单或解耦参数不一致，`E_RESUME_MISMATCH` |
| `4` | 输出存在结构化差异，`E_RENDER_DIFFERENCE` |
| `5` | 渲染完成但业务合规检查为 FAIL |
| `1` | 未预期内部错误 |

## 实际验证结果

当前工作区已执行 600 秒固定种子工程：

- 清单：`artifacts/rainforest_600s/project/manifest.json`
- 母带：`artifacts/rainforest_600s/master/`
- 帧数：立体声与单声道均为 `28,800,000`，48 kHz，32-bit float。
- 全部 9 项报告检查：`PASS`。
- 接缝样本差：`0.0`；接缝短时能量跳变 `7.999e-05`，允许上限为同长度中段最大跳变的 2 倍，即 `0.066745`。
- BS.1770 积分响度：`-18.000000001 LUFS`，目标 `-18 LUFS`。
- 真峰值：`-4.1364 dBTP`，低于 `-1.0 dBTP` 上限；未触发限幅。
- 鸟鸣解耦：72/72 个瞬态被检测，修改 `1,175,040` 个雨幕采样，SNR 从 `-11.435 dB` 提升到 `-3.196 dB`，提升 `8.239 dB`。
- 声像最大相邻耳间变化：七轨均小于 `6.9e-07 dB`，阈值 `0.01 dB`。
- “虫鸣+鸟鸣”中频能量占比的立体声/单声道偏差：`0.000553 dB`，容差 `1.0 dB`。
- 独立 FFmpeg `ebur128=peak=true` 复核：`-18.0 LUFS`、真峰值 `-4.1 dBFS`。
- 一次性渲染耗时约 `69.6 s`，峰值常驻内存约 `2.18 GB`。
- 600 秒续算实测：在第 `137/600` 块提交快照后重启续算，`verify` 返回 `OUTPUTS_IDENTICAL`；立体声 SHA-256 为 `16a844d8...f3f11e`，单声道 SHA-256 为 `6b43a7bb...ed6f74d`，统计报告也逐字节一致。

自动化测试 `python3 -m unittest discover -s tests -v`：8/8 通过，覆盖确定性字节比较、清单错误码、接缝、声像、掩蔽、窗外零修改、响度/真峰值、单声道折叠、快照续算逐字节一致、摘要不匹配退出 `3`、SIGINT 退 `0` 及差异报告退出 `4`。

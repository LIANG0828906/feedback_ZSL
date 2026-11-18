# UID211 多语种离线母带封装组件

从分轨 WAV 生成英、日、德三条可交付母带和一份可复核的混音报告。实现只依赖 Python
3.10+ 与 NumPy 1.x，不使用网络、容器、外部音频或音频服务。所有演示素材由
`mastering.synth` 确定性合成。

## 快速开始

```bash
python -m pip install -e .

# 合成 6 秒演示工程并完成三条母带
python -m mastering demo --workdir .demo

# 只生成演示工程，不执行母带
python -m mastering synth --workdir .demo
python -m mastering run --project .demo/project.json

# 从最终 WAV 复算报告中的响度/真峰值、头信息、帧数和哈希
python -m mastering verify --report .demo/output/mix_report.json
```

成功后输出目录包含：

```text
.demo/output/
  master_en.wav
  master_ja.wav
  master_de.wav
  mix_report.json
  recovery_report.json
```

正常完成和 `Ctrl+C` 可处理中断的退出码均为 `0`。非法工程或运行失败使用非零退出码，
并向 stdout 输出 JSON 错误对象。

## 工程与规则格式

### `project.json`

`tracks` 可以是对象（推荐）或数组。路径相对工程文件所在目录解析。所有输入 WAV
必须与工程的全局 `sample_rate`、`channels`、`bit_depth` 一致。

```json
{
  "project_version": 1,
  "sample_rate": 48000,
  "channels": 2,
  "bit_depth": 24,
  "block_frames": 4800,
  "required_languages": ["en", "ja", "de"],
  "rules_path": "rules.json",
  "output_dir": "output",
  "tracks": {
    "voice_en": {
      "role": "voice",
      "language": "en",
      "path": "input/voice_en.wav"
    },
    "voice_ja": {
      "role": "voice",
      "language": "ja",
      "path": "input/voice_ja.wav"
    },
    "voice_de": {
      "role": "voice",
      "language": "de",
      "path": "input/voice_de.wav"
    },
    "music": {"role": "music", "path": "input/music.wav"},
    "sfx": {"role": "sfx", "path": "input/sfx.wav"}
  }
}
```

`role` 也可写作 `kind`，语音轨支持 `voice`、`voiceover`、`dialogue`、`dialog`。
语言标识只允许字母、数字、`_`、`-`，避免输出文件名注入。

### `rules.json`

```json
{
  "schema_version": 1,
  "languages": {
    "en": {"target_lufs": -23.0, "true_peak_dbtp": -1.0},
    "ja": {"target_lufs": -24.0, "true_peak_dbtp": -1.0},
    "de": {"target_lufs": -23.5, "true_peak_dbtp": -1.0}
  },
  "buses": {
    "music_target_lufs": -34.0,
    "sfx_target_lufs": -39.0,
    "music_gain_db": 0.0,
    "sfx_gain_db": 0.0
  },
  "ducking": {
    "depth_db": 12.0,
    "threshold_dbfs": -34.0,
    "attack_ms": 18.0,
    "release_ms": 260.0,
    "frame_ms": 10.0
  },
  "limiter": {
    "true_peak_dbtp": -1.0,
    "lookahead_ms": 5.0,
    "release_ms": 90.0,
    "oversample": 4,
    "safety_margin_db": 0.08
  },
  "silence": {
    "threshold_dbfs": -60.0,
    "min_active_ratio": 0.01,
    "frame_ms": 20.0
  },
  "duration": {"trim_strategy": "trailing_silence"}
}
```

`languages` 中的真峰值是每个语种母带的上限；未声明时使用 `limiter.true_peak_dbtp`，
该值缺省为 `-1.0`。`buses.*_target_lufs` 可选，存在时先把音乐/音效各自对齐到目标
响度，再叠加规则增益。工程轨道上的 `target_lufs` / `true_peak_dbtp` 可覆盖规则值。

## 业务校验与错误码

校验在创建任何母带前完成；所有发现的问题聚合到一个 JSON 对象：

```json
{
  "status": "error",
  "errors": [
    {
      "code": "SAMPLE_RATE_MISMATCH",
      "track": "voice_en",
      "message": "WAV declares 44100 Hz; project requires 48000 Hz"
    }
  ]
}
```

主要业务码：

| 业务码 | 含义 |
| --- | --- |
| `TRACK_MISSING` | 轨道文件或必需角色不存在 |
| `INVALID_WAV` | RIFF/WAVE 结构、fmt/data 或样本格式无效 |
| `SAMPLE_RATE_MISMATCH` | WAV 头采样率与工程不符 |
| `CHANNEL_COUNT_MISMATCH` | WAV 头声道数与工程不符 |
| `BIT_DEPTH_MISMATCH` | WAV 头位深与工程不符 |
| `DUPLICATE_LANGUAGE` | 两条语音轨使用同一语种 |
| `MISSING_LANGUAGE` | `required_languages` 中的语种没有语音轨 |
| `TRIM_WOULD_REMOVE_AUDIO` | 语音长于音乐，且尾部静音不足，不能安全裁剪 |
| `SILENT_TRACK` | 告警而非错误；静音语音轨跳过增益调整 |

其他工程结构错误使用 `PROJECT_INVALID`、`PROJECT_MISSING`、
`LANGUAGE_RULE_MISSING` 等稳定代码。错误对象不含绝对路径。

## 处理链路

### 时长适配

音乐轨定义母带总帧数。每条语音轨和音效轨：

- 短于音乐轨：尾部补零到相同帧数；
- 长于音乐轨：只删除尾部低于 `silence.threshold_dbfs` 的静音；
- 所需裁剪量大于可用尾部静音：拒绝整个工程，不输出母带。

报告中记录原始/适配后帧数及补齐、裁剪的采样数。

### BS.1770 响度

使用 ITU-R BS.1770 的 K 加权双二阶滤波器、400 ms 块、100 ms 步长（75% 重叠），
并依次应用 `-70 LUFS` 绝对门限和相对门限（绝对门限结果减 10 LU）。对每个语音轨：

- 测量适配后整体响度；
- 按语种 `target_lufs` 计算增益；
- 若 `min_active_ratio` 判定为静音，写入 `SILENT_TRACK` 告警、增益固定为 `0 dB`，
  不放大静音段。

### 对白闪避

以 `frame_ms` 帧能量判断对白是否超过 `threshold_dbfs`。音乐与音效先相加为总线，
再共用一条平滑衰减曲线：

- 攻击与释放均使用一阶平滑；
- 攻击端提前一个攻击时间观察语音能量，避免语音起始处的阶跃；
- 最大衰减由 `depth_db` 限定，默认应用到 `music+sfx` 总线。

报告给出每个语种的总线闪避采样数、秒数、最大衰减量以及音乐总线对应值。

### 真峰值限制

1. 使用 4 倍（可配置 1/2/4/8）截断多相 sinc 过采样估计真峰值。
2. 计算逐帧所需增益，向前看 `lookahead_ms`，再做平滑攻击与释放。
3. 通过内部前视路径产生增益，并补偿前视延迟，因此母带总帧数等于未限幅混音帧数。
4. 量化到目标位深后再次从样本复核真峰值；必要时做确定性的小幅节目增益修正。
5. 最终文件再次由 `verify` 从 WAV 复算，确保不超过规则上限。

限幅器不会硬削波：衰减从语音/瞬态到来前开始，释放为指数平滑。

### 节目响度

在语音轨达到目标后完成音乐/音效叠加和闪避，再对每个语种做节目响度闭环，
使最终母带达到该语种目标（容差 `±0.5 LU`）。量化后的母带还要再次复核响度与
真峰值；不达标时以业务错误退出，不交付伪达标文件。

## 输出报告

`mix_report.json` 字段顺序固定，无时间戳和绝对路径，包含：

- 工程摘要、采样率、声道数、位深、总帧数和总秒数；
- 每条语音轨的原始/适配时长、裁剪/补齐采样数、调整前后响度、目标响度、增益、
  静音告警；
- 各语种闪避参数、闪避采样数/秒数、最大衰减量；
- 各语种最终母带整体响度、真峰值、真峰值上限、限幅触发采样数、最大衰减量、
  前视与延迟补偿采样数；
- 三个 WAV 的相对文件名、字节数和 SHA-256。

`recovery_report.json` 记录 `status`、恢复次数、丢弃的快照版本、已完成块数、
总块数、总帧数和已完成输出文件名。

## 快照、Ctrl+C 与强制终止

工作目录默认为 `<output>/.mastering-state`。快照协议：

- 固定 `block_frames` 写入三条工作 WAV；
- 当前块的三条 WAV 数据均 `flush + fsync` 后，才原子提交新版本快照；
- 快照包含工程摘要、块偏移、分析确定的每语种增益/闪避参数、限幅统计、文件长度
  和工作 WAV 头 SHA-256；
- 快照自身有 SHA-256 完整性字段，采用临时文件 + `os.replace` 原子更新。

恢复时：

- 用工程 JSON、规则内容和所有输入 WAV 的 SHA-256 生成工程摘要；
- 摘要不一致、快照 JSON/完整性损坏、工作文件变短或 WAV 头不符时，删除整个状态目录，
  从第 0 块重新分析，不沿用旧偏移或旧增益；
- 工作文件有多余未提交尾字节时，自动截断到最后一次已提交长度；
- 成功交付后自动清理状态目录，不需要人工清理临时文件。

真实 `Ctrl+C` 会设置中断标志，当前块完成并提交快照后写 `recovery_report.json`
（`status: "interrupted"`），以 `0` 退出。强制终止不保证恢复报告；下一次启动只依据
最后已提交快照继续，清理并截断可能存在的半块数据。

测试提供了两个仅用于确定性注入的环境变量：

```text
MASTERING_SIMULATE_SIGINT_AFTER_BLOCKS=N
MASTERING_SIMULATE_SIGKILL_AFTER_BLOCKS=N
```

它们分别模拟可处理中断和不可处理强制终止，不改变正式处理路径。

## 测试

```bash
python -m unittest discover -s tests -v
python -m mastering verify --report .demo/output/mix_report.json
```

测试覆盖：

- BS.1770 响度参考值、过采样真峰值、限幅平滑、闪避曲线和尾部静音裁剪；
- 三语种母带响度/真峰值、WAV 头自洽、报告哈希与独立复算；
- 同一输入重复运行逐字节一致；
- 强制终止后恢复、Ctrl+C 中断、快照损坏、工程摘要失配；
- 静音轨告警与不放大；
- 文件缺失、采样率/位深/声道不符、重复/缺失语种等业务码。

实际测试结果见 `VERIFICATION.md`。

## 代码结构

```text
mastering/
  cli.py        CLI、信号处理、报告复核
  engine.py     校验编排、分析、混音、分块提交与交付
  dsp.py        时长适配、闪避、过采样真峰值、前视限幅
  loudness.py   BS.1770 K 加权与门限
  project.py    工程/规则解析
  state.py      版本化快照、完整性校验、恢复
  synth.py      确定性演示素材
  wavio.py      RIFF/WAVE 校验、PCM/float 读取、PCM 输出
tests/
  test_mastering.py
```

## 支持范围

- 输入 WAV：PCM 8/16/24/32-bit 与 IEEE float 32/64-bit；
- 输出 WAV：PCM 16/24/32-bit；
- 声道：1–8；演示与验收使用立体声；
- 不包含响度元数据写回（如 BWF/ADM/iXML），平台响度通过音频样本本身与报告证明。

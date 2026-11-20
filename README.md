# 多语种游戏配音本地修复工作台

这是一个无网络依赖的本地工具，覆盖 16-bit PCM WAV 读取、v1/v2/v3 修复配方升级、逐片段修复、跨版本一致性比对、原子导出、并发互斥和强制终止恢复。核心处理链不依赖 GUI，PyQt5 只负责桌面呈现。

## 快速开始

Windows:

```powershell
py -3.12 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -U pip
python -m pip install -r requirements.txt
python -m game_audio_restorer.gui
```

Linux/WSL:

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m game_audio_restorer.gui
```

本次实现的实际验证环境为 Linux WSL2（Python 3.12.16），Windows 兼容路径使用 `os.replace`、`msvcrt.locking` 和同目录临时文件；代码未使用容器。

## 命令

```bash
# 生成 8k/48k、单/双声道、v1/v2/v3 的确定性样本与基线
.venv/bin/python -m game_audio_restorer generate \
  --out artifacts/acceptance --quick

# 完整 8k/16k/44.1k/48k × 单/双声道网格
.venv/bin/python -m game_audio_restorer generate \
  --out artifacts/acceptance_full

# 执行修复，存在有效快照时自动续跑
.venv/bin/python -m game_audio_restorer run \
  --wav artifacts/acceptance/samples/sr48000_ch2.wav \
  --recipe artifacts/acceptance/recipes/v3.json \
  --work artifacts/acceptance/manual-v3

# 显式恢复
.venv/bin/python -m game_audio_restorer resume \
  --wav artifacts/acceptance/samples/sr48000_ch2.wav \
  --recipe artifacts/acceptance/recipes/v3.json \
  --work artifacts/acceptance/manual-v3

# 从完整快照重新校验并原子导出
.venv/bin/python -m game_audio_restorer export \
  --wav artifacts/acceptance/samples/sr48000_ch2.wav \
  --recipe artifacts/acceptance/recipes/v3.json \
  --work artifacts/acceptance/manual-v3

# 三版逐片段比对
.venv/bin/python -m game_audio_restorer compare \
  --wav artifacts/acceptance/samples/sr48000_ch2.wav \
  --recipe-v1 artifacts/acceptance/recipes/v1.json \
  --recipe-v2 artifacts/acceptance/recipes/v2.json \
  --recipe-v3 artifacts/acceptance/recipes/v3.json \
  --work artifacts/acceptance/manual-compare

# GUI
.venv/bin/python -m game_audio_restorer.gui
```

所有 CLI 结果输出为 JSON。业务错误退出码为 `2`，协作式取消为 `3`。

## 工程结构

```text
game_audio_restorer/
  wav_io.py      严格 RIFF/PCM 解析、内部 float64 表示、原子写
  recipes.py     v1/v2/v3 校验、统一表示、升级来源与丢失字段
  processing.py  声道映射、DC、门限、谱减、增益包络、限幅
  metrics.py     K 加权积分响度、峰值、RMS、残留噪声
  snapshot.py    快照版本、逐片段哈希链、断链定位
  locks.py       跨平台同源任务非阻塞文件锁
  restore.py     检查点执行、取消、恢复、原子 WAV/JSON/JSONL 导出
  compare.py     三版共同字段、逐片段指标、容差、可读归因
  samples.py     确定性样本、配方、基线与轨迹生成
  gui.py         PyQt5 波形、片段网格、参数编辑、取消、不一致标记
scripts/acceptance.py
tests/
artifacts/acceptance/
```

## WAV 契约

接受：

- RIFF/WAVE，PCM format 1
- 16-bit little-endian
- 单声道或双声道
- 8000–48000 Hz
- RIFF 长度、`fmt`、`byte_rate`、`block_align`、`data` 长度与文件字节一致

内部表示是 `float64[frames, channels]`，按 `int16 / 32768.0` 归一。未知 RIFF chunk 可跳过，但 chunk 边界和对齐填充必须完整。

所有格式错误返回 `E_WAV_INVALID`，并包含 `location.file`、`location.byte_offset`、`location.field`。配置错误、未知版本和越界分别返回 `E_RECIPE_INVALID`。

## 配方版本语义

v1:

```json
{
  "version": 1,
  "denoise_strength": 0.55,
  "gain_db": 2.0
}
```

v2 增加 `remove_dc`、`silence_threshold_dbfs`。v3 再增加：

```json
{
  "channel_merge_mode": "preserve",
  "spectral_subtraction": {
    "segment_ms": 32,
    "oversubtraction": 1.8,
    "spectral_floor": 0.015
  },
  "loudness_target_lufs": -20.0
}
```

旧版升级保持旧版可表达语义：

| 字段 | v1 升级值 | v2 升级值 |
|---|---:|---:|
| `remove_dc` | `false` | 配方提供 |
| `silence_threshold_dbfs` | `null`（disabled） | 配方提供 |
| `channel_merge_mode` | `preserve` | `preserve` |
| `segment_ms` | `32` | `32` |
| `oversubtraction` | `1.5` | `1.5` |
| `spectral_floor` | `0.02` | `0.02` |
| `loudness_target_lufs` | `null`（disabled） | `null`（disabled） |

每份统一配方都带 `field_mapping`，记录字段值、提供版本、来源（`provided` / `upgrade_default` / `disabled_default` / `compatibility_default`）和影响。旧版源无法表达的字段同时进入 `lost_fields`，包含首次支持版本、影响范围和可读说明。

## 处理链

每个 1 秒逻辑片段独立执行：

1. `channel_merge_mode`：`preserve` / `mono_mix` / `left` / `right`
2. 去直流：仅启用时按声道独立统计并减去各声道均值
3. 区间静音门限：20 ms 能量窗、5 ms 平滑淡出淡入
4. 分段谱减：Hann STFT、20% 噪声分位、过减系数和谱底；左右声道使用完全相同的窗长、帧边界和 hop
5. 增益包络：固定增益、v3 响度修正、5 ms raised-cosine 边缘斜坡
6. 峰值限幅：上限 `-0.18 dBFS`，整片段统一缩放

每阶段参数、FFT 长度、hop、实际 alpha、实际响度修正、限幅增益、DC 各声道均值和声道布局都写入片段参数，可直接 JSON 序列化。

处理先得到完整多声道片段再编码落盘，任一阶段异常会中止整片段；不会出现只写左声道或半写 WAV。默认在源采样率原生处理，时间窗按采样率换算为样本数，不做隐式重采样。

## 跨版本一致性

对同一 WAV 依次运行三版配方，报告逐片段给出：

- `v1_v2`、`v1_v3`、`v2_v3` 三组比较
- 共同字段：`denoise_strength`、`gain_db`、`silence_threshold_dbfs`
- 指标差：残留噪声能量、峰值、K 加权积分响度、片段增益跳变、波形 RMS 差
- 每片段 `incompatible` 标记
- 可读归因：共同字段变化、v2/v3 新增语义、声道布局、响度目标或无法归因

默认容差在报告 `tolerances` 中显式声明。输出既含逐片段明细，也含各比较对的不兼容片段计数；不会只给总数。三版共用源采样率，报告明确说明采样率重采样不是本比较中的差异来源。

## 原子性与恢复

恢复目录结构：

```text
snapshot.json
segments/000000.wav
segments/000001.wav
...
restored.wav
report.json
trace.jsonl
```

每个片段先写同目录临时文件，`fsync` 后由 `os.replace` 原子替换片段文件；随后原子替换 `snapshot.json`。清单保存快照版本、源 WAV 哈希、配方哈希与版本、片段边界、参数、指标、内容哈希、前驱链哈希和当前链哈希。

恢复时：

1. 验证快照版本和必填字段
2. 验证源 WAV 哈希、配方哈希和分段尺寸
3. 逐片段验证文件存在、内容 SHA-256、边界连续和前驱/当前哈希链
4. 从最后已提交片段之后继续；已提交片段不重写
5. 首个断链、缺失字段或内容不符以 `E_RECOVERY_INVALID` 中止，并给出 `first_break_segment`

同一 WAV 由操作系统文件锁互斥。重复启动返回 `E_BUSY`。进程被强制终止时锁随进程释放，磁盘上最后一次原子提交仍可恢复；强制终止不会承诺生成退出报告。

最终导出前再次验证当前配方版本/哈希与片段哈希清单。不一致返回 `E_VERSION_STALE`。WAV、`report.json` 和 `trace.jsonl` 都用同目录临时文件加原子重命名写出。

## 界面

PyQt5 工作台支持：

- 载入并显示 WAV 波形与逻辑片段边界
- 查看采样率、声道数、帧数、时长和源哈希
- 切换并编辑 v1/v2/v3 参数
- 查看字段映射和旧版丢失字段影响
- 启动修复、协作式取消、片段网格实时显示指标
- 跨版本比对后在片段行显示不兼容比较对，并用悬浮文本展示归因

恢复目录默认按 WAV 和配方版本分开，避免切换版本后误用旧快照。

## 确定性样本与轨迹

`generate` 默认生成 8k/16k/44.1k/48k × 单/双声道；`--quick` 生成 8k/48k × 单/双声道。每个样本运行 v1/v2/v3，并生成：

- `samples/*.wav`：确定性源语音
- `recipes/v1.json`、`v2.json`、`v3.json`
- `runs/<sample>/v*/restored.wav`
- `runs/<sample>/v*/report.json`
- `runs/<sample>/v*/trace.jsonl`
- `runs/<sample>/comparison.json`
- 根目录 `trace.jsonl`：每行包含样本、片段号、配方版本、字段映射、指标、内容哈希和链哈希
- `baseline.json`：全部版本逐片段指标与内容哈希

生成器不写时间戳或随机绝对路径到轨迹；相同种子、参数和操作序列下，源 WAV、修复 WAV、轨迹和基线逐字节一致。`tests/test_samples.py` 会在两个不同目录生成并逐文件比较。

## 实际验证结果

2026-11-21 在本工作区运行：

```bash
.venv/bin/python -m pytest -q
# 45 passed

.venv/bin/python scripts/acceptance.py --out artifacts/acceptance
```

验收产物：

- `artifacts/acceptance/verification.json`：7 项检查全部为 `true`
- `artifacts/acceptance/pytest.log`：本次 45 项测试的实际输出
- `artifacts/acceptance/trace.jsonl`：24 条片段轨迹加 1 条头
- `artifacts/acceptance/baseline.json`：4 个样本 × 3 版本基线
- `trace_sha256`: `1e168716c13d5b6f3ed952674d9ce0fb8e471e0b9a10d65bc9dd437c6750dfa0`
- `baseline_hash`: `4b641bc9ad05c715115aeaca3b7154aab672d55c8ec2ca5e652310a2afe4846d`

强制终止验证使用 48 kHz 双声道样本和 v2：

- 子进程提交第 1 个片段后被 `SIGTERM` 强制终止，退出码 `-15`
- 终止前 `report.json` 不存在
- 恢复后完成 2/2 片段
- 第 1 个片段字节未变化
- 最终链头 `82a925be74536903a1bac40e69948f1ab6f6028b12c105bb42d82bb5eda075d4`

自动化测试覆盖 WAV 头/长度/声道/采样率/位深错误定位、三版配方缺失和越界、旧版升级、DC 独立统计、立体声谱减对齐、失败不落盘、取消后续跑不重写、断链定位、版本过期、并发 `E_BUSY`、真实子进程强杀恢复、跨版本逐片段归因、生成器字节确定性和 PyQt5 离屏冒烟。

## 边界说明

- 本次实际运行平台是 WSL2/Linux；Windows 专用 `msvcrt.locking` 分支已实现但未在真实 Windows 主机执行。
- 响度使用 BS.1770 形式的 K 加权、400 ms 分块与相对门限实现，静音钳制到 `-120 LUFS`。
- 每个逻辑片段独立做 DC、谱减和 v3 响度归一，因此相邻片段可能产生增益跳变；比对报告会把该跳变作为指标显式列出，而不是隐藏。
- 不访问网络、真实账号或外部音频资源。

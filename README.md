# 离线童声合唱母带渲染器

从零生成八轨确定性合唱素材，完成男高换声点检测、频带音色补偿、四声部能量平衡、BS.1770 积分响度与真峰值合规检查，并输出可逐字节复算的立体声 WAV、稳定 JSON 报告和可强杀恢复的分块快照。

整个流程不需要外部媒体或网络。输入样例由整数种子确定性合成。

## 环境与安装

- Python 3.10+
- numpy、scipy、soundfile
- Windows 10/11 x64、Linux/macOS 均可运行；自动化验证在当前本地 Linux 环境完成
- 输出固定为 48 kHz、16/24-bit PCM、双声道 WAV

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
.\.venv\Scripts\python -m choir_master --version
```

安装为命令（可选）：

```powershell
.\.venv\Scripts\python -m pip install -e ".[dev]"
choir-master --version
```

## 快速开始

生成确定性工程与八路 WAV：

```powershell
.\.venv\Scripts\python -m choir_master init `
  --workspace demo --seed 235 --duration 8 --block-frames 24000
```

校验：

```powershell
.\.venv\Scripts\python -m choir_master validate --manifest demo\project.json
```

一次性渲染：

```powershell
.\.venv\Scripts\python -m choir_master render `
  --manifest demo\project.json `
  --output demo\master.wav `
  --report demo\report.json `
  --snapshot demo\master.snapshot `
  --block-frames 24000 `
  --fresh
```

也可以直接生成并渲染：

```powershell
.\.venv\Scripts\python -m choir_master demo --workspace demo --seed 235
```

主要 CLI：

| 命令 | 作用 |
|---|---|
| `init` | 创建 `project.json` 和八个 WAV |
| `generate` | 按现有清单重新生成八路素材 |
| `validate` | 校验清单与 WAV，不渲染 |
| `prepare` | 执行分析、补偿、平衡和响度检查，仅打印摘要 |
| `render` | 分块渲染、提交快照、输出母带与报告 |
| `demo` | `init` 后立即 `render` |

## 工程清单

路径相对于清单文件所在目录。核心 schema 如下：

```json
{
  "schema_version": 1,
  "project": "deterministic_choir_master",
  "generator_version": "1.0",
  "seed": 235,
  "sample_rate": 48000,
  "bit_depth": 24,
  "duration_seconds": 8.0,
  "block_frames": 24000,
  "target_loudness_lufs": -20.0,
  "true_peak_limit_dbtp": -1.0,
  "voices": [
    {
      "name": "女高",
      "target_energy_ratio": 0.27,
      "breakpoint_band_hz": [500.0, 2500.0],
      "pan": -0.35,
      "synthesis": {
        "f0_hz": 523.25,
        "harmonics": 12,
        "formant_hz": 900.0,
        "level": 0.16
      }
    }
  ],
  "microphones": [
    {
      "name": "sop_a",
      "voice": "女高",
      "path": "audio/sop_a.wav",
      "channels": 1,
      "downmix_weight": 0.5,
      "position": {
        "delay_ms": 0.0,
        "gain_db": 0.0
      }
    }
  ],
  "generation": {
    "mutation_voice": "男高",
    "mutation_intervals_s": [[2.84, 3.64]],
    "mutation_band_hz": [400.0, 1600.0],
    "mutation_gain_db": -18.0
  }
}
```

约束：

- 四个声部必须齐全，名称为 `女高`、`女低`、`男高`、`男低`；为兼容输入，也接受大小写不敏感的 `soprano/alto/tenor/bass`。
- 每个声部恰好两路麦克风，总计八路。
- 四声部 `target_energy_ratio` 之和与 1 的偏差不得超过 `1e-6`。
- `breakpoint_band_hz` 必须是 `[low, high]`，且 `low < high`、位于 Nyquist 以下。
- `sample_rate` 必须为 48000；实际 WAV 的采样率、位深、声道数必须与清单一致。
- `position.delay_ms/gain_db` 用于生成期麦克风摆位；渲染读取已生成的 WAV，不会重复添加延迟。
- `generation` 是生成参数的可复算描述。检测器不会读取其中的真值区间，真值仅用于测试核对。
- 输出位深取自清单，支持 16 或 24 bit。

## 结构化错误

错误以 JSON 写到 `stderr`，包含业务码和麦克风名：

```json
{
  "status": "error",
  "errors": [
    {
      "code": "E_SAMPLE_RATE_MISMATCH",
      "mic": "ten_a",
      "message": "actual WAV sample rate does not match the manifest",
      "voice": "男高",
      "expected": 48000,
      "actual": 44100
    }
  ]
}
```

退出码：

| 退出码 | 含义 |
|---|---|
| `0` | 成功，或 Ctrl+C 已完成当前块并写出恢复报告 |
| `2` | 清单/音频校验失败 |
| `3` | `E_RESUME_MISMATCH`，旧进度已整体丢弃 |
| `4` | 处理、响度、真峰值或声部平衡无法满足约束 |

常用业务码：

| 业务码 | 含义 |
|---|---|
| `E_MANIFEST_MISSING` / `E_MANIFEST_JSON` | 清单缺失或非法 JSON |
| `E_FILE_MISSING` | 声明的 WAV 不存在 |
| `E_SAMPLE_RATE_MISMATCH` | 实际采样率不符 |
| `E_BIT_DEPTH_MISMATCH` | 实际位深不符 |
| `E_CHANNELS_MISMATCH` | 实际声道数不符 |
| `E_VOICE_INVALID` / `E_VOICE_MISSING` | 声部非法或缺失 |
| `E_RATIO_SUM` | 目标能量比例和偏离 1 超过 `1e-6` |
| `E_BAND_INVERTED` | 换声点频带上下限倒置 |
| `E_RESUME_MISMATCH` | 清单摘要、音频摘要或已确认分析结果与快照不一致 |
| `E_LOUDNESS_TARGET` / `E_TRUE_PEAK_LIMIT` | 极限目标在真峰值约束下不可达 |
| `E_VOICE_BALANCE` | 无法满足 0.02 声部比例容差 |

## 分块、快照与恢复

渲染分块提交：

- 已完成块以原始交错 PCM 写入 `snapshot/blocks/block-XXXXXXXX.pcm`。
- 每块先以临时文件写完、`fsync`，再原子改名。
- 随后原子更新 `snapshot/state.json`，其中记录完成块数、清单/音频 SHA-256、换声点区间、声部校正系数、块哈希和恢复次数。
- 默认检测到已有快照就续跑；只有显式 `--fresh` 才丢弃并从头开始。

恢复语义：

- Ctrl+C（以及可用的 SIGTERM/SIGBREAK）：完成正在写的当前块并提交快照，写 `report.json` 与 `report.recovery.json`（`status: "interrupted"`），以退出码 `0` 结束。
- 强制终止：不保证任何退出报告；重新执行相同命令（不要加 `--fresh`）即可从最后提交块继续。
- 清单摘要、音频摘要、块大小或已确认检测/增益结果变化：删除快照、母带和旧报告，输出 `E_RESUME_MISMATCH`，退出码 `3`。
- 续跑与一次性完整渲染的 WAV 逐字节一致。

`--block-pause-ms` 仅用于故障注入测试，在每块提交后暂停，不改变音频结果。

## 输出

母带：

- 48 kHz、双声道、16/24-bit PCM WAV
- 总帧数等于最长输入通道
- 块拼接不改变任何通道的时间关系

报告字段顺序固定，无时间戳和绝对路径，包含：

- 每个声部的换声点区间；未检测到时为空的稳定数组
- 每段补偿前后频带能量、基线能量、改善量、施加增益、最大修改幅度
- 各声部目标比例、实际占比、绝对偏差、校正系数与贡献百分比
- 积分响度、目标响度、偏差
- 4 倍过采样真峰值、样本峰值、真峰值上限
- 限幅触发样本数与限幅是否启用
- 完成块数、恢复次数、输入/输出 SHA-256 与合规检查摘要

## 测试

```powershell
.\.venv\Scripts\python -m pip install -e ".[dev]"
.\.venv\Scripts\python -m pytest -q
```

测试覆盖确定性生成、各类校验错误、检测/补偿、区间外不变性、声部平衡、响度/真峰值、16-bit 输出、重复渲染逐字节一致、SIGINT、SIGKILL 续跑以及两类 `E_RESUME_MISMATCH`。

实际验证数据见 [VERIFICATION.md](VERIFICATION.md)，算法细节见 [TECHNICAL.md](TECHNICAL.md)。

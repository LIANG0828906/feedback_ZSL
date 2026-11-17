# feedback_ZSL — 振动耦合反馈溯源、剔除与立体声母线渲染流水线

面向现场扩声后期场景：从确定性合成的多轨素材中自动溯源由舞台振动耦合
引入的窄带反馈（嗡鸣），剔除各通道中的反馈成分，输出可复核报告，并把
处理后的多通道下混为通过响度 / 真峰值检查的立体声母线。支持按固定采样
块渲染、崩溃快照恢复与 Ctrl+C 优雅中断。

## 环境

- Windows 10+ / Linux / macOS，Python 3.10+
- 依赖：`pip install -r requirements.txt`（NumPy、SciPy、soundfile；pytest 用于测试）
- 不需要任何外部媒体或网络；全部素材由任务代码确定性合成

## 快速开始

```bash
# 1. 生成确定性多轨工程（乐器式信号 + 注入的振动耦合反馈 + 场记参考轨）
python -m feedback_pipeline generate --out demo --seed 7 --segment 2

# 2. 溯源 -> 剔除 -> 报告 -> 渲染立体声母线（带块级快照）
python -m feedback_pipeline run --project demo/project.json --out demo/out
```

产物：

- `demo/out/bus.wav` — 立体声母线（24-bit PCM，采样数与最长通道一致）
- `demo/out/report.json` — 溯源结果、逐通道剔除质量、母线响度 / 真峰值 / 贡献占比
- `demo/out/work/` — 处理后的通道 WAV、块渲染快照（`snapshot.1/2.json`）、`bus.raw`
- `demo/ground_truth/` — 干净信号与注入清单（仅用于验收比对）

## 模块结构

| 模块 | 职责 |
| --- | --- |
| `feedback_pipeline/project.py` | 工程 JSON 加载与校验（文件缺失 / 采样率 / 声道数 / 命名 / 角色） |
| `feedback_pipeline/synth.py` | 确定性多轨合成；共振基频随 `song_segment` 变化 |
| `feedback_pipeline/detect.py` | 与参考轨比对，定位含窄带反馈的通道（基频 / 带宽 / 时间段 / 置信度） |
| `feedback_pipeline/remove.py` | 参考分量最小二乘对消，仅修改声明时间段内的采样 |
| `feedback_pipeline/loudness.py` | BS.1770 K 加权积分响度与 4x 过采样真峰值 |
| `feedback_pipeline/pipeline.py` | 编排：校验 → 溯源 → 剔除 → 报告 → 块渲染 |
| `feedback_pipeline/checkpoint.py` | 双代原子快照；损坏自动回退到最近一次完整提交 |
| `feedback_pipeline/errors.py` | 结构化业务错误码（JSON 输出，退出码 3） |

## 关键行为

- **整体拒绝**：工程校验失败（`SAMPLE_RATE_MISMATCH`、`CHANNEL_ROLE_MISSING`、
  `CHANNEL_FILE_MISSING`、`DUPLICATE_CHANNEL_NAME`、`CHANNEL_COUNT_MISMATCH`、
  `REFERENCE_MISSING` 等）时以结构化 JSON 错误结束，不输出任何部分母线。
- **溯源**：参考轨基频由抛物线插值谱峰估计（亚 Hz 精度）；通道判定要求
  窄带包络与参考包络相关 ≥ 0.65 且谱峰突出度 ≥ 8 dB。
- **剔除**：仅在溯源标记的时间段内做参考分量对消（段内 5 ms 升余弦交叉淡入），
  时间段外与未标记通道逐采样不变；采样率、采样数、位深保持不变。
- **母线**：下混权重和不为 1 时按工程声明的 `compensation`（`normalize`/`asis`）
  处理；积分响度归一到工程目标（容差 ±0.5 LU），真峰值不得超过声明上限，
  否则分别以 `BUS_LOUDNESS_OUT_OF_TOLERANCE` / `BUS_TRUE_PEAK_EXCEEDED` 拒绝。
- **中断与恢复**：Ctrl+C 完成当前块、提交快照、写 `recovery_report.json` 并以
  退出码 0 结束；强杀（SIGKILL）后重启仅凭已提交快照恢复，恢复渲染的母线与
  一次性完整渲染逐字节一致；工程摘要或溯源结果与当前输入不一致时丢弃旧进度、
  以 `E_RESUME_MISMATCH` 报出并从零开始；快照损坏自动回退到上一完整提交，
  双快照均损坏时无需人工清理直接从零开始。

## 测试与验收

```bash
python -m pytest tests/ -q
```

实测结果（本仓库环境，Python 3.14 + NumPy 2.5 + SciPy 1.18）：

- `17 passed in ~32s`，覆盖：生成逐字节确定性、曲目段改变共振基频、
  五类工程校验错误码、溯源通道集合与基频精度、剔除残余与未修改区域
  逐采样一致、母线响度 / 真峰值 / 长度、报告两次运行一致（除耗时字段）、
  强杀恢复逐字节一致、Ctrl+C 退出码 0、快照损坏回退、`E_RESUME_MISMATCH`。
- 演示工程（seed=7, segment=2）：溯源命中 `{gtr, vox}`（与注入清单一致，
  零误报），基频估计 240.0001 Hz（注入值 240 Hz，偏差 ≪ 1 Hz）；
  剔除后 240 Hz 处谱峰降至干净信号本底以下（gtr 27.9 dB vs 干净 30.5 dB），
  窄带能量衰减 27.5/27.1 dB（阈值 20 dB）；母线积分响度 -16.0 LUFS
  （目标 -16.0），真峰值 -5.82 dBTP（上限 -1.0）。

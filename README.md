# 舞台机械噪声定位与降噪工具包

确定性的多话筒舞台剧机械噪声（转台 / 升降台低频吱嘎声）溯源、压降、
BS.1770 立体声母带制作与断点续渲工具。所有素材由清单（场记）与整数种子
确定性合成，不依赖任何外部媒体或网络。

## 环境准备

- Python 3.10+（开发与验证使用 Python 3.14）
- 依赖：`numpy`、`soundfile`

```bash
python3 -m pip install -r requirements.txt
```

## 命令一览

```bash
# 1. 按清单确定性生成 6 支话筒 WAV + 注入真值 injection_truth.json
python3 -m stage_noise.cli generate configs/demo_manifest.json

# 2. 溯源 + 降噪 + 分块渲染 + 母带，产出 master_stereo.wav 与 noise_report.json
python3 -m stage_noise.cli render configs/demo_manifest.json

# 3. 仅校验清单与 WAV 头（采样率/位深/声道）
python3 -m stage_noise.cli validate configs/demo_manifest.json

# 4. 两份母带逐样本/逐字节比对
python3 -m stage_noise.cli compare reference.wav candidate.wav

# 一键端到端演示（含优雅中断 + 续渲逐字节核对）
bash scripts/run_demo.sh
```

## 模块结构

- `stage_noise/manifest.py`：清单加载与严格结构化校验
- `stage_noise/generator.py`：确定性素材合成与机械噪声注入
- `stage_noise/processing/tracing.py`：动作段内噪声溯源
- `stage_noise/processing/denoise.py`：带阻 FIR、分段交叉淡化、降噪指标
- `stage_noise/processing/mastering.py`：下混、BS.1770 响度、真峰值、限幅
- `stage_noise/persistence/snapshot.py`：分块内容寻址快照
- `stage_noise/pipeline.py`：端到端编排与中断/恢复语义
- `stage_noise/reporting/reports.py`：字段顺序稳定的 JSON 报告
- `tests/`：23 个单元/端到端/信号语义测试

## 关键设计与口径

### 确定性合成（要求 2）

- 每支话筒的随机流由 `SHA-256(清单种子 | 话筒名 | 话筒种子 | 用途)` 派生，
  独立可复算，重复生成逐字节一致。
- 对白式信号为 280–340 Hz 基频的三共振峰调幅音；伴奏式信号为 261 Hz 以上
  的和弦音；二者均落在机械参考频段（40–150 Hz）之外。
- 机械噪声为低频摆动基频（带内随机中心频率）+ 窄带滤波隆隆声，仅在清单
  动作段（含 150 ms 注入余量）内、按 `position_gains` 注入；真值写入
  `injection_truth.json`，含起止采样、中心频率与增益。

### 溯源（要求 3）

- Hann 窗 STFT（N=4096，hop=1024），以动作段外参考帧的带内功率中位数
  作为底噪地板。
- 段级门限：帧信噪比 ≥ 6 dB 的帧占比 ≥ 0.4 才判定存在噪声，抑制自然频谱
  抖动造成的误报；中心频率用带内频谱峰值 + 抛物线插值估计。
- 输出每话筒每动作段的起止时刻、中心频率、估计信噪比；时间边界对齐注入
  余量。实测中心频率误差 ≤ 1.4 Hz（容差 5 Hz），误报 0（允许上限 1）。

### 压降（要求 4、5）

- 4097 阶 Blackman 窗带阻 FIR（低通 + 高通并联），按白噪声单位功率归一化，
  通带（≥200 Hz）起伏约 0.03 dB，带内深度衰减、带边约 40 dB。
- 仅对溯源命中的动作段（含余量）做 30 ms 等功率交叉淡化；段外与未标记
  话筒样本完全不触碰（测试逐样本断言为零修改）。
- 实测带内衰减 24–51 dB（要求 ≥18 dB），对白/伴奏内容能量损失 ≤0.04 dB
  （要求 ≤1 dB），处理后采样数不变。
- 报告中的所有能量值用与处理相同的物理均方口径，可直接由落盘 WAV 复算。

### 母带（要求 6）

- 按下混权重合成立体声，长度取最长话筒轨，短轨补零，时间关系不变。
- BS.1770-4 K 加权（高架 + 高通零相位 FFT 实现）、400 ms/100 ms 块、
  绝对 + 相对两级门限计算积分响度，归一化到目标（偏差 ≤0.01 LU）。
- 4 倍过采样 FFT 估真峰值；超限时启用 5 ms 前瞻限幅器（van Herk 滑动
  最小值 + 释放平滑），延迟补偿后总采样数不变，并迭代天花板确保任何样本
  真峰值不越上限。报告记录限幅样本数、最大衰减与最终响度。

### 分块快照与恢复（要求 7、8）

- 固定 `chunk_samples` 渲染，每块写 `npy`（SHA-256 校验）并原子提交
  `snapshot.json`，记录已完成块数、清单摘要（规范 JSON 哈希）、溯源摘要
  与降噪参数。
- 清单/溯源/参数摘要不一致：整体丢弃旧进度并以业务码
  `E_RESUME_MISMATCH`（退出码 30）结束。
- 快照 JSON 或分块损坏：整体丢弃并从头渲染，不沿用任何旧块偏移/参数。
- SIGINT：完成当前块、提交快照、写恢复报告、退出码 0；SIGKILL：不保证
  退出报告，重启仅凭已持久化块恢复。
- 下混按块独立计算，续渲与一次性渲染装配完全相同的浮点值，母带逐字节
  一致（`compare` 与 `cmp` 均验证）。

## 结构化错误码

校验失败输出含 `business_code`、`mic_name`、`details` 的 JSON 到 stderr，
并以稳定非零码退出，且不产生任何母带或报告：

| 业务码 | 退出码 | 触发条件 |
| --- | --- | --- |
| `FILE_MISSING` | 20 | WAV/清单缺失 |
| `FILE_UNREADABLE` | 21 | WAV 无法读取 |
| `SAMPLE_RATE_MISMATCH` | 22 | 声明/实际采样率不符 |
| `BIT_DEPTH_MISMATCH` | 23 | 位深/subtype 不符 |
| `CHANNEL_COUNT_MISMATCH` | 24 | 声道数不一致 |
| `DUPLICATE_MIC_NAME` | 25 | 话筒重名 |
| `INVALID_ACTION_SEGMENT` | 26 | 动作段乱序/倒置/越界 |
| `INVALID_FREQUENCY_BAND` | 27 | 参考频段非法或超奈奎斯特 |
| `INVALID_MANIFEST` | 28 | 其他清单结构错误 |
| `E_RESUME_MISMATCH` | 30 | 快照与当前输入不一致 |
| `SNAPSHOT_CORRUPT` | 31 | 快照/分块损坏（内部转重头） |

## 报告稳定性

`noise_report.json` 字段顺序固定、不含时间戳与绝对路径；唯一随运行变化的
`elapsed_seconds` 已在测试中剔除后比对，同一输入重复运行其余字段完全相等。

## 验证结果

```
python3 -m unittest discover -s tests
# Ran 23 tests in ~79s — OK
```

覆盖：10 个校验错误码、6 话筒溯源对真值、衰减/内容保护/段外零修改、
母带响度与真峰值、限幅器天花板、生成逐字节一致、优雅中断/SIGKILL 恢复、
损坏快照重头、报告可复算与稳定性。

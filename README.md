# Deterministic Offline Binaural Bus

本工作区实现一个离线双耳母线合成组件，用于把多路演员麦克风、现场扬声器返送参考和工程声像轨迹合成为可整集播放的双耳 WAV，并输出可逐通道、逐段落复核的 JSON 制作报告。

组件只依赖 Python 3.11+、NumPy 1.26+ 和 Python 标准库。WAV 读写由项目内的 RIFF/WAVE 层完成，不访问网络，不依赖外部 HRTF、媒体文件、Docker 或容器。

## 快速开始

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -e .
.\.venv\Scripts\binaural-bus generate --project examples\project.json --out examples\inputs
.\.venv\Scripts\binaural-bus validate --project examples\project.json --audio-dir examples\inputs
.\.venv\Scripts\binaural-bus synthesize --project examples\project.json --audio-dir examples\inputs --output examples\output
```

Linux/WSL：

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -e .
.venv/bin/binaural-bus generate --project examples/project.json --out examples/inputs
.venv/bin/binaural-bus validate --project examples/project.json --audio-dir examples/inputs
.venv/bin/binaural-bus synthesize --project examples/project.json --audio-dir examples/inputs --output examples/output
```

也可以不安装，直接设置 `PYTHONPATH=.` 后运行 `python -m binaural_bus.cli ...`。

命令与退出码：

- `generate`：按工程种子生成演员麦克风、扬声器返送、直达声诊断轨和 `generation_manifest.json`。
- `validate`：校验工程和所有 WAV 头，不产生母线。
- `synthesize`：串扰消除、双耳渲染、响度/真峰值处理、分块导出和快照。
- `0`：完整写出或可处理中断且已提交快照。
- `2`：工程/输入校验拒绝。
- `3`：处理后的业务验收（残余串扰、直接损失、响度/真峰值）不通过。

校验失败时，命令行向标准输出打印稳定结构，例如：

```json
{
  "status": "error",
  "code": "E_INPUT_VALIDATION",
  "errors": [
    {
      "code": "SAMPLE_RATE_MISMATCH",
      "channel": "actor_lin.wav",
      "expected": 48000,
      "actual": 44100
    }
  ]
}
```

此时不会创建 `binaural_master.wav` 或 `production_report.json`。

## 示例工程

`examples/project.json` 是完整可运行工程。关键字段如下：

- `seed`、`sample_rate`、`bit_depth`、`duration_seconds`：素材与导出基础参数。
- `actors[]`：演员 ID、单声道文件、基础方位/仰角、`timeline` 方位时间线。
- `speakers[]`：扬声器 ID、单声道文件、听者坐标系方位/仰角，以及 `feedback[]` 串扰注入参数。
- `crosstalk.reference_windows[]`：参考段。生成器在其中的演员直达声为 0；用于估计各扬声器在演员声道中的增益与时延。
- `crosstalk.direct_probe_windows[]`：直达声审计段。生成器使扬声器返送在这些段前静音，用于测量串扰消除造成的演员直达能量损失。
- `crosstalk.residual_energy_ratio_max`：参考段残余能量比上限。
- `crosstalk.max_direct_energy_loss_ratio`：直达能量损失比例上限。
- `spatial`：头半径、声速、控制刻度、平滑时间、ILD/ITD 速率上限。
- `export`：WAV 采样率、位深、双声道、目标 LUFS、真峰值上限和固定块帧数。

方位坐标约定：前方 `0°`，右方 `+90°`，左方 `-90°`；仰角向上为正。时间线对相邻点线性插值，再用工程平滑时间和 ILD/ITD 速率上限重新约束。

串扰注入在生成器中量化到整数采样时延：

```text
delay_samples = round(delay_ms * sample_rate / 1000)
gain_linear   = 10 ** (gain_db / 20)
actor_mic[n]  = actor_direct[n]
              + sum_s(gain_linear_s * speaker_s[n - delay_samples_s])
```

`generation_manifest.json` 记录每条路径的量化增益、时延、扬声器方位和工程摘要，便于从工程文件复算。

## 算法

### 1. 确定性素材

演员轨由固定种子决定：短促低通噪声爆破音、带颤音的持续谐波元音、交替静音段。扬声器返送是独立于演员直达声的确定性低通噪声与谐波节目声。参考段和直达审计段由工程时间窗直接施加掩蔽，不依赖文件系统枚举或未播种随机数。

### 2. 串扰估计与消除

对每个演员、每个扬声器：

1. 在参考段内做 FFT 互相关，搜索 `[0, max_delay_ms]` 内的整数采样时延。
2. 用全部参考段构造多扬声器回归矩阵，做联合最小二乘。
3. 对每个时延做坐标下降细化，再联合刷新增益。
4. 从演员麦通道中减去全部估计路径。

报告给出每对通道的 `gain_db`、`delay_samples`、`delay_ms`、`before_energy`、`after_energy` 和 `residual_ratio`。参考段总残余比超过工程上限，或直达审计段能量损失超过上限时，整次合成以业务码拒绝，不写母线。

### 3. 双耳 HRTF

使用可复现的球头解析模型：

```text
lateral   = sin(azimuth) * cos(elevation)
ILD_dB    = ILD_max * lateral
theta_eff = asin(clamp(lateral))
ITD_s     = (head_radius / speed_of_sound) * (theta_eff + lateral)
```

ILD 为正表示右耳更强；ITD 为正表示左耳更晚到达，即声源位于右侧。左右耳时延分别为 `base_delay ± ITD/2`。远耳使用 Woodworth 时延、随侧向角连续变化的头影低通截止频率和远耳标量衰减，HRIR 做有限长度因果截断并按直流增益归一化。

控制轨迹先在 `parameter_block_seconds` 网格上插值、平滑，再对 ILD 和 ITD 的一阶变化率做限幅。渲染使用 50% 重叠 Hann 窗和逐块解析 HRIR 的 overlap-add，避免块边界产生采样级声像断点。

### 4. 母带响度与真峰值

- K 加权采用 BS.1770 两段双二阶滤波器，并在频域实现线性滤波。
- 积分响度使用 400 ms 块、100 ms 步长、绝对门限 `-70 LUFS` 和相对门限。
- 真峰值使用 4 倍带限 FFT 过采样测量。
- 先按目标响度归一化；如接近真峰值上限，使用确定性软峰限幅和必要的全局安全缩放。
- 最终报告包含积分响度、目标偏差、真峰值、样本峰值、被压制样本数和各演员能量占比；最终 PCM 样本绝对值也不超过真峰值上限对应的线性幅度。

## 续渲与快照

导出按 `export.block_frames` 固定块执行。每个块写完并 `fsync` 后，才原子提交一个快照；快照包含：

- `completed_blocks`、`completed_frames`、总块数；
- 工程摘要、完整输入 WAV 摘要；
- 全部串扰估计结果及指纹；
- 完整声像参数摘要及指纹；
- 已提交 PCM 字节数、数据偏移和前缀 SHA-256。

恢复规则：

- 只认校验和正确、且已提交前缀哈希匹配的最新快照。
- 强制终止后，未提交的尾字节被截断，从最后一次完整提交继续。
- 最新快照损坏时，自动回退到最近一次有效提交。
- 当前工程摘要、输入素材摘要、串扰结果或声像参数与快照不一致时，丢弃全部旧进度，报告 `E_RESUME_MISMATCH`，从块 0 和重新计算的耳间参数开始。
- `Ctrl+C` 只设置优雅中断标志：完成当前块、提交快照、写出 `recovery_report.json`，进程退出码为 `0`。
- `SIGKILL`/强制终止不写退出报告；下次启动仅使用已提交快照。

正常运行完成后保留 `.progress/snapshots/`，便于审计；报告和恢复报告不含时间戳或绝对路径，字段顺序固定。

## 报告与输出

成功合成后输出：

- `binaural_master.wav`：双声道 PCM WAV。
- `production_report.json`：
  - 输入摘要与校验状态；
  - 每演员、每扬声器的串扰估计和能量对比；
  - 每演员的方向时间线、段落 ILD/ITD 均值/极值/最大变化率；
  - 母线积分响度、真峰值、压制样本数量；
  - 各演员贡献能量与比例；
  - 分块交付信息。

`recovery_report.json` 仅在可处理中断时生成，包含已完成块数、下一块、工程摘要、串扰/声像指纹和 `resume_available`。

## 验证

运行：

```bash
PYTHONPATH=. .venv/bin/python -m unittest discover -s tests -v
```

本环境（Python 3.14.4、NumPy 2.5.5）的实际结果：

- 10 项自动化测试全部通过，包括高波峰素材的响度/真峰值闭环限幅压力用例。
- 素材生成两次的文件树 SHA-256 完全一致，共 7 个文件。
- 示例 4 条串扰路径估计为 `-14/-18/-20/-12 dB`，时延为 `120/192/156/84` 采样，和工程量化值一致。
- 两个演员的参考段残余能量比分别约 `1.07e-13`、`9.34e-14`，直达审计段能量损失均为 `0`。
- 常量方位 `-60°,-30°,0°,+30°,+60°` 下，右/左耳能量比依次为 `-15.81,-9.11,0,+9.11,+15.81 dB`，互相关 ITD 依次为 `-479,-271,0,+271,+479 µs`。
- 示例母带积分响度 `-16.00 LUFS`，真峰值 `-2.16 dBTP`，目标偏差 `0.00 LU`，压制样本数 `0`。
- 示例母带 `binaural_master.wav` 的 SHA-256 为 `b5a7c9891e49af8df8300a42093f9007bbb6a808c7ce214fdd9a33c69ccb4a23`。
- 可处理 `SIGINT` 实测退出码 `0`，写出恢复报告，重启后续渲结果与完整合成逐字节一致。
- `SIGKILL` 实测在提交 9 个块快照后终止，无完成报告；恢复母带与完整合成 SHA-256 完全一致。
- 损坏最新快照后恢复会报告 `E_SNAPSHOT_CORRUPT_FALLBACK` 并回退到前一有效提交；工程摘要变更会报告 `E_RESUME_MISMATCH` 并从零开始。

## 边界与约定

- 串扰模型为每对演员/扬声器单路径、整数采样时延；生成器也使用同一量化模型，因此离线样本可精确复核。若离线素材本身含分数采样时延，需要在该层增加亚采样插值估计。
- 演员和扬声器输入文件必须为工程声明的单声道；双耳导出必须为两声道。
- 直达损失审计依赖 `direct_probe_windows[]`。外部工程如果不声明该字段，报告会保留 `direct_loss_ratio: null`，不应把该次结果视为完成了直达损失验收。
- 双耳母带使用离散左/右声道叠加，不做额外的立体声下混，以保持 HRTF 的左右耳特性。

# 离线多机位鼓组分轨时间对齐

本目录实现了一个仅依赖 Python 标准库与 NumPy 的确定性离线工具。它以场记
小节起点为锚点，检测各机位底鼓击打，估计辅助机位相对基准机位的整数采样漂移
曲线，按曲线重采样并合并鼓组母带。渲染固定分块进行，每块完成后原子提交恢复
快照；Ctrl+C 与强制终止后的续跑结果都与一次性完整渲染逐字节一致。

## 快速开始

要求 Python 3.10+、NumPy 1.24+。

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
source .venv/bin/activate
python -m pip install -r requirements.txt

python -m drum_align generate-demo --output-dir demo_output
python -m drum_align render --project demo_output/project.json
```

也可用 `python main.py ...` 或安装后的 `drum-align ...` 调用同一 CLI。输出默认由
工程 JSON 的 `outputs` 决定：

- `aligned_master.wav`：48 kHz、16-bit PCM 对齐母带，总帧数等于基准分轨。
- `alignment_report.json`：完整对齐/恢复报告。
- `alignment_state.json`：仅中断时保留的原子进度快照。

生成样例会创建三台机位、每 0.5 秒一拍、含真实双踩及 13 个场记小节起点。
辅助机位具有不同录音起点、不同时钟比率与独立确定性噪声，`cam_c` 还带有反相，
可直接用于验收。

## 工程 JSON

规范格式如下。小节位置使用**基准机位时间轴上的整数采样**；也可用
`bar_times_seconds` 或 `HH:MM:SS[:FF]` 时间码表达。

```json
{
  "schema_version": 1,
  "sample_rate": 48000,
  "base_camera": "cam_a",
  "bar_starts_samples": [48000, 144000, 240000, 336000],
  "min_bar_starts": 4,
  "tracks": [
    {"camera": "cam_a", "path": "cam_a.wav", "gain": 1.0},
    {"camera": "cam_b", "path": "cam_b.wav", "gain": 0.82}
  ],
  "phase_check": {
    "window_samples": 4096,
    "pre_roll_samples": 480,
    "threshold": 0.30,
    "attenuation_gain": 0.45
  },
  "curve_points": 10,
  "max_offset_seconds": 2.0,
  "render": {"block_samples": 16384},
  "outputs": {
    "master": "aligned_master.wav",
    "report": "alignment_report.json",
    "snapshot": "alignment_state.json"
  }
}
```

支持字段别名包括 `reference_camera`、`min_anchor_count`、`bar_times_seconds`、
`timecode_table`、数组形式的 `tracks`、`phase`、`block_samples` 等。输入 WAV
必须为未压缩 16-bit PCM、48 kHz、单声道或双声道；输出声道数采用基准机位。
辅助文件至少要有基准文件同样的帧数，否则在开始渲染前以 `E_TRACK_SHORT` 退出。

### 漂移曲线语义

报告中的曲线满足：

```text
aux_source_sample = reference_sample + interp(ticks_samples, offsets_samples)
```

`offsets_samples` 始终是整数。刻度之间线性插值，首刻度之前与末刻度之后保持最近
刻度的常量偏移。重采样使用线性 PCM 插值，因此曲线刻度上是整数采样对齐；输出长度
不受偏移端点影响，始终截取/补零到基准帧数。

### 混合与相位

每个场记锚点附近的窗口会对齐后的辅助轨与基准轨计算 Pearson 相关。任一路低于
`phase_check.threshold` 即标记该机位，但仍参与混合，其有效增益为：

```text
effective_gain = track_gain * phase_check.attenuation_gain
```

未标记机位只乘 `track_gain`。混合采用浮点累加、四舍五入并饱和裁剪到 int16，
不做隐式归一化，保证同一输入的浮点运算路径和块边界一致。

## 中断与恢复

- 每个固定采样块渲染完成后，先写 PCM、`fsync`，再以临时文件 + `os.replace`
  原子提交 JSON 快照。
- 快照记录 `completed_blocks`、`completed_samples`、工程 SHA-256 摘要、漂移曲线、
  PCM 字节数与增量 SHA-256、恢复次数和恢复事件。
- 工程 JSON 或任一 WAV 内容变化会使摘要变化。旧快照被整体删除，从第 0 块开始，
  并以 `E_RESUME_MISMATCH` 同时写入 stderr 事件和报告 `recovery_events`。
- 快照中的曲线必须与本次从输入重算的曲线完全一致；不一致同样按
  `E_RESUME_MISMATCH` 丢弃，绝不沿用旧刻度。
- 若快照或 PCM 损坏，则丢弃并以 `E_RESUME_DAMAGED` 从零开始。
- Ctrl+C 只设置协作停止标志。当前块完成后提交快照、写 `report_kind=recovery`
  的报告，CLI 以退出码 0 结束；未完成时不生成最终母带。
- SIGKILL 等强制终止不保证报告。重启时以最后已提交快照为准，PCM 尾部未提交字节
  会被截断；续跑母带与一次性完整渲染逐字节一致。

## 报告

报告字段顺序固定，不含时间戳或绝对路径。主要字段：

- `project_digest`、`status`、`report_kind`、`master`、`bar_starts_samples`
- 每机位 `hits_samples`、完整 `hit_intervals_samples`、间隔摘要
- `drift_curve`：刻度、整数偏移、对应场记起点、基准/源击打、局部相关值
- `phase_check` 与 `quality_flags`：低相关窗口的采样范围和相关系数
- `errors`、`warnings`、`recovery_events`、`resume_count`
- `render.block_samples/completed_blocks/total_blocks`

硬错误通过 CLI stderr 的结构化 JSON 返回，退出码非零；质量低相关只进入
`quality_flags`，不阻止合并。

## 业务码

| 业务码 | 含义 |
| --- | --- |
| `E_PROJECT_READ` / `E_PROJECT_INVALID` | 工程不可读或字段非法 |
| `E_SAMPLE_RATE` | 工程或 WAV 不是 48 kHz |
| `E_CHANNELS` | 声道数不是单声道/双声道 |
| `E_WAV_FORMAT` / `E_WAV_INVALID` | 非 16-bit PCM 或 WAV 损坏 |
| `E_TRACK_MISSING` | 机位文件缺失，含机位名与文件名 |
| `E_TRACK_SHORT` | 辅助机位短于基准，含帧数原因 |
| `E_BAR_STARTS_COUNT` | 场记起点少于工程声明下限 |
| `E_BAR_STARTS_ORDER` | 场记起点非严格升序 |
| `E_BAR_STARTS_RANGE` | 场记起点超出基准长度 |
| `E_DETECTION_INSUFFICIENT` | 某机位击打数少于场记起点数 |
| `E_RESUME_MISMATCH` | 旧快照工程摘要/曲线不一致，已从零开始 |
| `E_RESUME_DAMAGED` | 快照或 PCM 损坏，已从零开始 |
| `E_OUTPUT` / `E_INTERNAL` | 文件系统或未预期错误 |

质量告警码为 `W_PHASE_CORRELATION`。

## 算法说明

1. 单声道化后以 5 ms 居中低通能量和短时能量上升检测低频击打；短时高频瞬态门
   抑制同一底鼓衰减尾部的伪峰，同时保留快速双踩。
2. 用包络 FFT 互相关求第一段粗机位差。
3. 按场记小节顺序，以预测偏移和波形相关候选匹配辅助击打；再对局部波形做整数
   采样互相关精修。
4. 在匹配小节中等距选取报告刻度，根据对应击打差得到整数偏移。
5. 按曲线逐块线性重采样、相位检查、衰减并混合。

## 验证

```bash
python -m unittest discover -v
```

本次实际执行结果（Python 3.14.4、NumPy 2.5.5）：

- 9 个测试全部通过，耗时约 16 秒。
- 合成样本检测击打数：`cam_a=61`、`cam_b=64`、`cam_c=63`，均不少于 13 个场记起点。
- 估计曲线示例：`cam_b` 从 `-6594` 漂移到 `-7077` 采样；`cam_c` 从 `3999`
  漂移到 `4333` 采样。
- 独立重采样并重新检测后，所有报告刻度上辅助/基准击打偏差均为 `0` 采样
  （要求不超过 2）。
- `cam_c` 各锚点窗口相关系数约 `-0.9999`，被质量告警并衰减；`cam_b` 通过。
- 同一输入重复运行，母带与报告字节完全一致。
- Ctrl+C 中断退出码为 0、无最终母带、恢复报告与快照存在；续跑与一次性渲染
  SHA-256 相同（样本为
  `96bd2fe6da37e24f2a185096ec13d1b58e5bb8099613e63185dfbc3e998679e7`）。
- SIGKILL 后仅凭快照续跑，母带哈希同样逐字节一致。
- 进度摘要不一致时报告 `E_RESUME_MISMATCH`，新快照块数从 1 重新开始，不沿用
  原快照的 3 块进度。

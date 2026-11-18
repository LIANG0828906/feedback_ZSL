# 离线确定性场景音频渲染器技术说明

## 1. 交付范围

实现位于 `src/scene_audio/`，包括：

- `generator.py`：确定性多轨 WAV 与场景 JSON 生成器。
- `scene.py`：场景解析、结构校验、WAV 元数据校验与内容摘要。
- `resample.py`：分段锚点时间映射、全轨重采样、锚点诊断。
- `detection.py`：原始采样率下的事件起点检测。
- `camera.py` / `spatial.py`：相机路径插值、解析式 ITD/ILD 双耳模型、
  遮挡层统计。
- `reverb.py`：动态 RT60/干湿比调度与可序列化反馈混响网络。
- `mastering.py`：ITU-R BS.1770 积分响度、4 倍过采样真峰值、限幅与
  24-bit PCM 安全量化。
- `snapshot.py` / `render.py`：固定块渲染、原子快照、恢复语义、稳定报告。
- `cli.py`：Windows/Linux 均可用的整数退出码 CLI。

所有运行时媒体均由代码生成，不读取外部模板、不访问网络。

## 2. 场景格式

最小结构如下：

```json
{
  "version": 1,
  "name": "demo",
  "duration": 6.0,
  "output": {
    "sample_rate": 48000,
    "channels": 2,
    "block_frames": 8192,
    "report_frame_stride": 4800,
    "target_loudness_lufs": -23.0,
    "true_peak_limit_dbtp": -1.0
  },
  "camera": {
    "up": [0.0, 0.0, 1.0],
    "path": [
      {"time": 0.0, "position": [0, 0, 1.7], "forward": [0, 1, 0]}
    ]
  },
  "tracks": [
    {
      "name": "dialogue",
      "path": "assets/dialogue.wav",
      "sample_rate": 44100,
      "channels": 1,
      "gain_db": 0.0,
      "sync_anchors": [
        {"source_sample": 0, "target_time": 0.0},
        {"source_sample": 264600, "target_time": 6.0}
      ],
      "events": [
        {
          "id": "DLG_001",
          "type": "dialogue",
          "start_time": 0.55,
          "duration": 1.25,
          "source_position": [2.4, 2.8, 1.6],
          "gain_db": 0.0,
          "detection": {"threshold": 0.08, "search_ms": 35.0}
        }
      ]
    }
  ],
  "reverb": {
    "blend_width": 0.2,
    "regions": [
      {"start_time": 0.0, "end_time": 6.0, "rt60": 0.8, "wet_dry": 0.2}
    ]
  },
  "occluders": [
    {"id": "wall", "min": [-1, 2, 0], "max": [1.5, 2.1, 3]}
  ],
  "spatial_limits": {
    "max_ild_db": 10.0,
    "max_itd_us": 620.0,
    "max_ild_rate_db_per_s": 90.0,
    "max_itd_rate_us_per_s": 6000.0
  }
}
```

### 2.1 硬校验

下列情况整体拒绝，不进入块渲染，不写 `master.wav`：

- 相机路径时间为负、超出场景时长或非严格递增：`E_CAMERA_TIME`、
  `E_CAMERA_RANGE`、`E_CAMERA_SCHEMA`。
- 事件起始为负、时长为非正、超出场景时长或同轨重叠：
  `E_EVENT_TIME`、`E_EVENT_DURATION`、`E_EVENT_OVERLAP`。
- 文件不存在、无法解码：`E_TRACK_MISSING`、`E_TRACK_DECODE`。
- 实际采样率/声道数不符：`E_TRACK_RATE_MISMATCH`、
  `E_TRACK_CHANNEL_MISMATCH`。
- 锚点非严格递增、越界：`E_SYNC_ANCHOR`。
- 混响区间不连续、越界、混合宽度非法：`E_REVERB_REGION`、
  `E_REVERB_BLEND`。
- 最终响度或真峰值不满足约束：`E_MASTER_LOUDNESS`、
  `E_MASTER_TRUE_PEAK`。
- 快照不可用或与输入不一致：`E_RESUME_MISMATCH`。

每条硬错误均带 `code`，并尽可能带 `track` 或 `event_id`。错误 JSON 和最终
报告都显式包含 `hard_errors` 与 `quality_warnings` 两类字段。

## 3. 时间轴与锚点重采样

锚点把源采样位置映射到场景时间。锚点之间为分段线性映射，外侧沿用最近
一段的恒定斜率。渲染网格为：

```text
t[n] = n / output_sample_rate
s[n] = anchor_map.source_at_target_time(t[n])
```

每个声道以双精度插值到 `s[n]`；降采样时先执行 8 阶零相位低通，避免明显
混叠。输出长度固定为 `round(duration * output_sample_rate)`，与源文件长度
和目标速率无关。

事件先在原始采样率检测起点，再通过正向锚点映射到目标时间，得到整数输出
偏移。事件片段从检测到的位置开始，放到 `round(start_time * output_rate)`。
因此报告中的 `placement_error_samples <= 0.5`。检测失败时使用声明时间，
写入含 `event_id` 的 `DETECTION_FAILED` 质量告警并继续。

## 4. 双耳空间化

对每个输出采样：

1. 在相机路径上插值位置和后向，重正交化得到相机前/右/上基。
2. 计算声源相对相机的方位角 `az` 与仰角 `el`。
3. 解析模型：

```text
ITD = (2 r / c) * sin(az) * cos(el)
ILD = ILD_strength * sin(az) * cos(el)
```

4. 分别钳制 ITD/ILD 幅度；对相邻采样差分做斜率限制，保证变化速率不超过
   场景上限。
5. 使用分数延迟分别渲染左、右耳，并按 ILD 与距离衰减计算增益。

模型只使用 NumPy 可见的解析计算，不依赖 HRTF 文件。相同输入重复调用时
输出数组逐字节一致。方位角跨越边界时 `sin/cos` 连续，且额外斜率限制避免
声像整侧跳变。

## 5. 动态混响

混响参数按场景时间区间给出。边界 `b` 的过渡区内使用 smoothstep：

```text
u = (t - (b - w/2)) / w
s = u^2 (3 - 2u)
rt60(t) = rt60_prev + s * (rt60_next - rt60_prev)
```

干湿比使用同一条连续曲线。渲染端为左右声道各 4 个反馈梳状滤波器与 2 个
全通滤波器；每个梳状滤波器的反馈系数按当前采样帧 RT60 计算：

```text
g = 10^(-3 * delay_seconds / rt60)
```

状态包括全部延迟线、写指针、阻尼低通状态和全通状态，逐块写入快照。因此
从中断块续算与一次性渲染一致。

## 6. 母带与报告

- 母带先在双精度下汇总事件双耳信号与事件增益。
- 积分响度按 BS.1770：K 加权、400 ms 块、75% 重叠、绝对/相对门限。
- 真峰值使用 4 倍过采样测量。
- 响度归一化与 lookahead 峰值限制迭代执行，最后进行 24-bit PCM 安全量化
  和样本级上限检查。
- 事件贡献占比按各事件空间化后的双耳能量计算，比例之和为 1。
- `suppressed_samples` 记录限幅器实际修改的通道样本数。

`report.json` 是最终报告，字段顺序固定，不含时间戳和绝对路径；包含：

- 场景摘要、恢复次数、总采样数。
- 硬错误、质量告警、每事件检测和对齐数据。
- 每帧相机、活动层数、每事件遮挡层数、RT60、干湿比、ILD、ITD。
- 空间速率和混响速率摘要。
- 母带 LUFS、真峰值、事件贡献、压制样本数、WAV SHA-256。

## 7. 快照与恢复

每完成一个固定块，已渲染母带累加器、完整混响 DSP 状态、场景摘要、完成
块数、当前混响区间和空间化摘要一起写入单个 `.npz` 临时文件，`fsync` 后
用 `os.replace` 整体替换正式快照。旧快照不会与新数据混合。

启动时：

- 快照存在且场景摘要、总样本数、块大小、混响区间状态、DSP 数组形状全部
  匹配：从 `completed_blocks * block_frames` 继续。
- 任一不匹配：删除快照并返回 `E_RESUME_MISMATCH`，不沿用旧块偏移。
- `Ctrl+C`：当前块结束后提交快照，写 `recovery_report.json`，退出码 0。
- SIGKILL/SIGTERM 等强制终止：不保证恢复报告；下次只使用最后一次完整提交
  的快照。
- `--verify-reference`：恢复母带与参考 WAV 逐字节比较；不一致时写
  `difference_report.json`，其中含块编号、采样坐标、通道及两侧样本值，退出
  码 3。

## 8. 实际验证结果

环境：Python 3.14.4、NumPy 2.5.5、SciPy 1.18.1、pytest 9.2.0。

```text
$ .venv/bin/python -m pytest -q
..................                                                       [100%]
18 passed
```

确定性 demo 渲染结果（6.0 s、48 kHz、双声道、288000 样本）：

- 积分响度：`-23.000000 LUFS`，目标 `-23.0 LUFS`。
- 真峰值：`-11.368092 dBTP`，上限 `-1.0 dBTP`。
- 母带 SHA-256：
  `6ab387ec67604ba2176f7d59a779a2369244eee7f243c25c94b63970649fbffa`。
- 5 个事件全部检测成功，放置误差均为 `0` 个输出采样。
- 最大 ILD 变化率：`6.289 dB/s`，上限 `90 dB/s`。
- 最大 ITD 变化率：`401.06 us/s`，上限 `6000 us/s`。
- 最大 RT60 变化率：`3.45 s/s`，混响边界连续。
- 两次独立完整渲染母带字节与报告完全一致。
- SIGINT 后恢复母带与完整渲染 SHA-256 完全一致，最终报告
  `recovery_count = 1`。
- SIGKILL 后从最近提交快照恢复，母带同样逐字节一致。
- 篡改场景或快照混响状态均返回 `E_RESUME_MISMATCH` 并删除旧快照。
- 静音事件产生 `DETECTION_FAILED` 告警后，其余事件与母带继续完成。


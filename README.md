# 剧院沉浸式双耳母带渲染器

这是一个完全离线、确定性的八轨剧院录音双耳母带组件。它读取工程 JSON 和
八路 WAV，完成输入校验、扬声器返送泄漏抑制、头部追踪声像补偿、连续参数
插值、BS.1770 响度/真峰值合规、固定块快照和中断恢复。所有测试素材由代码
生成，不访问网络或外部媒体。

## 快速开始

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt

# 重新生成示例工程描述（仓库中已有 examples/project.json）
python tools/make_example_project.py

# 生成八路确定性 WAV
python -m theatre_binaural generate --project examples/project.json

# 校验 JSON、文件头、采样率、位深、声道、追踪序列和四元数
python -m theatre_binaural validate --project examples/project.json

# 完整渲染
python -m theatre_binaural render \
  --project examples/project.json \
  --master artifacts/master.wav \
  --report artifacts/render_report.json \
  --state-dir artifacts/state
```

若系统 Python 没有 `ensurepip`，可使用 `uv` 创建同样的环境：

```bash
uv venv --python 3.14 .venv
uv pip install --python .venv/bin/python -r requirements.txt
```

也可使用默认渲染入口：

```bash
python -m theatre_binaural --project examples/project.json -o master.wav
```

`render` 默认保留已提交快照。需要丢弃旧进度时传入 `--fresh`。测试中断时可用
`--block-delay-ms` 在每个已提交块后加入固定等待时间，不影响音频结果。

## 示例工程

示例包含 5 路演员、1 路舞台环境、1 路观众氛围和 1 路扬声器返送参考，共 8 路。
解析器不限制这四类角色的具体组合，只要求恰好 8 路、至少有一个可解析的参考
通道，并为所有参与混音的空间声源提供锚点。

舞台坐标为米制右手坐标：`x` 向右、`y` 向前、`z` 向上。头部位置是
`[x, y, z]`；默认四元数为 `[w, x, y, z]`，语义是 `local_to_world`，即把头部
局部轴旋转到舞台坐标。`orientation_convention` 也可设为 `world_to_head`。

核心字段如下：

```json
{
  "seed": 233,
  "duration_seconds": 6.0,
  "sample_rate": 48000,
  "tracks": [
    {
      "name": "actor_01",
      "path": "audio/actor_01.wav",
      "sample_rate": 48000,
      "channels": 1,
      "bit_depth": 24,
      "source_type": "actor",
      "anchor": {"x": 2.6, "y": 1.4, "z": 1.55}
    }
  ],
  "reference_channel": "speaker_reference",
  "leakage": {
    "reference_channel": "speaker_reference",
    "injection_channels": ["actor_01", "actor_03", "actor_05"],
    "gain_db": -10.0,
    "channel_gain_db": {"actor_01": -10.0, "actor_03": -11.0, "actor_05": -9.0},
    "injection_delay_ms": 9.0,
    "max_delay_ms": 25.0,
    "lowpass_hz": 120.0
  },
  "head_tracking": {
    "quaternion_order": "wxyz",
    "orientation_convention": "local_to_world",
    "max_ild_rate_db_per_s": 600.0,
    "max_itd_rate_us_per_s": 30000.0,
    "samples": [
      {
        "t": 0.0,
        "position": [0.0, -0.2, 1.6],
        "orientation_quaternion": [1.0, 0.0, 0.0, 0.0]
      }
    ]
  },
  "head_model": {
    "head_radius_m": 0.0875,
    "speed_of_sound_m_s": 343.0,
    "max_ild_db": 12.0
  },
  "binaural": {
    "downmix": {
      "actor_01": {"left": 1.0, "right": 1.0},
      "stage_ambience": {"left": 0.45, "right": 0.45}
    }
  },
  "interpolation": {
    "azimuth_tolerance_deg": 2.0,
    "checks": [
      {
        "time_s": 1.5,
        "channel": "actor_01",
        "expected_azimuth_deg": 66.70971278
      }
    ]
  },
  "targets": {"loudness_lufs": -23.0, "true_peak_dbtp": -1.0},
  "render": {
    "block_frames": 2400,
    "master_bit_depth": 24,
    "limiter_lookahead_ms": 5.0,
    "limiter_release_ms": 80.0
  }
}
```

兼容常见字段别名，例如 `tracks/channels/sources`、`path/file/file_path`、
`t/time/time_s/timestamp`、`orientation/quaternion/rotation`、
`targets/master/loudness` 以及 `left/left_gain`。实际输入文件必须与声明一致。

## 处理链

1. **确定性素材生成**
   - 演员：随机但可复现的爆破音噪声簇、带颤音和共振峰加权的持续元音谐波、
     明确的停顿段。
   - 环境/观众：低频轰鸣与宽带空气声、观众音节式幅度调制。
   - 参考：24–120 Hz 低频噪声与确定性低频音。
   - 对声明的注入通道施加低频参考延迟和逐通道增益。生成器重复运行写出的
     WAV 逐字节一致。

2. **泄漏估计与抑制**
   - 以 150 Hz 高通能量和自适应门限识别演员停顿。
   - 在停顿段对演员低频成分与参考低频成分类互相关，搜索工程声明的最大延迟，
     再由最小二乘估计泄漏增益。
   - 从演员通道减去延迟参考成分，分别统计抑制前后泄漏频段能量和演员直达峰值。

3. **解析双耳模型**
   - 将舞台锚点转到头部局部坐标，计算
     `azimuth = atan2(x, y)`、`elevation = atan2(z, hypot(x, y))`。
   - 令 `lateral = sin(azimuth) cos(elevation)`，采用可复现解析模型：
     `ILD = max_ild * lateral`，
     `ITD = -(2 * head_radius / speed_of_sound) * lateral`。
   - 距离同时产生公共传播延迟和公共距离增益；左右延迟/增益由 ITD/ILD 重建。
   - 追踪点之间线性插值方向参数和耳间参数。ILD/ITD 在追踪点先做斜率限制，
     因而线性渲染段也不会超过工程声明的变化率上限。

4. **母带合规**
   - K 加权使用 BS.1770 的 De Man 精确系数公式。
   - 400 ms 块、75% 重叠、绝对 -70 LUFS 门限和相对 -10 LU 门限。
   - 真峰值使用 4 倍过采样估计。
   - 若响度归一化后真峰值超限，使用带前瞻的最小增益包络限幅；前瞻样本在输出
     时补偿裁剪，母线帧数严格不变。最后以量化后的 WAV 重新测量并复核。

## 快照与恢复

状态目录内包含：

- `snapshot.json`：已完成块数、总块数、工程/泄漏/声像摘要、块引用和校验和。
- `blocks/block_XXXXXX.f64`：已提交母带原始混音块。

每个块先写临时文件并 `fsync`，再原子替换块文件；之后写临时清单并原子替换
`snapshot.json`。因此强制终止最多留下一个未被清单引用的孤立块，下次启动会
忽略并清理。恢复时：

- 校验清单校验和、块文件存在性、长度、SHA-256、块顺序和几何。
- 重算工程摘要、泄漏估计摘要和声像参数摘要；任何不一致会整体删除旧进度，
  返回 `E_RESUME_MISMATCH`。
- 对已提交前缀做确定性重渲染比对。不一致时返回 `E_RESUME_DIVERGENCE`，错误
  详情包含块编号、`left/right` 通道和样本位置。
- 只用已提交块拼接前缀，再续算剩余块。最终母带与一次性渲染逐字节一致。

`SIGINT` 由信号处理器转为中断标志：当前块渲染、提交和 `fsync` 完成后退出，
写出 `*.recovery.json`，退出码为 0。`SIGKILL` 不写退出报告，下次仅从清单中
已提交的块继续。

## 错误与退出码

错误只输出一个稳定 JSON 对象到 stderr：

```json
{
  "status": "error",
  "error_code": "E_SAMPLE_RATE_MISMATCH",
  "channel": "actor_01",
  "exit_code": 2,
  "message": "declared 48000 Hz, actual 44100 Hz"
}
```

主要错误码：

| 错误码 | 含义 |
| --- | --- |
| `E_FILE_MISSING` | 工程声明的音频文件不存在 |
| `E_SAMPLE_RATE_MISMATCH` | 实际采样率与声明不符 |
| `E_BIT_DEPTH_MISMATCH` | 实际位深与声明不符 |
| `E_CHANNEL_COUNT_MISMATCH` | 实际声道数与声明不符 |
| `E_TRACKING_TIME_NOT_STRICT` | 追踪时间戳非严格递增 |
| `E_QUATERNION_NOT_NORMALIZED` | 四元数模长偏离 1 |
| `E_RESUME_MISMATCH` | 工程、泄漏或声像状态与快照不一致 |
| `E_SNAPSHOT_CORRUPT` | 快照清单/块损坏 |
| `E_RESUME_DIVERGENCE` | 恢复前缀与确定性重渲染不一致 |
| `E_LEAKAGE_REDUCTION` | 注入通道泄漏抑制不足 12 dB |
| `E_DIRECT_PEAK_LOSS` | 演员直达峰值损失超过 1 dB |
| `E_LOUDNESS_TARGET_UNREACHABLE` | 在峰值约束下响度偏差超过 0.5 LU |
| `E_TRUE_PEAK_EXCEEDED` | 量化母带超过真峰值上限 |

退出码：`0` 成功或可处理中断；`2` 输入错误；`3` 恢复状态不匹配；`4` 快照
损坏；`5` 恢复差分；`6` 渲染/合规失败。

## 报告

`render_report.json` 固定字段顺序，不包含时间戳和绝对路径，包括：

- 母带 SHA-256、帧数、位深、积分响度、真峰值、样本峰值。
- 限幅是否触发、最终触发样本数、累计触发样本数、前瞻样本数、处理延迟已补偿。
- 每个演员通道的泄漏延迟、增益、停顿长度、抑制前后能量和下降量、直达峰值损失。
- 所有追踪时刻的方位角/仰角、ILD/ITD、左右增益和延迟。
- 检查时刻的插值参数、期望方位角、实测方位角、误差和容差判定。
- 通道贡献能量、贡献占比和母线峰值。
- 恢复块数和快照验证状态。

## 已经执行的验证

环境：Python 3.14.4、NumPy 2.5.5、SciPy 1.18.1、SoundFile 0.14.0。

```text
$ .venv/bin/python -m pytest -q
...........                                                      [100%]
11 passed
```

默认示例的一次完整渲染结果：

- 母带：288000 帧、双声道、24-bit PCM，SHA-256
  `b8bd22bce61e1bdf4d6d9d93e035efb1d5dff98ed2f72f78402a4453fc9c7322`。
- 积分响度：`-23.000000 LUFS`；真峰值：`-9.449196 dBTP`（上限 -1 dBTP）。
- 注入通道 `actor_01/03/05` 的泄漏能量下降：
  `30.674 / 30.911 / 31.079 dB`，均超过 12 dB。
- 对应直达峰值损失：`0.008 / 0.000 / 0.000 dB`，均低于 1 dB。
- 最大 ILD 变化率 `1.349 dB/s`（上限 600 dB/s）；最大 ITD 变化率
  `57.350 us/s`（上限 30000 us/s）。
- 检查时刻最大方位角偏差 `0.472008 deg`（容差 2 deg）。
- 真实 `SIGKILL` 在第 3/120 块后截断，续算母带与一次性母带 SHA-256 相同。
- 真实 `SIGINT` 在第 2/120 块后触发，当前块完成、恢复报告写出、退出码 0；
  再次启动后母带仍与一次性母带逐字节一致。
- 高响度（-8 LUFS 目标）触发限幅：积分响度 `-8.035712 LUFS`，真峰值
  `-1.000000 dBTP`，母线仍为 288000 帧。

自动化测试还覆盖了缺失文件、采样率/位深/声道不匹配、非递增时间戳、未归一化
四元数、工程摘要不匹配、快照损坏、恢复差分块/通道定位、ILD 单调性和报告
稳定性。


# 确定性离线多轨母带组件

这是一个仅依赖 Python 3.10+ 与 numpy 的离线母带处理组件。它读取工程
manifest、11 轨 PCM WAV、自动化曲线和输出配置，完成校验、统一采样率、
锚点漂移补偿、同步脉冲对齐、自动化增益、BS.1770 响度校准、真峰值限幅，
并写出 PCM 16/24 位或 32 位浮点 WAV 与可复核 JSON 报告。

所有验收素材均由 `mastering.fixture` 确定性合成，不使用外部音频库、网络或容器。

## 快速开始

```bash
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt

# 生成 180 秒、11 轨、44.1/48/96 kHz、16/24 bit 样本
.venv/bin/python -m mastering.fixture \
  --output outputs/demo --duration 180 --target-lufs -14

# 母带处理
.venv/bin/python -m mastering \
  --manifest outputs/demo/manifest.json \
  --automation outputs/demo/automation.json \
  --config outputs/demo/output_config.json \
  --output outputs/demo/master.wav \
  --report outputs/demo/report.json
```

成功时 CLI 在 stdout 输出一行 JSON 摘要，退出码为 `0`。失败时 stdout 输出
结构化错误 JSON 且不写母带；校验失败退出码为 `2`，响度容差失败为 `3`。

Windows 10/11 使用对应的虚拟环境路径：

```bat
py -3 -m venv .venv
.venv\Scripts\python -m pip install -r requirements.txt
.venv\Scripts\python -m mastering.fixture --output outputs\demo --duration 180 --target-lufs -14
.venv\Scripts\python -m mastering --manifest outputs\demo\manifest.json ^
  --automation outputs\demo\automation.json --config outputs\demo\output_config.json ^
  --output outputs\demo\master.wav --report outputs\demo\report.json
```

## 测试与验收

```bash
# 21 项单元/端到端回归测试
.venv/bin/python -m unittest discover -v

# 默认 180 秒真实规模验收；生成两次母带并核对逐字节一致性
.venv/bin/python tools/run_acceptance.py \
  --workdir outputs/acceptance --duration 180 \
  --summary artifacts/acceptance_summary.json
```

当前已执行的真实规模结果：

| 项目 | 结果 |
|---|---:|
| 输入 | 11 轨，180 秒，44.1/48/96 kHz，16/24 bit PCM |
| 输出 | 48 kHz、双声道、PCM 24 bit |
| 输出帧数 | 8,640,000 |
| 同步最大残差 | 0 样本 |
| 漂移锚点最大内容位置误差 | 0 样本 |
| 被平滑的急剧段 | 5 段 / 1.2 秒 |
| 限幅触发帧 | 369 |
| 最大限幅衰减 | 8.5381 dB |
| 限幅前目标响度偏差 | 0.0 LU（目标 -14 LUFS） |
| 最终母带响度 | -14.0343 LUFS（偏差 0.0343 LU） |
| 最终解码真峰值 | -1.1200 dBTP（上限 -1.0 dBTP） |
| 两次运行 | WAV 逐字节一致，报告数值一致 |

完整机器可读结果见 `artifacts/acceptance_summary.json`，详细逐轨数据见
`outputs/acceptance/report.json`，母带见 `outputs/acceptance/master.wav`。
本次 PCM 24 位母带的 SHA-256 为
`b429dee96a607168894a4ad3f5f21006b9bedcfc72e877ab30945f2202da9ee0`。

## 输入格式

### `manifest.json`

```json
{
  "project": {
    "name": "session",
    "duration_seconds": 180.0,
    "tick_rate": 96000,
    "sync_target_seconds": 0.02
  },
  "tracks": [
    {
      "id": "kick",
      "name": "Kick",
      "file": "tracks/kick.wav",
      "sample_rate": 48000,
      "bit_depth": 24,
      "channels": 1,
      "pan": 0.0,
      "sync": {
        "search_start_sample": 0,
        "search_end_sample": 4320,
        "threshold_ratio": 0.2,
        "min_peak": 0.00001,
        "target_time_seconds": 0.02
      },
      "drift": {
        "tick_rate": 96000,
        "anchors": [
          {"source_sample": 0, "target_tick": 0},
          {"source_sample": 4320000, "target_tick": 8640000}
        ]
      }
    }
  ]
}
```

`file` 相对 manifest 所在目录解析。`id`（或 `name`）同时用于自动化曲线匹配。
没有任何漂移的轨道可省略 `drift`。`sync` 窗口和阈值可逐轨覆盖。

锚点把原始采样位置映射到母带时间刻度。两个锚点之间采用线性时间拉伸/压缩；
首锚点之前和末锚点之后继续使用各自最近锚点段的恒定斜率外推。输出帧数只由
工程时长与输出采样率决定，因此重采样和漂移补偿不会改变母带总帧数。

### `automation.json`

```json
{
  "tracks": {
    "kick": {
      "nodes": [[0.0, 0.0], [30.0, 0.0], [30.012, -12.0], [31.0, 0.0]]
    },
    "lead_vocal": {
      "nodes": [[0.0, -4.0], [60.0, 0.0], [180.0, 0.0]]
    }
  }
}
```

节点可写为 `[time_seconds, gain_db]` 或
`{"time_seconds": ..., "gain_db": ...}`。节点间线性插值，首节点前取首值，
末节点后取末值。所有轨道的曲线会在任何音频处理前一次性校验：时间必须
非递减、数值必须有限且增益位于配置范围内，否则整体拒绝。

当相邻节点间隔不超过 `smooth_max_interval_seconds` 且增益差达到
`smooth_min_gain_delta_db` 时，以 `smooth_duration_seconds` 为平滑过渡
时长，使用三次 smoothstep 替换该局部曲线。过渡窗口被裁剪到曲线首末节点
范围内；重叠窗口合并。报告同时给出原始急剧段数、合并后的区域数、区域
边界和总时长。

### `output_config.json`

```json
{
  "output": {
    "sample_rate": 48000,
    "channels": 2,
    "format": "pcm_s24",
    "true_peak_dbtp": -1.0,
    "target_lufs": -14.0,
    "duration_seconds": 180.0
  },
  "automation": {
    "gain_min_db": -96.0,
    "gain_max_db": 24.0,
    "smooth_max_interval_seconds": 0.03,
    "smooth_min_gain_delta_db": 6.0,
    "smooth_duration_seconds": 0.24
  },
  "limiter": {
    "lookahead_ms": 2.0,
    "release_ms": 45.0,
    "max_iterations": 8,
    "safety_margin_db": 0.12
  },
  "true_peak": {"oversample": 4}
}
```

`format` 支持 `pcm_s16`、`pcm_s24`、`float32`。`target_lufs` 可省略。
`duration_seconds` 可放在 `output`、manifest 的 `project` 或 manifest 顶层；
都未提供时使用最长轨道的标称时长。

## 处理设计

- **校验事务**：先统一解析 RIFF/`fmt `/`data` 块并交叉核对 manifest 声明；
  再校验全部曲线。所有处理完成且最终量化真峰值复核通过后才原子替换输出。
- **变速率重采样**：每个输出帧经锚点映射为原始采样坐标，采用双精度
  Catmull-Rom 四点插值；越界位置写零。每个轨道严格渲染到同一目标帧数。
- **同步对齐**：在原始采样率下计算多声道绝对包络，按峰值比例阈值检测脉冲
  前沿；映射到输出时间轴后取整并做保帧数整数平移。检测失败记录
  `DETECTION_FAILED`、偏移为 0、`aligned=false`，继续整张母带。
- **响度**：按 ITU-R BS.1770-4 设计 K 加权双二阶级联，使用 500 ms 截断
  冲激响应和分块 FFT 卷积，执行 400 ms 块 / 100 ms 步进、-70 LUFS 绝对门
  与相对 -10 LU 门。目标增益在限幅前施加并重新积分核验 ±0.5 LU。
- **真峰值限幅**：4 倍窗函数 sinc 过采样估计帧间峰值，滑动局部增益包络
  使用 2 ms lookahead 与 45 ms release；离线实现以非因果访问等效完成
  lookahead 延迟补偿，输出帧数不变。内部保留 0.12 dB 量化裕量，并在写出前
  对实际量化后的样本再次复核真峰值。
- **WAV**：PCM 16/24 位使用 `rint` 舍入后钳位，24 位显式组装小端 3 字节；
  float32 直接写 IEEE 小端。头部 `byte_rate`、`block_align`、`data` 长度、
  帧长在写后重新解析核验。

## 报告

报告包含：

- 每轨声明值与实际 WAV 头、输出帧数、pan；
- 漂移锚点、逐锚点映射误差、是否在 2 个输出样本内；
- 同步检测样本、目标/实际输出帧、应用偏移、对齐残差或失败告警；
- 自动化节点数、被平滑段数、合并区域与总平滑时长；
- 限幅前/校准后/最终响度、目标偏差；
- 限幅触发帧、最大衰减、lookahead/补偿样本、量化后真峰值；
- WAV 格式、头字段、帧数、data 字节数与 SHA-256；
- 最终验证布尔值和告警列表。

报告不含运行时间戳，同一输入与输出格式下 WAV 与数值结果均可重复。

## 业务错误码

文件/头错误：`FILE_NOT_FOUND`、`INVALID_WAV_HEADER`、
`SAMPLE_RATE_MISMATCH`、`BIT_DEPTH_MISMATCH`、
`CHANNEL_COUNT_MISMATCH`。

工程错误：`PROJECT_REJECTED`、`INVALID_MANIFEST`、
`INVALID_OUTPUT_CONFIG`、`INVALID_DRIFT_ANCHORS`。

曲线错误：`INVALID_AUTOMATION_CURVE`。

质量/输出错误：`DETECTION_FAILED`（告警，继续）、
`LOUDNESS_OUT_OF_TOLERANCE`（终止）、
`TRUE_PEAK_CEILING_VIOLATION`（终止）。

## 范围

输入要求为 RIFF/WAVE 的 16/24 位整数 PCM；输出支持 16/24 位 PCM 与 32 位
浮点。RIFF 的 4 GiB 上限由写出器检查。无外部网络、音频编解码器或 DSP
依赖。


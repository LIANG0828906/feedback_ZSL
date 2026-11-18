# 确定性离线多轨母带处理器

这是一个纯 Python、无网络、无容器的离线母带处理组件。运行时由代码确定性合成 11
路分轨、工程清单和断点标记，再完成校验、重采样、时钟漂移补偿、同步对齐、自动化
曲线平滑、混音、BS.1770 响度归一化、真峰值限幅与 WAV 导出。

## 环境与安装

- Python 3.10+
- NumPy、SciPy（`pip` 安装的稳定版本）
- 完整三分钟生成工程连同母带约 199.6 MB；首次生成约需 200 MB，覆盖已有母带时
  临时文件另需约 52 MB

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
source .venv/bin/activate
pip install -r requirements.txt
```

也可以不安装为包，直接在仓库根目录用 `python -m mastering ...` 运行。

## 快速运行

```bash
# 生成并处理完整三分钟演示工程
python -m mastering demo \
  --out-dir validation/full_project \
  --duration 180 \
  --sample-rate 48000 \
  --bits 24 \
  --target-loudness -16

# 独立复核母带 WAV 头、帧数、真峰值和积分响度
python -m mastering verify \
  --manifest validation/full_project/manifest.json \
  --master validation/full_project/master.wav
```

分步执行：

```bash
python -m mastering generate --out-dir my_project --duration 180 --seed 20261118
python -m mastering process \
  --manifest my_project/manifest.json \
  --master my_project/master.wav \
  --report my_project/report.json
```

`generate`/`demo` 支持 `--bits {16,24,32}`、单/双声道输出、任意目标响度或
`--no-loudness-target`。32 位输出为 IEEE float WAV，16/24 位为整数 PCM WAV。

## 工程清单

核心字段如下，完整示例见 `validation/full_project/manifest.json`。

```json
{
  "schema_version": 1,
  "project": {
    "name": "deterministic_indie_band",
    "seed": 20261118,
    "output_seconds": 180.0
  },
  "sync": {"reference_pulse_output_sample": 24000},
  "automation_config": {
    "gain_min_db": -48.0,
    "gain_max_db": 6.0,
    "jump_threshold_db": 3.0,
    "smoothing_seconds": 0.05
  },
  "output": {
    "sample_rate": 48000,
    "channels": 2,
    "bits_per_sample": 24,
    "true_peak_limit_dbtp": -1.0,
    "target_loudness_lufs": -16.0,
    "loudness_tolerance_lu": 0.5
  },
  "tracks": [
    {
      "name": "drums",
      "file": "tracks/drums_48000Hz_24bit.wav",
      "sample_rate": 48000,
      "bits_per_sample": 24,
      "channels": 1,
      "automation": [
        {"time": 0.0, "gain_db": -3.0},
        {"time": 20.0, "gain_db": -3.0},
        {"time": 20.005, "gain_db": 3.0}
      ],
      "drift_anchors": [
        {"output_sample": 480000, "source_sample": 480144},
        {"output_sample": 3040000, "source_sample": 3040912}
      ]
    }
  ]
}
```

`drift_anchors` 表示输出时间轴样本 `output_sample` 应对应源文件样本
`source_sample`。锚点内采用分段线性时间映射；首锚点之前和末锚点之后沿用各自端
段斜率，保持恒定速率。自动化曲线的首节点前取首值、末节点后取末值，节点时间允
许相等（表示瞬时跳变），但不得递减，增益必须位于配置范围。

## 处理顺序

1. **整体校验**：检查清单结构、曲线顺序/范围、锚点单调性、文件存在性、WAV
   实际头部的采样率/位深/声道数，以及与漂移锚点的源帧范围一致性。发现任一硬
   错误即拒绝整个工程，不启动音频处理。
2. **原采样率同步检测**：在前部搜索窗内检测同步脉冲的实际阈值起点，报告源域
   位置。检测失败记为 `DETECTION_FAILED` 质量告警，该轨以零偏移继续。
3. **统一采样率**：用有理数多相滤波重采样到输出采样率，内部全程 `float64`。
4. **锚点漂移补偿**：在输出采样率域按分段线性时钟映射做流式 Catmull-Rom 三次
   插值；锚点处映射精确，锚点外保持端段恒定速率。
5. **同步整数对齐**：在重采样/漂移后的时间轴上复核脉冲起点，四舍五入为整数
   偏移并移动波形，残差不超过 0.5 样本。首尾补零/截断，母带帧数固定。
6. **自动化与平滑**：一次性校验全部曲线；对“间隔短于平滑时长且增益跳变超过阈
   值”的段，以 smoothstep 在配置时长内过渡，窗口严格裁剪到曲线整体时间范围。
7. **混音与响度**：单声道复制、立体声转单声道取平均；按 BS.1770 的 K 加权、
   400 ms 分块、75% 重叠、绝对/相对门限计算积分响度。先算目标增益并复测，超
   出 `loudness_tolerance_lu` 时以 `LOUDNESS_OUT_OF_TOLERANCE` 终止。
8. **真峰值限幅**：以 4 倍过采样峰值包络驱动 5 ms look-ahead、50 ms release
   限幅，内部预留 0.02 dB 安全余量并做二次校正；净时间延迟为零，输出帧数不
   变。
9. **格式量化与原子写出**：16/24 位按负数满幅 `-2^(n-1)`、正数 `2^(n-1)-1`
   正确舍入并钳位；24 位按小端 3 字节组装。最终量化样本再次测真峰值，通过后
   才在同目录写临时文件并 `os.replace`，不会留下半截母带。

48 kHz 的 K 加权使用 ITU-R BS.1770-4 公布系数；其他输出采样率使用相同的
4 dB high-shelf 与 38 Hz high-pass 参数重新计算二阶节。报告同时记录限幅前目标
响应和限幅后最终响度，二者可能因较强限幅而差异明显。

## 报告、错误码与告警

成功报告包含每个轨道的源格式、检测到的同步位置、整数偏移与残差、漂移锚点数、
平滑段列表/总时长、自动化后峰值；另含混音峰值、响度、限幅触发样本数、最大衰减
量、最终真峰值及母带 WAV 字段。

主要业务码：

| 类型 | 代码 | 含义 |
|---|---|---|
| 硬错误 | `TRACK_FILE_NOT_FOUND` | 轨道文件不存在 |
| 硬错误 | `TRACK_READ_ERROR` | RIFF/WAV 头或数据不可解析 |
| 硬错误 | `SAMPLE_RATE_MISMATCH` | 声明与实际采样率不符 |
| 硬错误 | `BIT_DEPTH_MISMATCH` | 声明与实际位深不符 |
| 硬错误 | `CHANNEL_COUNT_MISMATCH` | 声明与实际声道数不符 |
| 硬错误 | `INVALID_AUTOMATION_CURVE` | 时间乱序、负时间或增益越界 |
| 硬错误 | `INVALID_DRIFT_ANCHOR` | 锚点非单调、越界或源位置超文件 |
| 硬错误 | `LOUDNESS_OUT_OF_TOLERANCE` | 响度复测偏差超容差 |
| 硬错误 | `TRUE_PEAK_LIMIT_EXCEEDED` | 量化后仍越过真峰值上限 |
| 质量告警 | `DETECTION_FAILED` | 同步脉冲检测失败，继续处理 |

`quality_warnings` 与 `hard_errors` 分开存放；出现硬错误时报告
`master_written: false`，且目标母带不会被本次运行创建。

## 测试

```bash
python -m pytest -q
```

测试覆盖：16/24/32 位 WAV 往返与头一致性、24 位小端组装、量化钳位、自动化插值
与平滑边界、漂移锚点映射与 2 样本内容对齐、同步检测/整数对齐、BS.1770 参考响
度、真峰值限幅、完整管线、同步检测失败继续、非法自动化/头部不符整体拒绝、无母
带保证、响度容差失败、输出逐字节确定性，以及含 11 轨生成工程的可重复性。

本次完整验收数据见 [VALIDATION.md](VALIDATION.md) 和
`validation/full_project/report.json`。

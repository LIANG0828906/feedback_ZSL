# mcSync Workbench

本地 8 通道、独立声卡、16-bit PCM WAV 的时基漂移同步工作台。实现覆盖：

- 多通道 WAV（单文件交织或逐通道目录）读取与确定性 16-bit PCM 写出
- 周期脉冲标记检测、置信度/SNR、`E_MARKER_MISSING` 精确定位
- 分段归一化互相关时延、峰值相关度、95% 不确定区间
- 时钟频偏与事件跳变的连续性/跳变诊断，未知时返回 `E_DRIFT_UNKNOWN`
- 稳健一阶漂移拟合、逐段预测/残差、拟合参数变化对比
- 线性插值与 Hann 窗 sinc 分数延迟重采样、模式对比和 `E_MODE_INVALID`
- v1/v2/v3 报告解析、升级来源标注、降级 `E_VERSION_LOSS` 字段清单
- 原子 WAV/JSON 导出、内容哈希、`E_VERSION_STALE` 参数版本校验
- 增量缓存、内容哈希校验、只读快照整体替换
- 按通道区间提交的恢复日志、SHA-256 哈希链、`E_RECOVERY_INVALID`
- 确定性合成样本、已知漂移真值、基线残差摘要、逐字节复现
- 可选 PySide6 桌面界面

详细设计见 [docs/TECHNICAL.md](docs/TECHNICAL.md)，三版示例见
[examples/README.md](examples/README.md)。

## 环境与安装

要求 Python 3.10+。本工作区实测环境：

- Python 3.14.4
- NumPy 2.5.5
- PySide6 6.12.0
- pytest 9.2.0

```bash
python3 -m pip install --user --break-system-packages -e '.[gui,test]'
```

也可以直接使用源码目录中的 `python3 -m mcsync ...`，无需安装。

## 快速开始

生成 8 通道、24 秒、含已知漂移和每 0.5 秒脉冲标记的样本：

```bash
python3 -m mcsync synth --out artifacts/demo --duration 24 --channels 8
```

运行全链路并导出 v1/v2/v3：

```bash
python3 -m mcsync analyze \
  --input artifacts/demo/sample_8ch.wav \
  --out artifacts/demo/analysis \
  --mode linear \
  --segment-seconds 1 \
  --marker-period-seconds 0.5 \
  --versions all
```

输出包括：

```text
artifacts/demo/analysis/
  compensated.wav
  report_v1.json
  report_v2.json
  report_v3.json
  diagnostics.md
  summary.json
```

运行本地验收：

```bash
python3 scripts/run_validation.py
python3 -m pytest -q
```

启动界面：

```bash
python3 -m mcsync gui
```

## Python API

```python
from mcsync import (
    CompensationConfig,
    MarkerConfig,
    PipelineConfig,
    SegmentationConfig,
    SyncPipeline,
    export_result,
    read_audio,
)

audio = read_audio("sample_8ch.wav")
config = PipelineConfig(
    reference_channel=0,
    marker=MarkerConfig(period_seconds=0.5, pulse_duration_samples=96),
    segmentation=SegmentationConfig(segment_length_samples=48000),
    compensation=CompensationConfig(mode="windowed_sinc", tolerance_samples=2.0),
)
result = SyncPipeline(config).analyze(audio)
export_result(result, "compensated.wav", "report_v3.json", target_version="v3")
```

增量发布：

```python
from mcsync import IncrementalSyncEngine

engine = IncrementalSyncEngine(audio, config)
outcome = engine.update_parameters(mode="linear")
print(outcome.reuse_report["recomputed_concat_chunks"])
snapshot = engine.snapshot()  # always one complete published revision
```

## 关键约定

时延符号为 `channel_arrival - reference_arrival`。一阶模型：

```text
delay_samples(t) = initial_offset_samples + slope * reference_sample
drift_ppm        = slope * 1_000_000
```

补偿时，输出参考样本 `m` 从通道输入位置
`initial_offset_samples + (1 + slope) * m` 重采样，因此正频偏通道不会在长素材中持续后移。

分段互相关在标记粗时延附近做精细搜索；无有效标记时才使用较大的全局搜索范围。
分段越长，平均漂移估计越稳但越难捕捉事件跳变；`segment_length_samples` 是精度与鲁棒性的显式取舍。

## 版本与错误

内部统一为 v3。载入 v1/v2 时会推导可推导字段，并在 `provenance` 标明：

- `v1:direct_marker_times`
- `v1:derived_from_marker_times`
- `v2:direct_segment_delays`
- `unavailable_in_v1` / `unavailable_in_v2`

降级文档包含：

```json
{
  "compatibility": {
    "code": "E_VERSION_LOSS",
    "source_version": "v3",
    "target_version": "v1",
    "lost_fields": ["segment_delays", "..."]
  }
}
```

稳定错误码：

| Code | Meaning |
|---|---|
| `E_MARKER_MISSING` | 标记缺失或 SNR/置信度不足，带通道和标记序号 |
| `E_VERSION_LOSS` | 降级无法表达的字段清单 |
| `E_VERSION_INVALID` | 文档结构、版本或内容哈希无效 |
| `E_VERSION_STALE` | 报告、补偿参数或发布结果参数版本不一致 |
| `E_DRIFT_UNKNOWN` | 无法可靠区分频偏/跳变，附候选解释 |
| `E_RESIDUAL_THRESHOLD` | 拟合残差超阈值并定位最大残差段 |
| `E_MODE_INVALID` | 非法插值模式 |
| `E_RECOVERY_INVALID` | 恢复记录断链、缺字段、重叠或块哈希错误 |
| `E_WAV_INVALID` | WAV 格式或 PCM 数据错误 |
| `E_CHANNEL_INVALID` | 通道或参考通道越界 |

## 实测结果

`scripts/run_validation.py` 使用 8 通道、48 kHz、8 秒、种子 `20261122` 的本地样本，结果为：

- 128/128 个标记全部检测成功，无缺失。
- 已知漂移为 `0, 120, -95, 180, -150, 60, -35, 210 ppm`。
- 最大漂移估计误差 `0.841 ppm`。
- 线性补偿最大标记偏差 `1.018` 样本，容差 2 样本。
- 窗 sinc 与线性模式均完整输出；本样本窗 sinc 最大偏差更小约 `0.033` 样本。
- v1/v2/v3 均可导出并重新载入，v3 内容哈希校验通过。
- 模拟中断后，6 个已提交区间被跳过，26 个未完成区间续算；结果与直接补偿逐样本一致。
- 相同种子和操作序列下，补偿 WAV 与 v3 报告逐字节一致。
- 注入缺失标记后，准确报告 `channel=1, marker_index=2`。
- 注入 120 样本事件跳变后，诊断结论为 `event_jump`，证据跳变段为 2。

完整机器可读结果见 `artifacts/validation/validation_summary.json`。

## 测试

```text
25 passed
```

覆盖标记检测/缺失、WAV round-trip、漂移拟合、补偿容差、事件跳变/未知诊断、
非法插值模式、v1/v2/v3 互转、原子导出与版本过期、增量复用、恢复哈希链、
恢复重叠写入拒绝、确定性样本与逐字节复现。


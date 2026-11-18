# 验收记录

以下数据均来自仓库内确定性生成的工程，不依赖外部音频或网络。

## 环境

- Python 3.14.4
- NumPy 2.5.5
- SciPy 1.18.1
- 操作系统：Linux（实现只使用标准库、NumPy 与 SciPy，路径和临时文件写出兼容
  Windows 10+）

## 自动测试

```text
22 passed
```

对应命令：

```bash
python -m pytest -q
```

## 三分钟完整工程

命令：

```bash
python -m mastering demo \
  --out-dir validation/full_project \
  --duration 180 \
  --sample-rate 48000 \
  --bits 24 \
  --target-loudness -16
```

生成并处理结果：

- 11 轨，源采样率覆盖 44.1 kHz、48 kHz、96 kHz；源位深覆盖 16/24 位 PCM
- 鼓、贝斯为完整 180 秒并带 `+300 ppm` / `-300 ppm` 时钟漂移
- 工程总占用 199,592,751 字节（191 MiB），低于 200 MB 预算
- 母带：48 kHz、双声道、24 位 PCM、8,640,000 帧、51,840,000 数据字节
- 最终真峰值：`-1.0200001632769229 dBTP`，低于 `-1.0 dBTP`
- 限幅触发 14,426 个输入帧，最大衰减 `-0.9205171573441826 dB`
- 限幅 look-ahead 240 样本；净延迟 0；输入/输出均为 8,640,000 帧
- 目标 `-16 LUFS`，限幅前复测 `-15.999999999999604 LUFS`，偏差
  `3.96e-13 LU`
- 最终积分响度 `-16.00101087929478 LUFS`
- 每轨 3 个平滑段，总计 `0.15 s`
- 同步残差绝对值最大 `0.39774` 样本；漂移锚点映射误差为 0 样本
- 无硬错误、无质量告警
- 母带 SHA-256：
  `77b94cd50d1418880cb9cfb9f5e0749e8cb7a436b5d570a0566c0c49030c56bc`

完整处理实际耗时约 25.06 秒（包含生成 11 路素材），峰值常驻内存约 1.41 GB。

独立复核：

```bash
python -m mastering verify \
  --manifest validation/full_project/manifest.json \
  --master validation/full_project/master.wav
```

输出摘要：

```json
{
  "header": {
    "bits_per_sample": 24,
    "channels": 2,
    "data_bytes": 51840000,
    "frames": 8640000,
    "sample_rate": 48000,
    "self_consistent": true
  },
  "integrated_loudness_lufs": -16.00101087929478,
  "status": "ok",
  "true_peak_dbtp": -1.0200001632769229,
  "true_peak_limit_dbtp": -1.0,
  "true_peak_ok": true
}
```

## 逐字节确定性

将同一工程独立处理到临时母带后比较：

```text
77b94cd50d1418880cb9cfb9f5e0749e8cb7a436b5d570a0566c0c49030c56bc  validation/full_project/master.wav
77b94cd50d1418880cb9cfb9f5e0749e8cb7a436b5d570a0566c0c49030c56bc  /tmp/full_master_repeat.wav
BYTE_IDENTICAL
```

短工程另外验证了 16 位整数 PCM、32 位 IEEE float，以及 44.1 kHz 输出：均通过
独立 WAV 头/帧数/真峰值复核；44.1 kHz 场景的同步残差绝对值仍小于 0.5 样本。

## 异常路径

自动测试实际确认：

- 采样率、位深、声道数不符和文件缺失会一次性汇总，报告含轨道名与对应业务码，
  目标母带不存在。
- 乱序或越界自动化曲线在任何音频处理前以 `INVALID_AUTOMATION_CURVE` 拒绝。
- 静音轨触发 `DETECTION_FAILED` 质量告警但仍继续，成功报告与硬错误分离。
- 响度复测超容差时以 `LOUDNESS_OUT_OF_TOLERANCE` 终止，报告保留实际测量值，
  不写母带。

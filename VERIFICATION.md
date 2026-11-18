# 实际验证记录

验证日期：2026-11-18

环境：

- Python `3.11.18`
- NumPy `2.4.6`
- 本地离线 WAV 合成，无外部媒体、网络或音频设备

## 自动化测试

命令：

```bash
.venv/bin/python -m unittest discover -s tests -v
```

结果：

```text
Ran 9 tests in 12.198s
OK
```

覆盖 2 个 DSP 基准测试和 7 个工程级测试，包括确定性合成、结构化校验错误、全链路合规、真实（测试钩子）强制退出续算、Ctrl+C 恢复、快照失配和素材时长越界。

## 示例工程完整渲染

命令：

```bash
.venv/bin/python -m mastering.cli synthesize --manifest manifest.example.json
.venv/bin/python -m mastering.cli render --manifest manifest.example.json --fresh
```

关键实测值：

| 指标 | 结果 |
| --- | --- |
| 母带采样数 | `307200`（48 kHz，6.4 s） |
| 分块数/快照提交 | `75 / 75` |
| 最大对白对齐误差 | `0 sample` |
| 对白 A/B、解说整数平移 | `-7 / +5 / +3 sample` |
| 折叠最大能量占比偏差 | `3.3976e-05`（容差 `0.015`） |
| 闪避区间数 | `3` |
| SIR 提升 | `7.433374 / 8.235882 / 7.254399 dB`（要求 `7.0 dB`） |
| 积分响度 | `-17.000510 LUFS`（目标 `-17.0 ± 0.5`） |
| 立体声真峰值 | `-1.089941 dBTP`（上限 `-1.0`） |
| 单声道真峰值 | `-1.145518 dBTP`（上限 `-1.0`） |
| 限幅触发样本 | `241` |

## 中断恢复实测

1. 真实 `SIGKILL`：进程在第 6 块提交后收到 `SIGKILL`，退出码 `-9`；快照保持 `completed_blocks=6`。随后 `--resume` 完成 75 块。
2. 真实 `SIGINT`：进程在第 4 块提交后被中断，当前块继续完成并提交为第 5 块；退出码 `0`，生成 `recovery_report.json`，其中 `completed_blocks=5`。
3. 两种恢复生成的 `master_stereo.wav` 与 `master_mono.wav` 均与一次性 `--fresh` 渲染通过 `cmp` 逐字节比较一致。
4. 修改清单目标响度后执行 `--resume`，程序删除旧快照、返回退出码 `3` 和 `E_RESUME_MISMATCH`，未沿用旧块。

## 示例产物 SHA-256

```text
5cb9c8a4d4495672819b63e16d1170df043a9c18a9af0b90862ad1710621da28  media/dialogue_a.wav
0f8e9ed784a1fab7e69d65b4db1f16a85cf95da767ecb67e55e6a8f3a0d99889  media/dialogue_b.wav
32789734c306a94dbb37ee102e7e87a1cb903178635bf63ed3fb82100b24c658  media/music_bed.wav
5d75e840a103f0a1e0246f7d3096d6f66c8beac9069f956a5d7fba3c2d77b18d  media/narration.wav
16578be7d1113e9fb90a71768928a4c4c939027a8f4c53f7e19d3593dbece21a  deliverables/master_stereo.wav
15b9d1c4eac969984c1e5397252b4d48c33f5f4fb953cc2e5366ba9b0a992e07  deliverables/master_mono.wav
c7a14c86bc83058d35f84b62e8a9deffa2233dbde7c7dc30cd0d64485256ed5f  deliverables/mastering_report.json
41e2911668e0d87067dc5f9503ad290898f6df31f24b4258a618029d6e8dba38  state/render.snapshot.json
```

清单摘要：`a20ed8304d4a6c3af32678203c494e616d92e6235ba16b819491c025998f0b98`

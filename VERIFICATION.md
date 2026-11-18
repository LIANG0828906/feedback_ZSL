# 实际验证记录

验证日期：2026-11-18  
环境：Windows 兼容代码路径；本次实际运行于 Linux，Python 3.14.4，NumPy 2.5.5。
代码未使用 NumPy 2 专属 API，依赖声明为 `numpy>=1.21`，适用于 Python 3.10+ /
NumPy 1.x。

## 1. 自动化测试

命令：

```bash
python -m unittest discover -s tests -v
```

结果：

```text
Ran 13 tests in 36.056s

OK
```

覆盖并通过的断言：

- 1 kHz 立体声参考正弦的 BS.1770 响度与参考值 `-20.252837 LUFS` 误差小于 `0.01 LU`；
- 4 倍过采样真峰值高于样本峰值，且差异落在合理范围；
- 前视限幅输出真峰值不越过上限、帧数不变、增益包络无阶跃；
- 对白闪避达到 12 dB、存在有效闪避区、曲线逐样本连续；
- 仅裁剪尾部静音，尾部存在可听内容时拒绝裁剪；
- 三语种母带均有相同总帧数、目标响度误差不超过 `0.5 LU`、真峰值不超过 `-1 dBTP`；
- WAV 头采样率、声道数、位深、帧数和 data 长度自洽；
- 报告 SHA-256 与文件一致，`mastering verify` 可从最终 WAV 复算响度/真峰值；
- 同一工程成功后再次完整运行，三条 WAV 与 `mix_report.json` 逐字节一致；
- 强制终止后恢复，三条 WAV 与 `mix_report.json` 和一次性完整处理逐字节一致；
- Ctrl+C 模拟中断退出码为 `0`，恢复报告为 `interrupted`，后续恢复成功；
- 快照 JSON 损坏后整体丢弃，记录丢弃版本并从 0 开始；
- 工程摘要变化后整体丢弃，记录丢弃版本并重新分析；
- 静音语音轨产生 `SILENT_TRACK` 告警，增益固定为 `0 dB`，不放大底噪；
- 缺失文件、采样率不符、位深不符、声道数不符、重复语种、缺失语种均以非零码拒绝，
  且没有生成任何 `master_*.wav`。

## 2. 默认 6 秒三语种演示

命令：

```bash
python -m mastering demo --workdir /tmp/uid211-final-demo
python -m mastering verify --report /tmp/uid211-final-demo/output/mix_report.json
```

母带规格：48 kHz、立体声、24-bit PCM、每轨 288000 帧（6.0 秒）。

| 语种 | 补齐/裁剪采样 | 母带响度 (LUFS) | 目标 (LUFS) | 真峰值 (dBTP) | 上限 (dBTP) | 限幅触发采样 | 限幅最大衰减 | 音乐总线闪避 | 闪避深度 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| en | +79200 / 0 | -23.014116 | -23.0 | -1.080000 | -1.0 | 20936 | 1.206682 dB | 4.994500 s | 12.0 dB |
| ja | 0 / 16800 | -24.000000 | -24.0 | -1.357644 | -1.0 | 0 | 0.000000 dB | 5.998437 s | 12.0 dB |
| de | +40800 / 0 | -23.471424 | -23.5 | -1.080000 | -1.0 | 7343 | 0.049068 dB | 5.784500 s | 12.0 dB |

日本语轨原始长度长于音乐，报告证明只裁剪了 16800 个尾部静音采样；英语和德语轨
分别补齐 79200 和 40800 个静音采样。

独立复核输出：

```json
{"status":"ok","checks":[
  {"language":"en","ok":true,"loudness_lufs":-23.014116,"true_peak_dbtp":-1.08,"problems":[]},
  {"language":"ja","ok":true,"loudness_lufs":-24.0,"true_peak_dbtp":-1.357644,"problems":[]},
  {"language":"de","ok":true,"loudness_lufs":-23.471424,"true_peak_dbtp":-1.08,"problems":[]}
]}
```

母带 SHA-256：

```text
en  524dc9959dd022e247274d936f11cba34c4548ecb2f2445eb24f576838448b4c
ja  60d500cb21e42600ba0505b6e1eb3423d6c1680347202bde4ee7782e183a0ea5
de  b6963c299fde2d877a2f98c001b073aa82e35b9299d0af51e8dc55da6357b3ab
```

完成后的恢复报告：

```json
{
  "schema": "uid211.mastering.recovery.v1",
  "status": "completed",
  "recovery_count": 0,
  "discarded_snapshot_versions": [],
  "completed_blocks": 60,
  "total_blocks": 60,
  "total_frames": 288000,
  "outputs": ["master_en.wav", "master_ja.wav", "master_de.wav"]
}
```

## 3. 恢复一致性证据

自动化测试对同一确定性工程执行以下对照：

1. 一次性完整处理，保存三条母带和报告字节；
2. 在第 2 个固定块提交快照后模拟 `SIGKILL`，进程以强制终止码退出；
3. 无人工清理，直接重新启动；
4. 逐文件比较恢复结果与一次性结果。

三条 WAV 和 `mix_report.json` 均逐字节相等。Ctrl+C 测试同样在第 2 块后退出，
保留已提交快照、写出 `recovery_report.json` 并以 `0` 退出；再次启动后与一次性结果
逐字节一致。

快照损坏和工程摘要失配测试均确认丢弃整个状态目录，恢复报告中包含被丢弃的快照版本，
并从第 0 块重新处理。

额外使用真实 `SIGINT`（而非注入开关）在分析阶段向子进程发送中断信号，观察到：

```text
exit code: 0
stdout status: interrupted
recovery_report.status: interrupted
completed_blocks: 1
```

即信号到达后仍完成并提交了当前块，然后以 0 退出；中断报告已写出。


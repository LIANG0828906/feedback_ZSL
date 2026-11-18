# 实际验证记录

## 环境

- 本地 Python 虚拟环境，Python 3.12
- numpy 2.5.5
- scipy 1.18.1
- soundfile 0.14.0
- 无 Docker、无外部媒体、无网络素材

代码避免 POSIX-only 文件 API；Windows 使用相同 CLI。本记录的执行主机为当前 Linux 工作区，Windows 实机未另行复测。

## 自动化测试

执行：

```bash
.venv/bin/python -m pytest -q
```

结果：

```text
.............                                                            [100%]
13 passed
```

覆盖点：

- 相同种子两次生成：八路 WAV 与清单逐字节一致。
- 两路麦克风共享源、增益和延迟关系可复算。
- 缺失文件、采样率、位深、声道数、非法/缺失声部、比例和、频带倒置。
- 检测 RMSE、未突变声部无误报、补偿改善量。
- 检测区间外男高与女低样本逐位不变。
- 声部比例偏差、积分响度、真峰值。
- 16-bit PCM 输出。
- 两次完整渲染 WAV 与报告逐字节一致。
- SIGKILL 后续跑 WAV 与一次性渲染逐字节一致。
- Ctrl+C 当前块提交、恢复报告、退出码 0。
- 清单变化和快照分析结果变化均触发 `E_RESUME_MISMATCH` 并删除旧进度。

## 默认真实样例

命令：

```bash
python -m choir_master init --workspace test-output/final --seed 235 --duration 8
python -m choir_master render \
  --manifest test-output/final/project.json \
  --output test-output/final/master.wav \
  --report test-output/final/report.json \
  --snapshot test-output/final/master.snapshot \
  --block-frames 24000 --fresh
```

关键结果：

| 项目 | 结果 |
|---|---|
| 生成真值区间 | `2.840000–3.640000 s` |
| 检测区间 | `2.843833–3.633833 s` |
| 起止 RMSE | `5.134 ms`（要求 ≤ 50 ms） |
| 男高突变频带 | `400–1600 Hz` |
| 补偿前频带落差 | `17.991375 dB` |
| 补偿后频带落差 | `0.040437 dB` |
| 改善量 | `17.950938 dB`（要求 ≥ 8 dB） |
| 最大补偿修改幅度 | `0.423648119109` |
| 未突变声部误报 | `0` |
| 声部目标/实际比例 | 女高 27/27%，女低 25/25%，男高 26/26%，男低 22/22% |
| 最大比例绝对偏差 | `0.000000`（要求 ≤ 0.02） |
| 积分响度 | `-20.000000 LUFS`（目标 -20.0，偏差 0） |
| 真峰值 | `-8.474278 dBTP`（上限 -1.0） |
| 限幅触发样本 | `0` |
| 母带帧数 | `384000`，等于最长输入 |
| 输出格式 | 48 kHz / stereo / PCM_24 |
| 母带 SHA-256 | `ea3b4e0d963406a1fec20c1256c96545e5535c16056d09c36c4e5b579c8af4a2` |
| 报告 SHA-256 | `e36a814c0bd427ecfa5600eefed737fd30e120a2b32cfea5f2010c1325964bf3` |

## 限幅场景

将同一工程目标改为 `-12.0 LUFS` 后渲染：

| 项目 | 结果 |
|---|---|
| 积分响度 | `-12.021306 LUFS` |
| 真峰值 | `-1.511880 dBTP` |
| 限幅启用 | `true` |
| 限幅触发样本 | `12014` |
| 最大声部比例偏差 | `0.00084684` |
| 母带帧数 | `384000`，未因限幅改变 |

## 恢复验证

- 强杀测试在至少 3 个块已提交后发送强制终止。
- 续跑报告 `recovery_count = 1`，WAV SHA-256 与一次性完整渲染完全相同。
- Ctrl+C 测试确认退出码为 `0`，主报告和 `*.recovery.json` 均为 `status: "interrupted"`，快照块数与报告一致。
- 修改清单目标后复用旧快照：退出码 `3`，错误码 `E_RESUME_MISMATCH`，快照与旧输出被删除。
- 手工篡改快照中的已确认换声点起始时刻后复用：同样退出码 `3`，确认结果不一致时旧进度被整体丢弃。


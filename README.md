# 古琴多轨离线母带组件

这是一个从零构建的确定性离线母带渲染组件。它读取场记清单与多通道
WAV，完成串扰解耦、撮音瞬态对齐、分段立体声合成、BS.1770 响度/真峰值
合规检查，以及可持久化续算的固定块渲染。仓库内不依赖任何外部音频或
网络素材；六通道古琴样例由代码按整数种子生成。

## 快速开始

环境要求：Python 3.10+、NumPy、SciPy。

```powershell
py -3 -m pip install -r requirements.txt
py -3 -m guqin_master generate-demo --out demo_project
py -3 -m guqin_master run --project demo_project\manifest.json --out rendered
```

也可以用脚本入口：

```powershell
py -3 guqin_master.py generate-demo --out demo_project
py -3 guqin_master.py run --project demo_project\manifest.json --out rendered
```

成功渲染后主要产物：

- `rendered/master.wav`：清单位深、清单采样率的双声道母带。
- `rendered/master_report.json`：稳定的机器可读验收报告。
- `rendered/state/progress.json`：最近一次已提交块的快照。
- `rendered/state/master.partial.f64`：已提交块的交错 float64 L/R 中间母带。
- `rendered/state/recovery_report.json`：仅在可处理中断后存在。

比较两份母带是否逐字节一致：

```powershell
py -3 -m guqin_master compare --left rendered\master.wav --right resumed\master.wav
```

## 清单结构

`generate-demo` 会写出一个完整六通道样例。清单的关键字段如下：

```json
{
  "schema_version": 1,
  "project_id": "deterministic_guqin_six_channel",
  "seed": 20261118,
  "sample_rate": 48000,
  "bit_depth": 24,
  "duration_seconds": 10.0,
  "reference_times_seconds": [0.9, 2.1, 3.3, 4.7, 6.0, 7.2, 8.5],
  "target_loudness_lufs": -16.0,
  "true_peak_limit_dbtp": -1.0,
  "block_size": 4096,
  "crossfade_seconds": 0.06,
  "channels": [
    {
      "name": "ch1_C2",
      "path": "audio/ch1_C2.wav",
      "string": 1,
      "pitch": "C2",
      "pan": -0.78,
      "start_offset_samples": 0,
      "reference": false
    }
  ],
  "crosstalk": [
    {
      "source": "ch1_C2",
      "target": "ch2_D2",
      "window_seconds": [0.86, 1.66],
      "gain": 0.135
    }
  ],
  "decoupling": {
    "residual_threshold_db": -50.0,
    "minimum_attenuation_db": 18.0,
    "detection_margin_db": 12.0,
    "max_unrelated_peak_loss_db": 1.0
  },
  "segments": [
    {
      "name": "intro",
      "start_seconds": 0.0,
      "end_seconds": 3.4,
      "gains": {"ch1_C2": 0.78}
    }
  ]
}
```

约束与校验：

- 每路 WAV 必须是单声道 PCM 16/24/32-bit，采样率、位深必须与清单一致。
- 通道名、琴弦编号、音高均不可重复；音高按十二平均律比较。
- `reference_times_seconds` 必须严格升序且落在时长内。
- 恰好一个 `reference: true` 的泛音参考通道；它不得作为串扰目标。
- 段落必须从 0 开始、连续覆盖到 `duration_seconds`，且为每个通道声明增益。
- 文件缺失、格式不符或清单非法时，输出含 `business_code`、`channel`、`message`
  的 JSON，退出码为 2，且不写母带。

## 处理口径

### 确定性素材

所有随机数均由 `seed`、通道和事件标签经过 SHA-256 派生 PCG64 生成器产生。
每根琴弦包含散音、按音和泛音，并有一个确定性的短拨弦瞬态；清单声明的
通道起始偏移直接作用于样本时间轴。低音弦按 `crosstalk` 声明，在目标通道
的指定时间窗注入同频基波与二次谐波。注入集合、增益和时间仅由清单复算。

### 共振解耦

1. 对参考泛音通道的一阶差分取 MAD，稳健估计噪声本底。
2. 在每条串扰的目标通道、指定时间窗内，对源琴弦基频与二次谐波做最小二乘
   正弦拟合。
3. 仅保留超过 `max(residual_threshold_db, noise_floor + detection_margin_db)`
   的分量，并在窗口内扣除；窗口首尾各 1 ms 使用线性保护斜坡。
4. 窗口外样本保持逐位不变，未命中的通道保持逐位不变。报告给出分量扣除量、
   残余 dBFS、非串扰峰值损失和每个被修改样本数。

### 撮音对齐

对每个参考时刻，在 `±max(100 ms, 声明偏移 + 25 ms)` 内寻找第一个超过
稳健差分门限的拨弦瞬态。各参考点的检测偏移取中位数，形成整数采样平移；
平移后每个参考瞬态残差必须不超过 1 采样。检测不到或无法满足残差时抛出
`E_DETECTION_FAILED`，不得静默跳过。

### 母带与合规

- 每个段落按该段各通道增益形成时间包络；边界使用清单长度的升余弦交叉淡化。
- 等功率声像合成立体声，母带样本数严格等于清单全时长对应样本数。
- 积分响度实现 ITU-R BS.1770 K-weighting、400 ms 块、100 ms 跳步、绝对
  -70 LUFS 门限和相对 -10 LU 门限。
- 真峰值使用 4 倍多相重采样测量。
- 需要时使用无输出延迟的前瞻增益包络限幅，保留总采样数；报告记录触发样本数、
  最大增益衰减和 `processing_delay_samples: 0`。

## 快照与恢复

渲染以固定 `block_size` 推进。每块流程为：

1. 计算交错 float64 L/R 块并写入 `master.partial.f64`。
2. `flush` + `fsync` 数据。
3. 写入该块 L/R SHA-256，更新 `completed_blocks` 和 `committed_samples`。
4. 原子替换 `progress.json`。

因此快照永远只引用已经落盘的数据。快照包含清单摘要、输入 WAV 摘要、完整解耦
参数、整数对齐偏移、块大小、总块数以及已提交块哈希。

- `Ctrl+C`：SIGINT 处理只设置停止标志，当前块仍会完成并提交，然后写
  `state/recovery_report.json`，退出码 0。
- 强制终止：不保证恢复报告；下次启动从最近已提交快照继续。
- 清单摘要、输入摘要、渲染几何或已确定参数不一致：删除旧快照与旧中间数据，
  输出 `E_RESUME_MISMATCH`，退出码 4。
- 已提交中间数据与块哈希不一致：输出含 `block_index`、`sample_range`、
  `channel` 的 `E_RESUME_DIFF`，退出码 5。
- 完整渲染完成后，续算结果与一次性结果必须逐字节相同；`compare` 也会给出
  结构化差异。

进程级测试可设置以下诊断环境变量：

- `GUQIN_TEST_INTERRUPT_AFTER_BLOCKS=N`：在第 N 块提交后模拟可处理中断。
- `GUQIN_TEST_BLOCK_SLEEP_MS=N`：每块提交后暂停 N ms，用于实际信号测试。

## 错误码

| 业务码 | 退出码 | 含义 |
| --- | ---: | --- |
| `OK` | 0 | 完成，或 Ctrl+C 已提交当前块并写出恢复报告 |
| `E_INVALID_PROJECT` | 2 | 清单或 WAV 校验失败 |
| `E_DETECTION_FAILED` | 3 | 瞬态检测或对齐失败，含通道名 |
| `E_RESUME_MISMATCH` | 4 | 快照与当前输入/参数不一致，旧进度已丢弃 |
| `E_RESUME_DIFF` | 5 | 已提交块或恢复母带与基准不一致 |
| `E_IO` | 6 | 未预期的 I/O 或渲染失败 |

所有成功/错误摘要使用稳定排序 JSON；报告不含时间戳、耗时或绝对路径。

## 测试与实际验证

```powershell
py -3 -m unittest discover -v
```

本次在 Python 3.14.4、NumPy 2.5.5、SciPy 1.18.1 环境实测：

- 9 项自动化测试全部通过（约 24 秒）。
- 六通道样例：480,000 采样，24-bit PCM，立体声。
- 积分响度：`-16.0493 LUFS`，目标 `-16.0 LUFS`，偏差 `-0.0493 LU`。
- 真峰值：`-1.0500 dBTP`，上限 `-1.0 dBTP`。
- 三条串扰的基波扣除量均约 `265–267 dB`；窗口外和未命中通道逐位不变；
  非串扰峰值损失 `0.0 dB`。
- 六通道整数对齐残差最大为 `1` 采样。
- 三次独立完整渲染的 `master.wav` 与 `master_report.json` 均逐字节一致。
- 实际 SIGINT：第 4 块提交后退出 0，恢复报告存在，母带未提前生成。
- 实际 SIGKILL：退出 -9，无恢复报告，快照记录第 13 块；重启完成后的母带
  与 SIGINT 恢复结果、全新一次性渲染结果均逐字节一致。

当前验证环境为 Linux/WSL；实现只使用 Python 标准库、NumPy、SciPy 和跨平台
文件 API，未使用容器或平台专属音频服务。

## 已生成交付物

仓库内保留了本次确定性验收产物：

- `deliverables/project/manifest.json` 与 `deliverables/project/audio/*.wav`：
  可由种子重复生成的六通道素材。
- `deliverables/rendered/master.wav`：可交付的 24-bit/48 kHz 立体声母带。
- `deliverables/rendered/master_report.json`：逐项核对报告。
- `deliverables/rendered/state/progress.json`：完整渲染后的块快照。
- `deliverables/verification_summary.json`：测试结论、关键指标和 SHA-256。

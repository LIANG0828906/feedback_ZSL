# 技术说明：多轨掌声母带确定性管线

## 1. 工程契约与整体拒绝

`project.json` 声明采样率、话筒（名称、WAV 相对路径、声道数、三维位置）、
检测门限、频段划分（至少三段、连续不重叠、不超过奈奎斯特频率）、目标积分
响度、真峰值上限、能量门限、本底噪声增益与渲染块长。`project.load_project`
在任何渲染前完成全部校验，错误以 `BusinessError(code, message, details)`
抛出，CLI 打印结构化 JSON 并以非零退出码结束，且不写出母带：

| 业务码 | 触发条件 |
| --- | --- |
| `FILE_MISSING` | 工程文件或话筒 WAV 不存在 |
| `SAMPLE_RATE_MISMATCH` | WAV 实际采样率与声明不符 |
| `CHANNEL_COUNT_MISMATCH` | WAV 实际声道数与声明不符 |
| `DUPLICATE_MIC_NAME` | 话筒名称重复 |
| `INVALID_FREQ_BAND` | 频段少于 3 个、边界倒置、越界或重叠/留缝 |
| `INVALID_PROJECT` | 标量字段缺失或类型错误 |
| `UNSUPPORTED_SAMPLE_RATE` | 非 48 kHz（BS.1770 K 加权系数按 48 kHz 发布） |
| `E_RESUME_MISMATCH` | 快照摘要与当前工程/事件清单不一致 |

## 2. 确定性合成

`synth.derive_schedule(seed, duration, sample_rate, microphones)` 仅依赖整数
种子与话筒位置即可复算全部真值：

- 事件数量（4-7）、起止时刻、每个掌声脉冲（40-90 ms 间隔）来自
  `numpy.random.SeedSequence` 的命名流 `events`，独立流互不串扰。
- 每个掌声是 3-6 ms 指数衰减的宽带噪声 + 1.8-3.4 kHz 振铃的短促脉冲串。
- 话筒增益 `1/(1+0.12d)`、传播延迟 `d/343`（窗口 sinc 分数延迟）与位置
  相关相位倾斜全部由三维位置决定。
- 距舞台最近的两支话筒叠加 70-150 Hz 脚步瞬态（能量集中在低频）。
- 各话筒独立的本底噪声流（RMS 0.003），写出 16-bit PCM WAV。

同参数重跑逐字节一致（`test_synthesis_is_byte_identical`），真值来源记录在
报告 `ground_truth.provenance`，并含每话筒距离/延迟/增益供核对。

## 3. 事件检测

`detect.detect_events` 对每支话筒做零相位 500 Hz 高通（脚步能量集中在
300 Hz 以下，因此不会产生虚警），以 5 ms 帧计算 RMS 并与工程门限比较；
帧运行区间先按 100 ms 桥接（掌声脉冲间隔 40-90 ms，独立事件间隔 >= 900 ms）、
最短事件 150 ms，再跨话筒取并集并再次合并。输出含序号、起止、时长的全局
清单。真值事件首脉冲紧贴起点、末脉冲距终点 20 ms，使能量法检测无结构性
偏差；实测边界 RMSE 约 14-19 ms（要求 <= 20 ms），计数差为 0。

## 4. 频段能量配比

`bands.compute_ratios` 对每个事件、每个 Butterworth 带通频段计算各话筒
窗内能量 `sum(x^2)`；低于 `energy_gate * 最强能量` 的话筒标记
`gated_microphones` 且占比为 0，其余按能量归一化，并用一次余数修正保证
占比和恰为 1.0（容差 1e-6）。话筒名显式排序，结果与工程内排列顺序无关
（`test_band_ratios_sum_to_one_and_are_order_independent`）。整段全部被门
限时回退为等权并置 `fallback_equal_weights=true`。

## 5. 分块母带渲染与可复算性

`render.render_master` 以 `block_seconds` 固定块长流式处理：每话筒每频段
维护 `sosfilt` 流式状态，按事件权重（含 5 ms 等功率淡入淡出，杜绝阶跃）
把带通成分加权到立体声；位置角度决定等功率声像。非事件时段以
`noise_gain_db` 保留环境底噪。母带采样数等于最长话筒轨。报告中的事件清单
与配比表即渲染所用的逐样本权重来源，可逐项复算。

## 6. 响度与真峰值合规

`loudness.integrated_loudness` 实现 BS.1770-4：两级 K 加权（48 kHz 系数）、
400 ms/75% 重叠块、-70 LUFS 绝对门限与 -10 LU 相对门限。
`loudness.true_peak` 以 `resample_poly` 4 倍过采样取峰值（dBTP）。
`true_peak_limit` 在 4 倍域计算安全增益，经 0.2 ms 起控/15 ms 释放的一极点
平滑（无阶跃）后降采样回原速率，采样总数不变；记录触发样本数、最大衰减量
与限幅前积分响度。归一化循环在 TP 上限内迭代补偿，使最终积分响度落在目标
±0.5 LU，实测约 -23.3 LUFS / -1.0 dBTP。

## 7. 快照与恢复语义

每块完成后通过临时文件 + `os.replace` 原子提交 `snapshot.json`，含输入摘要
（工程 JSON 与全部 WAV 字节的 SHA-256）、已完成块数、事件清单、配比表与
块长。重启时：

- 摘要/事件/采样数任一不一致 -> 删除旧快照与旧母带、抛 `E_RESUME_MISMATCH`
  并从零重算，报告置 `resume_mismatch_restart=true`，绝不沿用旧偏移。
- 一致则按已提交块数快速重建滤波状态后续算。
- Ctrl+C（SIGINT）由 CLI 转为协作标志：当前块写完、快照提交、输出
  `resume_report.json` 并以退出码 0 结束。
- SIGKILL 无退出报告；下次启动仅凭快照恢复，母带与一次性渲染逐字节一致
  （`test_sigkill_then_resume_is_byte_identical`）。

## 8. 稳定报告与确定性

报告字段顺序固定、无时间戳、无绝对路径（仅文件名/话筒名/频段名）。浮点
WAV 经 `wavio.strip_peak_chunk` 剔除 libsndfile 非稳定的 PEAK 块，保证相同
样本写出逐字节一致，报告 `master_sha256` 为最终母带 SHA-256。

# 技术说明

## 1. 数据与计算契约

所有跨线程、跨进程持久化的核心对象都是冻结 dataclass：

- `CalibrationParams`：参考声道、门限、频带模式、FFT 分帧、平滑带宽、目标曲线、FIR 阶数、误差阈值和一致性容差。
- `CalibrationState`：版本号、父哈希、参数、源 WAV 哈希、频带定义、每声道计算产物、误差、一致性告警、受影响集合、节点哈希、内容哈希和恢复运行 ID。
- `ChannelComputation`：单声道对齐、频带能量、平滑结果、FIR 与逐频带误差。
- `FilterDesign`：线性相位 FIR 系数、群延迟、逐频带设计增益、截断误差和受影响频点统计。

版本内容哈希使用排序键、紧凑分隔符和规范 JSON。计算哈希时排除 `content_hash` 与 `run_id`，但保留 `parent_hash`，因此历史链、参数和结果任一变化都会改变哈希。浮点数组先转普通列表，非有限浮点直接拒绝写入。

## 2. WAV 与直达声对齐

读取器支持：

- 单声道 `WAVE_FORMAT_PCM`。
- 5.1 `WAVE_FORMAT_PCM` 或 `WAVE_FORMAT_EXTENSIBLE`（PCM 子格式 GUID）。
- 固定 48 kHz、valid bits 16、block align 与声道数一致。
- 标准 5.1 顺序 `FL, FR, FC, LFE, BL, BR`。

每个声道的自适应门限：

```text
sigma_noise = median(|x[0:N_noise]|) / 0.67448975
T = max(10^(floor_dbfs/20), noise_multiplier * sigma_noise, peak_fraction * peak)
arrival = min{ i | |x[i]| > T }
```

默认 `floor_dbfs=-60 dBFS`、`noise_multiplier=6`、`noise_window_ms=10`、`peak_fraction=0.005`。使用中位数绝对偏差估计噪声可在早期脉冲存在时保持稳健。

参考声道 `r` 确定后：

```text
offset_c = arrival_c - arrival_r
gain_c = direct_peak_r / direct_peak_c
aligned_c = shift(x_c, -offset_c) * gain_c
```

对齐保持原数组长度；向前移动的样本丢弃，向后移动以零填充。`alignment_difference` 同时输出新旧参考、每声道新旧到达样本、到达差、偏移差和变化声道集合。门限或参考变化会强制重算全部对齐节点。

## 3. 频带能量与上界

对每个对齐后的脉冲响应：

1. 以可配置帧长、重叠和窗函数分帧；最后一帧覆盖信号尾部。
2. 每帧 `rfft`，功率谱按相干增益平方归一：`P = |FFT(xw)|^2 / cg^2`。
3. 每段在频带内求和，得到 `E_k[b]`。
4. 频带能量为 `mean_k E_k[b]`，并输出线性功率和 `10 log10` 值。

上界由三部分构成：

```text
e_cg    = eps * sqrt(N) / cg
e_seg   = 2 * std(E_k[b]) / sqrt(K) / mean(E_k[b])
e_round = 8 * eps * sqrt(N)
e_band  = clip(e_cg + e_seg + e_round, 0, 2)
```

`e_seg` 的 2 倍系数形成保守双侧界。`BandEnergy` 直接保存 `coherent_gain`、`segments`、`segment_std_rel`、`rounding_rel` 和 `relative_upper`，传播模型无需重新解释原始 FFT。

倍频程中心按 `2^(k/3)`（三分之一倍频程）或 `2^k` 生成；频带范围为 `[f_center*2^(-s/2), f_center*2^(s/2))`，其中 `s` 为 1 或 1/3。

## 4. 平滑与线性相位 FIR

平滑在 `log2(f)` 域进行。带宽参数表示完整平滑窗宽度（octave）；高斯权重标准差取带宽的 `1/2.35482`，并对权重归一化。改变平滑带宽时，用新旧权重矩阵逐频带比较得到受影响频带集合。

补偿设计：

```text
desired_db[b] = clip(target_db[b] - smoothed_db[b], -max_cut_db, max_boost_db)
```

在 FIR 的 `rfft` 频点上按 `log2(f)` 插值目标增益，构造零相位幅频响应，`irfft` 后循环移到中心，得到对称线性相位 FIR。要求奇数阶，群延迟为 `(order-1)/2` 样本。只改变目标曲线或平滑时，未受影响频带复用上一版 `desired_gain_db`，仅更新受影响响应区段，再执行一次整体逆变换。

阶数不在 `[15, 8191]`、为偶数或目标曲线非法时，直接抛出 `E_FILTER_INVALID`；引擎在创建快照前校验，因此最后发布版本和上一有效滤波器保持不变。

## 5. 误差传播

每个声道、每个频带串联以下节点：

1. `quantization`：`q/sqrt(12)/max(|direct_peak|, eps)`，其中 `q=1/32768`。
2. `window_coherent_gain`：窗归一化舍入上界。
3. `segment_variance`：分段方差与 FFT 舍入。
4. `smoothing_coupling`：`max(0, sum_j w[b,j]*upstream[j] - upstream[b])`，显式表达邻带误差耦合。
5. `filter_truncation`：有限 FIR 在密集频点上的目标响应偏差 95% 分位，加上首尾系数能量占比惩罚，上限 1。

相对误差按保守加法累计：

```text
cumulative = sum(contribution_i)
upper_db   = 20 log10(1 + cumulative)
```

每个节点记录该节点后的累计上界和相对放大系数。上界跨过用户阈值时，`attribution_node` 取首个跨阈节点，`dominant_node` 取贡献最大节点；告警同时给出声道、频带中心、节点误差、累计值、阈值和放大系数。

## 6. 增量计划

参数差异映射到节点集合：

| 变化 | 对齐 | 频谱 | 平滑 | 滤波器 | 误差 | 一致性 |
| --- | --- | --- | --- | --- | --- | --- |
| 参考声道/门限 | 全量 | 全量 | 全量 | 全量 | 全量 | 全量 |
| FFT/频带配置 | 复用 | 全量 | 全量 | 全量 | 全量 | 全量 |
| 平滑带宽 | 复用 | 复用 | 受影响频带 | 受影响频带 | 受影响频带 | 受影响频带 |
| 目标曲线 | 复用 | 复用 | 复用 | 受影响频带 | 受影响频带 | 复用 |
| FIR 阶数/增益范围 | 复用 | 复用 | 复用 | 全量 | 全量 | 复用 |
| 误差阈值 | 复用 | 复用 | 复用 | 复用 | 全量 | 复用 |
| 一致性容差 | 复用 | 复用 | 复用 | 复用 | 复用 | 全量 |

未受影响节点保留上一版本的最终对象和 `node_hashes` 值。`recomputed_nodes` 是实际重算节点名集合，便于验收缓存命中。每声道全部计算完成后写一次快照段，然后进入下一声道检查点。

## 7. 版本与原子发布

`StateStore.publish` 在进程级文件锁内执行：

1. 校验新 state 自身内容哈希。
2. 读取当前最高完整版本，校验期望父哈希。
3. 要求版本号严格递增 1。
4. 将版本写入同目录临时文件，`fsync` 后 `os.replace` 到只读不可变版本文件，并 `fsync` 目录。
5. 原子替换 `latest.json`。
6. 追加并 `fsync` `trace.jsonl`。

即使进程在步骤 4 与 5 之间被杀死，步骤 4 的版本文件已经是完整有效发布；`latest` 读取逻辑以最高完整版本为准，因此查询不会看到半成品。

内容哈希不匹配时 `load` 返回 `E_VERSION_STALE`。回退创建新 state：父哈希指向当前最高版本、产物复制自目标版本、`rollback_of` 记录目标版本号，再走同一发布路径；历史文件永不改写。

## 8. 快照与恢复

快照目录：

```text
snapshots/run-<created_ns>-<random>/
  manifest.json
  segment-000-ch00.json
  segment-001-ch01.json
  ...
```

manifest 包含格式版本、run ID、创建顺序、base 版本/哈希、完整参数、源哈希、采样率、声道布局、频带定义、段清单和 chain head。每个段 payload 包含：

- 段索引、声道索引、频带区间。
- 参数哈希、源哈希、前一段哈希。
- 已完成声道的对齐、频谱、平滑、FIR 系数和误差上界。

段写入顺序为“段临时文件 + fsync + 原子重命名”，随后“manifest 临时文件 + fsync + 原子重命名”。强制终止后未进入 manifest 的临时文件或孤儿段不会被恢复采用。

恢复流程：

1. 选择最新 run 的 manifest；缺字段立即 `E_RECOVERY_INVALID`。
2. 校验 base 版本文件存在且哈希匹配。
3. 按段索引顺序校验文件存在、payload 哈希、参数哈希、声道索引、前向哈希和 manifest 段字段。
4. 任何断链返回 `E_RECOVERY_INVALID`，`first_broken_interval` 给出首个有问题的声道/频带区间。
5. 已提交声道直接作为缓存输入；缺失声道重新计算，不重复调用快照写入。
6. 所有声道完成后重算跨声道一致性，构造完整 state，以一次发布原子替换版本。
7. 若该 run 已有匹配 `run_id` 的发布版本，恢复直接返回已发布状态；若最新 run 是失败业务提交，则返回最后已发布版本。

## 9. 并发边界

- `CalibrationEngine` 使用不可重入 busy 锁；`submit`、`start_async`、`rollback`、`recover` 同时只允许一个。
- 重复提交返回 `E_BUSY`。
- 异步任务使用独立 Python 线程；GUI 只通过线程安全 `queue.Queue` 接收进度，由主线程 `QTimer` 更新控件。
- 取消使用 `threading.Event`，在 prepare、每个声道、每个频带、smoothing 每个频带、consistency 和 publish 前检查；取消抛出 `E_CANCELLED`，不发布新版本。
- 版本查询读取完整只读文件；不会在版本替换中途暴露部分声道或部分频带。

## 10. 图形工作台

PySide6 主线程仅负责界面；校准、恢复均在后台线程运行。界面包含：

- 声学响应：所有声道原始/平滑频带能量、选中声道误差上界包络、FIR 幅频响应与群延迟。
- 声道概览：到达样本、偏移、增益、门限、群延迟、最大误差、一致性告警和滤波器抽头数。
- 误差与一致性：阈值归因表和跨声道告警表。
- 版本：不可变版本表、与上一版参数/系数/误差差异、回退按钮。
- 运行日志：提交参数、发布哈希、取消、恢复与错误业务码。

界面文字使用英文，以避免无中文字体环境出现 Qt 方框；代码和文档仍以中文说明业务语义。

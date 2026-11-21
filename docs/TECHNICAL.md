# 技术说明

## 1. 处理链

```text
WAV/dir
  -> MarkerDetection
  -> SegmentDelay[]
  -> DriftFit[] + DriftDiagnosis[]
  -> CompensationParams
  -> Resampled chunks -> synchronized multi-channel WAV
  -> v3 SyncReport -> v1/v2/v3 atomic export
```

入口为 `SyncPipeline.analyze()`。每一步也作为公开函数提供，便于离线批处理、
测试和替换算法。

## 2. WAV 与确定性

`mcsync.wavio` 使用 Python 标准库 `wave` 读取 PCM：

- 仅接受 `sample_width=2`、`comptype=NONE`。
- 交织 PCM 转为 `float64` 的 `(channels, frames)`。
- 逐通道目录按文件名排序；不同长度以零填充到最大长度，并保留
  `original_lengths`。
- 生成 WAV 时先量化为 `<i2`，再通过同目录临时文件、`fsync`、
  `os.replace` 和目录 `fsync` 原子发布。

FFT 与时延计算使用 NumPy。PySide6 仅用于桌面界面，核心算法不依赖 GUI。

## 3. 标记检测

输入已知周期为 `T` 的脉冲标记。处理步骤：

1. 对通道平方做滑动均值并开方，得到短时能量包络。
2. 以中位数/MAD 估计噪声底，抑制固定幅值阈值对增益的敏感性。
3. 找局部包络峰值，用参考通道首个标记锚定全部通道的 `marker_index`。
4. 后续标记用最近成功位置、周期和在线速率估计外推；缺失后仍从最后有效位置
   继续外推，避免整通道错位。
5. 在峰值邻域对原始波形做短时能量质心细化，降低包络平台造成的样本偏差。
6. 置信度由包络对比度和 SNR 共同组成。

任一标记未达到 `min_snr_db`/`min_confidence` 时生成 `E_MARKER_MISSING`，
包含 `channel`、`marker_index`、期望位置、搜索范围、SNR 和置信度。
通道不会被静默跳过。

## 4. 分段时延

`SegmentationConfig.segment_length_samples` 将整段录音切为等长区间。

对每通道：

1. 收集参考/通道在区间内同序号标记的到达差。
2. 用稳健直线拟合得到区间中点的粗时延。
3. 在粗时延附近执行分段归一化互相关。
4. 对峰值抛物线插值，得到分数样本时延。
5. 由标记残差散布和相关峰平台宽度合成 95% 不确定区间。

有标记时只搜索粗时延附近 `refine_search_samples`；无有效标记对时才使用
较大的 `delay_search_samples`。这避免事件跳变/稀疏脉冲被零延迟背景相关峰
掩盖。

`segment_length_samples` 的取舍：

- 较短：更容易定位跳变，但每段标记少，方差大。
- 较长：频偏估计更稳，但段内漂移和跳变被平均。

## 5. 漂移拟合

模型：

```text
d_i = offset + slope * x_i + e_i
x_i = segment midpoint sample
drift_ppm = slope * 1e6
```

拟合流程：

1. 对 `n <= 64` 的所有段对做确定性 RANSAC；更长序列使用均匀抽取的 64 个锚点。
2. 以“内点数、内点残差中位数、RMS、较小斜率”排序选模型。
3. 用内点进行加权最小二乘。
4. 使用 Huber 型权重重估固定次数。
5. 给出斜率标准误和 ppm 置信区间。
6. 计算逐段预测、残差、最大残差段。

超过 `residual_threshold_samples` 时生成 `E_RESIDUAL_THRESHOLD`，并在
`max_residual_segment` 定位。

`compare_drift_fits()` 输出每个通道：

- 新旧初始偏移差；
- 新旧 ppm 差；
- 每段新旧预测差；
- 新模型对该段观测的预测误差。

增量引擎在拟合配置或单通道拟合覆盖变化时自动把该比较放入
`reuse_report.fit_parameter_comparison`。

## 6. 漂移来源诊断

诊断同时使用：

- 一阶拟合的 ppm 显著性和置信区间；
- 拟合优度/RMS；
- 相邻分段时延差减去中位相邻差后的跳变量；
- 跳变前后的证据段。

结论：

- `reference_channel`
- `clock_frequency_offset`
- `event_jump`
- `mixed_clock_drift_and_event_jump`
- `unknown`

`unknown` 生成 `E_DRIFT_UNKNOWN`，候选解释包括低于阈值的频偏、缺少稳定邻段的
事件跳变、标记检测误差。

## 7. 分数延迟补偿

拟合模型给出：

```text
input_position(m) = offset + (1 + slope) * m
```

两种模式：

- `linear`: `np.interp`，速度快，适合低成本预览。
- `windowed_sinc`: Hann 窗 sinc，局部 `2 * sinc_half_taps` taps，分块计算，
  边缘复制，权值归一化。

输出长度为所有活动通道可安全映射长度的最小值。补偿后重新执行标记检测，
计算每通道残差 RMS、最大绝对偏差、最差通道和容差判定。

`compare_interpolation_modes()` 同时运行两种模式，输出：

- 两种模式的残差摘要与输出哈希；
- 最大偏差差；
- RMS 差；
- 每种模式的最差通道；
- 推荐模式（`best_mode`）。

非法模式立即抛出 `E_MODE_INVALID`，并列出允许值。

## 8. 报告版本

### v1

仅能表达顶层采样率、参考通道和标记：

```json
{
  "channel": 1,
  "marker_index": 4,
  "arrival_sample": 108042.5
}
```

### v2

增加：

- `segments` / 分段时延、峰值相关度、CI；
- `drift_models` / 一阶参数、逐段残差和预测；
- 诊断结论与拟合配置。

### v3

增加：

- `channel_layout`；
- `compensation` 参数与输出长度；
- `residual_summary`；
- `provenance`；
- `issues`；
- `integrity.content_sha256`。

载入旧版后内部统一为 v3。`provenance` 记录直接来源或推导来源。
降级不会删除字段而不告知：目标无法表达的字段写入
`compatibility.lost_fields`，同时 API 返回 `E_VERSION_LOSS` Issue。

## 9. 原子导出与哈希

报告内容哈希范围是“去掉 `integrity` 后的规范化 JSON”。规范化规则：

- key 排序；
- 紧凑 UTF-8；
- 禁止 NaN/Inf；
- 数组/NumPy 标量转为普通 JSON 类型。

导出顺序：

1. 校验 `result.parameter_version == report.parameter_version ==
   compensation.parameter_version`。
2. WAV 在同目录临时文件写完、`fsync` 后原子替换。
3. 计算 WAV SHA-256。
4. 报告 JSON 同目录临时文件写完、`fsync` 后最后原子替换。

报告最后提交，且 `integrity.wav_sha256` 可验证对应 WAV。

## 10. 增量缓存与发布

缓存层级：

- 标记检测：键含音频哈希、通道、参考通道、标记配置。
- 分段：键含音频、通道/参考、区间、两端标记、分段配置。
- 拟合：键含该通道分段内容、拟合配置、采样率、参考通道。
- 补偿拼接块：键含音频、通道、模型、模式、taps、输出区间。

每次缓存项均保存内容 SHA-256，命中后先重算哈希校验，再复用。

变更影响：

- 只换插值模式：标记/分段/拟合复用，只重算补偿块。
- 减少活动通道：保留剩余通道缓存，删除/忽略被移除通道。
- 单通道拟合覆盖：仅该通道拟合与补偿块重算。
- 分段长度变化：分段键失效，分段/拟合/块重算。
- 参考通道变化：所有跨通道键失效。

新结果在状态副本上完全构建后，在锁内一次替换 `PublishedSnapshot` 引用；
构建期间的查询直接取得完整旧快照。查询只能看到完整旧快照或完整新快照，
不会看到半成品。`reuse_report` 明确列出：

- `reused_segments` / `recomputed_segments`
- `reused_fits` / `recomputed_fits`
- `reused_concat_chunks` / `recomputed_concat_chunks`
- 各缓存项内容 SHA-256

## 11. 恢复日志

恢复目录结构：

```text
journal/
  commit_00000000.json
  commit_00000001.json
  ...
  chunks/
    ch00_0000000000_0000009600.f64le
```

每个提交记录包含：

- 连续序号；
- 报告参数版本和报告内容哈希；
- 通道与半开区间 `[start, end)`；
- 该通道拟合参数、区间内分段时延、补偿参数；
- 原始 little-endian float64 补偿块路径、SHA-256、dtype、shape；
- `prev_hash`、`record_hash`。

`record_hash` 为记录（去除自身）的规范化 JSON SHA-256；`prev_hash` 形成链。

恢复时：

1. 读取全部 `commit_*.json`，序号必须连续。
2. 校验 `prev_hash`、`record_hash`、参数版本、报告哈希。
3. 校验块路径不越界、块文件存在、字节哈希、shape 和区间长度。
4. 发现断链、缺字段、重叠、块损坏均抛 `E_RECOVERY_INVALID`。
5. 只对未提交区间调用补偿回调。
6. 每完成一个区间，先原子写块，再原子写提交记录。

提交已存在的区间会被拒绝，恢复过程不会重复写入。

## 12. 真实取舍与限制

- 当前脉冲标记模型假设周期已知且单通道内无强同周期干扰。
- 事件跳变可由诊断定位，但不属于一阶连续漂移模型可补偿的范围；此时补偿残差
  会超容差并报告 `E_ALIGNMENT_FAILED`。
- 分段互相关以标记对作为粗时延锚点；完全无标记的区间只能依赖上一段外推和
  全局相关搜索，可靠性下降。
- `windowed_sinc` 精度通常更高，但计算量约为线性的数十倍。
- 增量缓存当前为进程内缓存；恢复日志提供跨进程的补偿区间持久化。


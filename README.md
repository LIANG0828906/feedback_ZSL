# 虚拟演出场馆监听串音工作台

这是一个完全本地、确定性的声道路由与串音诊断工作台。输入是逐对源声道/拾取声道的 16-bit PCM WAV、扬声器与拾取位置清单、分频段配置；输出包括几何对齐、逐频段耦合强度与相对相位、路由矩阵、残余串音预算、相位/极性判定、抵消增益与极性设置，以及可审计的增量复算轨迹。

运行环境只需要 Python 3.10+、NumPy 和 PyQt5：

```bash
python3 -m pip install numpy pyqt5
python3 -m unittest discover -v
```

不访问网络、不使用 Docker、不依赖真实音频设备。样本、缓存、发布快照和轨迹日志都写入本地工作区。

## 快速开始

生成确定性样本工作区：

```bash
python3 -m venue_crosstalk generate artifacts/demo --seed 20261122 --duration 0.5
```

执行完整分析：

```bash
python3 -m venue_crosstalk analyze artifacts/demo --json-out artifacts/demo/report.json
```

运行验收序列（基线 -> 目标抑制量变化 -> 分频段边界变化），轨迹写入 `logs/trace.jsonl`：

```bash
python3 -m venue_crosstalk diagnose artifacts/demo \
  --fresh --regenerate --seed 20261122 --duration 0.5 \
  --json-out artifacts/demo/diagnostic_report.json
```

启动桌面工作台：

```bash
python3 -m venue_crosstalk gui artifacts/demo
```

查看最后一次原子发布的结果：

```bash
python3 -m venue_crosstalk show artifacts/demo
cat artifacts/demo/publish/snapshot.json
```

对照生成器注入真值执行自动验收：

```bash
python3 -m venue_crosstalk verify artifacts/demo \
  --json-out artifacts/demo/verification.json
```

## 工作区格式

`generate` 创建以下结构：

```text
workspace/
  manifest.json           # 采样率、几何/配置路径、逐对测量清单
  geometry.json           # 6 个扬声器与 6 个拾取点的笛卡尔坐标
  crossover.json          # 频段、过渡带、目标抑制量、校准约束
  ground_truth.json       # 注入的距离、延迟、频段增益、相位和场景标签
  references/test_*.wav   # 单声道原始测试信号
  pairs/*.wav             # 单声道或 5.1 布局的逐对测量
  cache/                  # 内容寻址节点缓存
  publish/snapshot.json   # 最后一次完整发布
  logs/trace.jsonl        # 确定性 JSONL 轨迹
```

`manifest.json` 中逐对条目示例：

```json
{
  "id": "FL>FR",
  "source": "FL",
  "pickup": "FR",
  "wav": "pairs/FL__to__FR.wav",
  "layout": "5.1",
  "source_channel_index": 0,
  "pickup_channel_index": 1,
  "reference": null,
  "reference_channel_index": 0,
  "distance_m": null
}
```

- `5.1` 文件按 `FL, FR, FC, LFE, SL, SR` 排列。同一文件中，`source_channel_index` 是干净参考，`pickup_channel_index` 是拾取分量。
- `mono` 文件只保存拾取分量，`reference` 指向独立的单声道参考 WAV。
- `distance_m` 为 `null` 时，距离由扬声器与拾取点欧氏距离计算；非空时作为显式距离依据。负值会以 `E_GEOMETRY_INVALID` 中止。
- 几何坐标缺失、非有限值或缺少扬声器/拾取条目时，同样以 `E_GEOMETRY_INVALID` 中止，并在 `details.missing` 中定位。

## 算法

### 1. 几何对齐

对每一对 `s -> p`：

```text
d = ||speaker_s - pickup_p||          (或 manifest 显式 distance_m)
τ_seconds = d / c
τ_samples = round(τ_seconds * 48000)
```

分析把拾取分量向左移 `τ_samples` 个整样本，并裁剪参考到同一长度。同时用 FFT 互相关估计实际到达样本，输出：

- `arrival_samples`：互相关峰对应的到达样本；
- `alignment_offset_samples`：实际用于对齐的偏移；
- `residual_offset_samples`：检测到达与几何延迟之差；
- `distance_basis`：距离、延迟、坐标和换算类型。

### 2. 分频段耦合估计

每个频段使用升余弦过渡带掩码，在频域对参考和拾取分量做确定性带通，并估计复传递关系：

```text
H_b = Σ W(f) Y(f) conj(X(f)) / Σ W(f) |X(f)|²
耦合强度 = |H_b|
相对相位 = arg(H_b)
```

`W(f)` 在频段平台为 1，在配置的 `transition_hz` 过渡带内按升余弦衰减。当前实现不使用随机窗或随机初始化。

幅度不确定度由三部分合成为相对误差：

```text
n_eff = N * min(1, bandwidth / Nyquist)
e_q   = (quantization_step / sqrt(12)) / max(rms_y, quantization_step / sqrt(12))
e_t   = effective_transition_hz / bandwidth
e_n   = 1 / sqrt(n_eff)
e     = sqrt(e_q² + e_t² + e_n²)
U_amp = 20 log10(1 + e)
```

相位不确定度使用 `atan(e) + 0.35/sqrt(coherence)`，并将输出限制到 `[0, π]`。每条估计都给出幅度区间、相位区间和相干度。分频段边界或测量内容变化时，`estimation_changes` 会列出旧/新幅度、相位、幅度不确定度和相位不确定度差。

### 3. 相位连续性与极性

算法按频率顺序检查相邻频段：

- 原始相位差超过 `wrap_threshold_deg`，但按 `[-180°, 180°]` 折返后的差很小：标记为边界相位卷绕，不据此判定反接；
- 折返后仍超过 `jump_threshold_deg`：标记为相位跳变；
- 低频相邻频段的相位斜率换算为群延迟，如果相邻群延迟差超过阈值，标记为低频群延迟不连续；
- 所有频段都落在 `±180°` 容差内且没有不可解释跳变：判定为 `inverted`；
- 只有局部频段落在 `±180°` 附近，或存在无法解释的跳变/群延迟不连续：判定为 `unknown`，报告 `E_POLARITY_UNKNOWN`，并给出候选频段区间、卷绕、跳变、群延迟和证据。

真正的全频段反接与单独的分频段边界卷绕使用不同证据链。样本中的 `FL>FR` 被识别为真实反相，`FL>FC` 在约 1.2-5 kHz 的边界卷绕不会误报为反接。

### 4. 路由矩阵与残余串音预算

每个频段构造 `pickup × source` 复矩阵。非对角元素是测得耦合，缺失的对角元素归一为 1。对每对非对角声道求解抵消项后，预算同时给出：

- 逐对残余幅度、残余能量、达到的抑制量、目标、差距和是否达标；
- 每个拾取声道在该频段的残余能量和相对主声道的抑制量；
- `shortfall_pairs` 与 `shortfall_pickups`，不会只输出整机平均值。

### 5. 抵消增益与极性约束

测得复耦合为 `c = a e^(jφ)`，允许的抵消项是 `h = +g` 或 `h = -g`。求解器先选择使残余最小的符号，再取理想增益 `a|cos φ|`，按固定顺序执行：

1. `phase_polarity`：依据相位判定选择抵消极性；
2. `cancel_gain_limit`：限制到 `max_cancel_gain`；
3. `primary_suppression_limit`：按照主声道泄漏系数 `κ` 限制增益，使 `-20log10|1-gκ|` 不超过 `max_primary_suppression_db`；
4. `target_suppression`：计算最终抑制量并检查目标。

每一步都记录增益前后值、限制、达到的抑制量以及是否命中目标。残余值包含由估计不确定度和量化步长导出的保守保护下限，避免报告非物理的无限抑制。任一约束导致目标不可达时，输出 `E_CALIB_INFEASIBLE`，并定位声道对、频段、差距和主声道抑制量。

`polarity_setting` 是“抵消注入项”的极性；`polarity_verdict` 是“测量路径”的极性判定。因此，测量路径本身反相时，抵消注入通常使用 `normal`，这不会被混为同一个字段。

## 增量依赖链

分析被拆成可内容寻址的节点：

```text
measurement
  -> geometry_alignment
     -> band_coupling (每对 × 每频段)
        -> routing_matrix (每频段)
        -> phase_continuity (每对，依赖全部频段估计)
           -> calibration (每对 × 每频段)
              -> residual_budget (每频段)
                 -> publish
```

缓存键由节点名、范围和依赖内容哈希组成；缓存记录自身还保存 payload 哈希，读取时重新验证。行为如下：

- 修改目标抑制量：耦合、矩阵、相位不变，只重算校准和预算；
- 修改一个分频边界：只让相邻受影响频段的分频段估计、矩阵、校准和预算失效；相位连续性按声道对重算，但未受影响频段的校准复用其相位判定签名；
- 修改几何或距离依据：对齐失效，并保守重算全部频段；
- 修改某个测量文件：该对内容哈希变化，相关节点失效；
- 无法比较旧发布状态、发布 schema 不兼容或缓存损坏：保守重算，并在轨迹 `incremental_plan` 节点和 `incremental.reasons` 写明原因。

发布不是一个节点的原地修改：计算结果完成后写临时快照，再由 `os.replace` 原子替换 `publish/snapshot.json`。取消或异常不会覆盖最后一次成功发布。

## 并发工作台

PyQt5 界面提供：

- 可编辑频段边界和每个频段的目标抑制量；
- 路由矩阵热力图，单元格显示耦合 dB 与相位；
- 逐对/逐拾取声道的残余预算表，未达标行标红；
- 相位连续性、卷绕、跳变和群延迟标记；
- 抵消增益、极性、约束顺序、主声道抑制量和可行性；
- 几何到达样本、对齐偏移与距离依据。

复算在后台线程中完成。控制器对同一工作区使用进程内仲裁，重复提交返回 `E_BUSY`。取消使用协作式事件，分析线程在节点边界检查取消；取消后不发布临时结果，并保留上一次快照。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_GEOMETRY_INVALID` | 距离为负、坐标无效或几何条目缺失 |
| `E_POLARITY_UNKNOWN` | 真实反接与局部相位偏移无法唯一区分 |
| `E_CALIB_INFEASIBLE` | 增益上限、主声道限制或相位条件使目标不可达 |
| `E_BUSY` | 同一测量已有复算任务 |
| `E_CANCELLED` | 协作式取消 |
| `E_MEASUREMENT_INVALID` | WAV 格式、布局、声道或帧数不合法 |
| `E_CONFIG_INVALID` | 分频段或校准配置不合法 |
| `E_CACHE_INVALID` | 缓存内容与证明路径不一致 |

## 确定性轨迹

`logs/trace.jsonl` 每行都是一个按字段排序的规范 JSON 对象，包含：

- `node`：`incremental_plan`, `measurement`, `geometry_alignment`, `band_coupling`, `routing_matrix`, `phase_continuity`, `calibration`, `residual_budget`, `publish`；
- `pair`、`band`；
- `cache_hit`、`reason`、`content_hash`；
- 耦合估计、残余预算、校准设置或发布统计；
- `seq` 和累计 `chain_hash`。

日志不写墙钟时间、进程号、绝对临时路径或随机值。相同种子、相同样本和相同操作序列在全新目录中产生逐字节一致的 `trace.jsonl`、路由矩阵、校准设置和发布快照。

## 本地验证结果

已执行：

```bash
python3 -m unittest discover -v
```

结果：15 个测试全部通过，覆盖：

- 单声道/5.1 WAV 读取与几何对齐；
- 150 个逐频段耦合估计与注入真值对比；
- 真实反相、边界卷绕、局部相位翻转、低频群延迟不连续；
- 逐频段路由矩阵、逐对和逐拾取预算；
- 抵消增益上限、主声道限制和 `E_CALIB_INFEASIBLE`；
- 几何缺失/负距离错误定位；
- 目标变化只重算校准/预算、边界变化只重算受影响频段；
- 测量文件内容变化时输出新旧耦合和不确定度差；
- 未变化输入的全缓存命中；
- 取消不覆盖最后发布；
- 后台控制器 `E_BUSY`；
- 两个全新工作区在三步操作序列后的日志和快照逐字节一致；
- PyQt5 `offscreen` 界面渲染冒烟测试。

对随附验收工作区执行 `verify` 的结果为 12/12 项通过：

```text
pair_count: 30
band_count: 5
coupling_estimate_count: 150
mean_relative_magnitude_error: 0.01246
max_relative_magnitude_error: 0.14933
mean_phase_error_deg: 0.14344
max_phase_error_deg: 3.49750
max_alignment_residual_samples: 1
final_cache_hits: 246
final_cache_misses: 154
```

最终增量发布只重算了 `low` 与 `mid` 两个受影响频段，并输出 60 条逐对旧/新估计差；目标抑制量变化时 150 个耦合节点全部命中缓存，只重算 150 个校准节点和 5 个预算节点。

随交付的验收证据位于：

- `artifacts/acceptance/workspace/logs/trace.jsonl`
- `artifacts/acceptance/workspace/publish/snapshot.json`
- `artifacts/acceptance/diagnostic_report.json`
- `artifacts/acceptance/verification.json`

## 已知边界

- 相位判定只使用频段平均复传递，不重建完整扬声器模型；当混响、多路径或分频器相位与真实反接不可分解时，按设计报告 `E_POLARITY_UNKNOWN`。
- 主声道约束使用配置或逐对声明的泄漏系数 `κ`；它表达抵消路径对主声道的归一化影响，不是额外声学仿真。
- 当前发布清单是单个完整 JSON 快照。GUI 和 CLI 都从该快照及磁盘缓存恢复。

# 虚拟演播厅多声道串音校准工作台

本仓库实现一个完全本地、确定性、可恢复的多声道串音测量与校准工作台。它从逐对 16-bit PCM WAV 分量、几何清单和分频段配置出发，完成声道对齐、逐频段耦合估计、路由矩阵、残余串音预算、相位/极性判定、抵消增益求解、增量复算和原子快照发布。

不依赖声卡、麦克风阵列、扬声器或容器环境。所有验证使用固定种子的本地合成样本。

## 快速开始

要求 Python 3.9 以上。仓库验证环境固定为 Python 3.12.16。

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -r requirements.txt
uv pip install --python .venv/bin/python -e . --no-deps
```

生成样本并跑完整诊断：

```bash
chmod +x scripts/run_demo.sh
./scripts/run_demo.sh
```

也可以直接调用：

```bash
.venv/bin/python -m studio_calibrator generate \
  --out validation/demo/sample --seed 42 --duration 1.0 --force

.venv/bin/python -m studio_calibrator diagnose \
  --manifest validation/demo/sample/manifest.json \
  --geometry validation/demo/sample/geometry.json \
  --config validation/demo/sample/config.json \
  --snapshot-dir validation/demo/snapshots \
  --trace validation/demo/trace.jsonl \
  --out validation/demo/diagnosis.json

.venv/bin/python -m studio_calibrator restore \
  --snapshot-dir validation/demo/snapshots \
  --out validation/demo/restored.json
```

启动工作台：

```bash
QT_QPA_PLATFORM=xcb .venv/bin/studio-calibrator gui \
  --manifest validation/demo/sample/manifest.json \
  --geometry validation/demo/sample/geometry.json \
  --config validation/demo/sample/config.json \
  --snapshot-dir validation/demo/snapshots \
  --trace validation/demo/gui_trace.jsonl
```

## 输入格式

### `manifest.json`

声明采样率、源声道、拾取声道、每个源声道的参考激励，以及 `mono` 与 `5.1` 两种逐对分量布局。5.1 文件按 WAVE 顺序 `FL FR FC LFE BL BR` 存放六路拾取分量。

```json
{
  "schema_version": 1,
  "sample_rate": 48000,
  "speakers": ["FL", "FR", "FC", "LFE", "BL", "BR"],
  "receivers": ["FL", "FR", "FC", "LFE", "BL", "BR"],
  "main_receivers": {
    "FL": "FL",
    "FR": "FR",
    "FC": "FC",
    "LFE": "LFE",
    "BL": "BL",
    "BR": "BR"
  },
  "stimuli": {"FL": "stimuli/FL.wav"},
  "layouts": {
    "mono": {
      "pairs": [
        {
          "source": "FL",
          "receiver": "FR",
          "path": "mono/FL__FR.wav",
          "channel": 0
        }
      ]
    },
    "5.1": {
      "channel_order": ["FL", "FR", "FC", "LFE", "BL", "BR"],
      "pairs": [
        {
          "source": "FL",
          "receiver": "FR",
          "path": "pickups_5_1/FL.wav",
          "channel": 1
        }
      ]
    }
  }
}
```

`main_receivers` 明确每个源声道的主拾取声道；当源和拾取 ID 不同时必须提供，缺省则使用同名 ID。Workbench 会验证 WAV 为 48000 Hz、`PCM_16`，且布局声道数合法。`mono` 必须为单声道，`5.1` 必须为六声道。任一声源-拾取对缺失、WAV 无法读取或整段无有效分量时返回 `E_PAIR_MISSING`。

### `geometry.json`

每一对源-拾取声道都必须有正的 `distance_m`。若有三维位置，会额外计算欧氏距离并报告显式距离与位置距离之差。

```json
{
  "schema_version": 1,
  "speed_of_sound_m_s": 343.0,
  "distance_basis": "explicit_distance_quantized_from_3d_positions",
  "speaker_positions_m": {"FL": [-1.7, 1.0, 0.2]},
  "receiver_positions_m": {"FL": [-0.75, 0.35, 0.1]},
  "pairs": [
    {"source": "FL", "receiver": "FR", "distance_m": 2.501041667}
  ]
}
```

传播延迟：

```text
geometry_delay_samples = round(distance_m / speed_of_sound_m_s * sample_rate)
```

分量按 `geometry_delay_samples` 左移，使传播延迟归零；状态中同时报告互相关实测到达样本、几何延迟、对齐偏移、距离依据、实测与几何差，以及 `high/medium/low` 置信级别。

### `config.json`

```json
{
  "schema_version": 1,
  "measurement_layout": "mono",
  "max_cancellation_gain_db": 12.0,
  "target_suppression_db": 12.0,
  "min_band_samples": 64,
  "estimator_version": "band-ls-v1",
  "bands": [
    {
      "id": "B0",
      "low_hz": 20.0,
      "high_hz": 120.0,
      "transition_hz": 20.0
    }
  ],
  "phase": {
    "jump_threshold_deg": 120.0,
    "polarity_tolerance_deg": 35.0,
    "low_frequency_max_hz": 500.0,
    "group_delay_jump_samples": 2.5,
    "ambiguity_confidence": 0.6
  },
  "calibration": {
    "gain_step_db": 0.05,
    "residual_floor": 1e-12
  }
}
```

启动前强制检查 `bands`、`low_hz`、`high_hz`、`transition_hz`、`max_cancellation_gain_db`、`target_suppression_db` 和 `measurement_layout`。频段必须有序、互不重叠、过渡带宽度合法，上限位于 `[0, 60]` dB，目标位于 `(0, 120]` dB。

## 算法与业务语义

### 分频段耦合估计

对齐后的参考与分量在频域以确定性升余弦频带权函数做局部滤波，再用复最小二乘估计频带传递系数 `H_b`：

```text
H_b = sum(W * conj(X) * Y) / sum(W * |X|^2)
```

输出：

- 耦合强度 `20 log10(|H_b|)` 与相对相位 `arg(H_b)`。
- 强度与相位不确定区间。区间由有效样本数、16-bit 量化步长、相干残差和过渡带占比推导。
- 相干度、群延迟、有效样本数、有效频点数和边界泄漏占比。

频带有效样本数按 `valid_time_samples * band_width / nyquist` 换算。低于 `min_band_samples` 时以 `E_BAND_UNRESOLVED` 中止，并报告频段和有效样本数。

### 抵消深度与残余串音

`gain_db` 是正的抵消深度，范围为 `0..max_cancellation_gain_db`。`enabled=false` 表示不注入校正信号。相位失配会形成不可突破的残余地板：

```text
phase_floor = coupling_amplitude * |sin(phase)|
residual = max(coupling_amplitude * 10^(-gain_db / 20), phase_floor)
suppression_db = -20 log10(residual)
```

所需最小深度：

```text
required_gain_db = 20 log10(coupling_amplitude / target_amplitude)
```

若所需深度超过上限，或相位地板已经高于目标残余，则预算条目标注 `short` 并返回 `E_BUDGET_SHORT`。不会静默放宽目标值。预算输出逐频段、逐源-拾取对的耦合、所需深度、可达抑制量、实际残余和极性提示。

### 相位连续性与极性

对每对声道按频段顺序检查：

- `boundary_phase_wrap`：相邻相位跨过 `+/-180` 度的解析卷绕，报告原始差和卷绕差。
- `phase_jump`：超过阈值的实际相位跳变。
- `polarity_flip_candidate`：接近 180 度的跳变；同时保留“真实极性反接”“分频边界跳变”“边界能量泄漏”候选原因。
- `group_delay_discontinuity`：低频相邻频段群延迟跳变。
- `polarity_reversed`：全频段相位中心接近 180 度且圆一致性足够高，判定为稳定极性反接。

无法区分候选原因且置信度低于策略阈值时，状态中返回 `E_PHASE_AMBIGUOUS`，并保留候选原因和置信度；校准会将相关声道对标为冲突。

### 校准求解

约束按固定顺序执行：

1. `phase_polarity`
2. `target_suppression`
3. `gain_cap`
4. `main_gain_preserved`

每一步都记录目标、可达值以及是否命中。主声道冻结为 `0 dB / polarity=+1`。任何约束无法同时满足时返回 `E_CALIB_INFEASIBLE`，列出失败约束、源-拾取对和频段；该状态的 `publishable=false`。

### 增量复算

依赖链路为：

```text
alignment -> coupling -> route_matrix -> budget -> phase -> calibration
```

缓存键包含对齐内容哈希、频段哈希、估计器版本和配置内容。目标抑制量变化只重算 `budget` 与 `calibration`；改变两个相邻边界时，只重算受影响频段的耦合/路由/预算，并因连续性依赖保守重算相位。频段结构、覆盖不连续或估计器版本变化无法证明安全时，所有频段强制重算并记录原因。

状态中包含逐节点、逐频段的 `cache_hits/recomputed_bands/affected_bands`，以及新旧耦合差异摘要和最大偏差频段。未受影响频段复用缓存时，轨迹仍记录其内容哈希。

### 原子发布与恢复

每次诊断先完整构造内存状态，再写入快照临时目录：

```text
result.json
route_matrix.json
calibration.json
metadata.json
COMMITTED
```

只有所有文件、逐文件 SHA-256 和 `COMMITTED` 标记都落盘后，目录才原子改名为快照目录，随后原子替换 `CURRENT`。查询只读取管理器当前引用的完整不可变状态，不会看到半套路由矩阵或校准设置。

恢复只读取 `CURRENT` 指向且带 `COMMITTED` 的目录，并重新校验文件哈希。临时目录和未完成目录会被忽略。测试会用 `SIGKILL` 在提交前杀死进程，再验证只能恢复上一次已提交快照。

### 并发与取消

`RecomputeManager` 对同一组测量只允许一个活跃复算。重复提交返回 `E_BUSY`。取消为协作式，工作线程在配对和频段边界检查取消事件；取消后不提交快照，查询仍返回上一次已提交状态。文件读取使用 `with` 生命周期，WAV 与轨迹句柄在退出时关闭。

PyQt5 工作台在 `QThread` 中运行完整复算。主线程只更新视图，不执行滤波、FFT 或求解。

## 轨迹日志

每个事件为一行规范 JSON，包含：

- `seq`
- `node`
- `action`
- `pair`（适用时）
- `band`（适用时）
- `cached`
- `reason`
- `content_hash`
- `details`

耦合事件包含强度、相位、不确定区间、相干度和群延迟；预算事件包含逐对残余能量与可达抑制量；校准事件包含深度、极性、目标和可行性。日志不使用时间戳或文件大小代替内容哈希。

相同样本随机种子、配置和操作顺序下，校准设置、状态哈希及轨迹日志逐字节一致。

## 错误码

| 错误码 | 行为 |
| --- | --- |
| `E_PAIR_MISSING` | 几何距离缺失、配对/声道缺失或整段分量为空，中止 |
| `E_BAND_UNRESOLVED` | 频段有效样本/能量不足，中止并报频段 |
| `E_BUDGET_SHORT` | 频段无法在深度上限内达到目标，标注并继续诊断 |
| `E_PHASE_AMBIGUOUS` | 极性反接与边界效应无法区分，报告候选与置信度 |
| `E_CALIB_INFEASIBLE` | 校准约束冲突，输出完整冲突列表且不可发布 |
| `E_BUSY` | 同一组测量已有活跃复算 |
| `E_CANCELLED` | 协作式取消，保留上次已提交状态 |
| `E_CONFIG_INVALID` | 配置字段缺失或取值非法 |
| `E_LAYOUT_INVALID` | WAV/清单布局不一致 |
| `E_SNAPSHOT_INVALID` | 快照标记或内容哈希校验失败 |

## 测试

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest -vv
```

测试覆盖：

- 合成样本逐字节确定性。
- 单声道与 5.1 布局的同分量解码。
- 独立主拾取 ID 映射下的路由、预算和主声道冻结。
- 对齐、耦合、预算、极性反接与校准真值。
- 缺失距离、空分量、频段不足、预算短缺。
- 相位卷绕与极性候选区分。
- 目标/边界变化的增量缓存分区。
- 快照恢复后路由矩阵与校准设置逐字节一致。
- 提交前 `SIGKILL` 只恢复已完成快照。
- `E_BUSY`、协作式取消和上次提交结果保留。
- 无显示环境下 PyQt5 工作台渲染、后台复算与启动恢复。

实际验证结果见 [VALIDATION.md](VALIDATION.md)。

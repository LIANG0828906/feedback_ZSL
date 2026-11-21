# Cover Replay Inspector

本地确定性战术掩体对局重放、定点损伤结算、并行预算调度、撤回补偿、原子快照恢复与权威基线比对工具。

项目不依赖网络服务或真实账号。核心引擎只使用 Python 标准库；Qt 桌面界面使用可选的 PySide6，NumPy 作为任务环境依赖已纳入安装清单。

## 交付内容

| 路径 | 作用 |
| --- | --- |
| `src/cover_replay/loader.py` | 对局、掩体与配置解析校验，错误定位到记录序号和字段路径 |
| `src/cover_replay/fixed.py` | Decimal 定点量化、格式化与误差上界 |
| `src/cover_replay/damage.py` | 入射角/动量损伤与距离衰减 |
| `src/cover_replay/scheduler.py` | 固定并行度、内存预算、降级和等待队列 |
| `src/cover_replay/engine.py` | 物理步状态机、挂账、冲突、开裂、发布和续算 |
| `src/cover_replay/snapshot.py` | 临时文件 + 原子重命名、哈希链校验、轨迹重建 |
| `src/cover_replay/baseline.py` | 版本校验和首个逐帧差异归因 |
| `src/cover_replay/generator.py` | 确定性单样本与矩阵样本生成器 |
| `src/cover_replay/gui.py` | PySide6 时间轴/耐久/挂账/队列/差异工作台 |
| `tests/` | 30 项本地回归测试 |

## 环境准备

要求 Python 3.11 及以上、PySide6 6.x、NumPy 1.26 及以上。

Linux/macOS 示例：

```bash
uv venv .venv
uv pip install -e ".[gui,test]"
```

Windows PowerShell 示例：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -e ".[gui,test]"
```

也可直接使用：

```bash
python -m pip install -r requirements.txt
python -m pip install -e .
```

本次实际验证环境为 Python 3.14.4、NumPy 2.5.5、PySide6 6.12.0，满足下限要求。

## 快速开始

仓库内保留了一个已验证的小样本 `samples/final-smoke/`，可直接用于下面的重放命令。

生成一个本地样本。生成器会把配置、对局、掩体描述、基线、轨迹和清单放在同一目录：

```bash
cover-replay generate \
  --output samples/demo \
  --seed 42 \
  --covers 8 \
  --players 3 \
  --revoke-density 0.25 \
  --steps 24 \
  --parallelism 2 \
  --memory-budget 16384 \
  --clean
```

在全新目录中重放并对齐基线：

```bash
cover-replay replay \
  --input samples/demo \
  --output runs/demo-001 \
  --clean
```

从最近一次完整快照续算：

```bash
cover-replay replay \
  --input samples/demo \
  --output runs/demo-001 \
  --resume
```

无头模式注入撤回（不指定步号时，默认在目标射击生效步撤销）：

```bash
cover-replay replay \
  --input samples/demo \
  --output runs/demo-revoke \
  --clean \
  --no-baseline \
  --inject-revoke P1:0
```

启动桌面界面：

```bash
cover-replay gui --input samples/demo --output runs/gui-001
```

无显示环境可退化为脚本入口，核心重放和轨迹生成不依赖 Qt：

```bash
cover-replay replay --input samples/demo --output runs/headless --clean --json
```

生成多组合矩阵样本：

```bash
cover-replay generate-matrix --output samples/matrix --seed 2026 --steps 20 --clean
```

默认矩阵覆盖 4/8/12/16 块掩体、2/3/4/6 名玩家和 0/0.15/0.2/0.3/0.4/0.5 撤回密度。

## 目录数据契约

输入目录包含 `match.json`、`config.json`、`covers.json`，可选 `baseline.json`。引用使用相对于对局文件的路径，不使用硬编码绝对路径。

`config.json`：

```json
{
  "schema_version": 1,
  "physical_steps": 24,
  "fixed_point_decimals": 3,
  "parallelism": 2,
  "memory_budget_bytes": 16384,
  "degradation_sequence": ["1.0", "0.75", "0.5", "0.25"],
  "crack_max_iterations": 16,
  "crack_propagation_ratio": "0.4",
  "revocation_window_steps": 3,
  "damage_model_version": "cover-damage-v1",
  "baseline_version": "baseline-v1",
  "quantization_mode": "half_even"
}
```

`covers.json`：

```json
[
  {
    "id": "C01",
    "bounds": {
      "min_x": "1.000",
      "min_y": "1.000",
      "max_x": "11.000",
      "max_y": "9.000"
    },
    "material_hardness": "1.750",
    "max_durability": "120.000",
    "segment_damage_threshold": "28.000",
    "attenuation_radius": "13.000",
    "neighbors": [
      {"cover_id": "C02", "distance": "11.500"}
    ],
    "temporary_buffer_bytes": 4096
  }
]
```

`match.json`：

```json
{
  "schema_version": 1,
  "config_file": "config.json",
  "covers_file": "covers.json",
  "baseline_file": "baseline.json",
  "shots": [
    {
      "player_id": "P1",
      "client_seq": 0,
      "local_timestamp": 1001,
      "arrival_seq": 1,
      "hit_position": {"x": "5.250", "y": "4.500"},
      "cover_id": "C01",
      "incidence_angle": "32.000",
      "momentum": "8.750",
      "effective_step": 0
    }
  ],
  "revokes": [
    {
      "player_id": "P1",
      "client_seq": 0,
      "arrival_seq": 2,
      "effective_step": 0
    }
  ]
}
```

校验规则包括：

- 必需字段缺失时返回 `E_REPLAY_INVALID`，并给出 `record_index` 与 `field_path`。
- 命中点必须位于至少一块掩体范围内；重叠时可用射击的 `cover_id` 明确目标。
- 入射角必须在 `[0, 90]`，动量和本地时间戳不得为负。
- `effective_step` 必须落在 `[0, physical_steps)`。
- `player_id/client_seq` 必须唯一，邻居必须存在且不能自引用。
- 耐久、阈值、硬度、衰减半径和临时缓冲必须为正。
- 配置并行度和内存预算只读，不会在运行时静默提高。

## 结算语义

### 定点损伤

量化单位 `q = 10^-fixed_point_decimals`。单次直接损伤：

```text
raw = momentum * cos(incidence_angle) / material_hardness
damage = quantize(raw, q)
```

默认 `half_even`，也支持 `half_up`。累计损伤以整数“单位”存储，不依赖浮点累加。

每步每块掩体输出：

- `accumulated_damage`
- `remaining_durability`
- `cracked` / `crack_segments`
- `predicted_remaining`
- `quantization_error_bound`

误差上界为 `已应用的定点运算次数 * q / 2`。若传入不同 `--decimals`，引擎会从第 0 步整体重算，并生成 `quantization_impact.json`，列出开裂段数发生变化的掩体块及两侧阈值/损伤。

### 稳定顺序

同一物理步内：

- 到达操作按 `arrival_seq` 稳定排序；相同到达序号依次按操作类型、玩家标识和客户端序号排序。
- 权威损伤批次按 `(player_id, client_seq, arrival_seq, 原始记录序号)` 稳定排序。
- 调度任务按掩体记录顺序提交，结果按提交序号提交，不按线程完成时序提交。

### 开裂与连锁传播

累计损伤每越过一个 `segment_damage_threshold` 就增加一个开裂段。每个新开裂段向邻居传播：

```text
secondary = source_segment_threshold
          * crack_propagation_ratio
          * max(0, 1 - neighbor_distance / source_attenuation_radius)
```

传播在同一物理步内迭代。新一代损伤仍可能触发新开裂，直到队列为空。达到 `crack_max_iterations` 仍有待传播开裂段时：

- 返回 `E_CRACK_UNSTABLE`
- 输出 `unstable_covers` 和 `unstable_paths`
- 整步不写快照、不发布

传播损伤会被耐久上限截断，不会在已毁掩体上继续累积形成伪连锁。

### 预测挂账、权威结算和撤回

- 射击到达时先写入预测挂账，客户端预测耐久立即变化。
- 权威结算按稳定顺序将损伤写入服务端耐久；若预测所需损伤超过权威剩余耐久，返回 `E_CREDIT_CONFLICT`，附带掩体块、射击键和两侧耐久。
- 目标已毁时，额外命中标记为 `returned`，不会制造负耐久。
- 同一物理步内、结算前到达的撤回会取消 pending 挂账。
- 已结算命中在 `revocation_window_steps` 内撤回时生成补偿记录，恢复预测显示耐久；权威耐久保持已结算值。这使复核报告能够直接看出“撤回补偿造成显示差异”的来源。
- 超过窗口或目标状态已关闭时返回 `E_REVOKE_CLOSED`。

### 并行预算

- 全局并行度固定为 `parallelism`，线程池不会扩容。
- 每一步的调度波次同时受并行度和 `memory_budget_bytes` 限制。
- 单块临时缓冲超过总预算时，按 `degradation_sequence` 顺序缩小后重试。
- 所有降级仍超限时返回 `E_BUDGET_EXCEEDED`，该步不发布。
- 因预算无法加入当前波次的任务写入 `queue` 调度事件。
- 每步输出 `peak_memory_bytes`、任务预留量、降级事件以及运行级 `buffer_watermark_bytes`。

### 原子快照与恢复

每步成功后写入 `snapshots/step_000000.json` 等快照：

- 先写同目录临时文件，`fsync` 后用 `os.replace` 原子重命名。
- 快照包含步号、掩体耐久/开裂段、挂账、撤回记录、预算水位、状态哈希和哈希链前缀。
- 状态哈希覆盖规范化场景指纹和完整状态载荷。
- 链哈希为 `sha256(previous_chain_hash + ":" + state_hash)`。

恢复时从第 0 步开始验证快照版本、必需字段、场景指纹、状态哈希和前驱链。任一断链或字段损坏返回 `E_RECOVERY_INVALID`，`details.first_break_step` 指向首个断链步。

恢复只继续 `snapshot.step + 1` 之后的物理步。已结算射击保存在快照账本中，不会重复应用。轨迹日志在恢复时从已验证快照重建，避免崩溃前后的半行或重复帧。

### 基线逐帧比对

基线比对字段：

- `state_hash`
- `crack_events`
- `budget_usage`
- `buffer_watermark_bytes`

版本不匹配返回 `E_BASELINE_VERSION` 并定位到 `baseline.baseline_version` 或 `baseline.damage_model_version`。

内容不一致时只报告**首个**差异：

- 步号
- 精确字段路径，例如 `frames[2].budget_usage.peak_memory_bytes`
- 基线值和实际值
- 该步参与结算的客户端序号序列
- 该步缓冲水位

`baseline_diff.json` 与 `baseline_report.json` 同时落盘。CLI 返回码为 `2`，但重放快照仍可用于后续诊断。

## 输出文件

重放输出目录：

```text
trajectory.jsonl          每步 JSONL 轨迹，规范化字节序列
snapshots/
  step_000000.json        完整原子快照
  step_000001.json
baseline_report.json      基线匹配状态
baseline_diff.json        仅在不一致时出现
failure.json              仅在失败时出现，含结构化错误
quantization_impact.json  仅小数位变化时出现
```

每帧轨迹包含：

- `step`
- `effective_shots` / `effective_shot_client_sequence`
- `state_hash` / `chain_hash`
- 每块掩体累计损伤、剩余耐久、预测耐久、开裂段和误差上界
- `crack_events`
- `budget_usage`、`step_peak_memory_bytes`、`buffer_watermark_bytes`
- `revocation_events` 与 `compensations`

生成器另外产出 `manifest.json`，记录参数、数量和文件 SHA-256。

## GUI

`cover-replay gui` 提供：

- 启动新重放
- 从输出目录快照恢复
- 暂停/继续
- 协作式取消
- 运行时注入撤回
- 物理步时间轴和进度条
- 掩体耐久/开裂状态表
- 预测挂账曲线
- 预算等待/降级队列
- 基线首差异列表

Windows 桌面可直接运行上述命令。无 `DISPLAY`/`WAYLAND_DISPLAY` 时 CLI 会返回 `E_NO_DISPLAY`，此时应使用 `replay` 脚本入口。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_REPLAY_INVALID` | 输入缺失、格式或范围错误，附记录序号和字段路径 |
| `E_CRACK_UNSTABLE` | 达到开裂传播迭代上限仍未稳定 |
| `E_BUDGET_EXCEEDED` | 降级后单块缓冲仍超预算 |
| `E_CREDIT_CONFLICT` | 客户端预测与权威耐久冲突 |
| `E_REVOKE_CLOSED` | 撤回超出补偿窗口或目标状态已关闭 |
| `E_REVOKE_NOT_FOUND` | 撤回目标不存在 |
| `E_RECOVERY_INVALID` | 快照版本、字段、哈希链或场景指纹无效 |
| `E_BASELINE_VERSION` | 基线版本或损伤模型版本不匹配 |
| `E_BASELINE_MISMATCH` | 基线逐帧内容差异报告码 |
| `E_CANCELLED` | 协作式取消，未启动的步不发布 |
| `E_IO` | 快照/轨迹原子写入失败 |
| `E_NO_DISPLAY` | 无可用的 Qt 图形显示 |

## 测试与实跑验证

运行全部测试：

```bash
pytest
ruff check src tests
```

本次实际结果：

- `30 passed`。
- 6 个矩阵组合全部 `completed`，基线逐帧 `match`。
- `samples/final-smoke` 两次独立重放的 `trajectory.jsonl` SHA-256 均为
  `49bef587eb1e212bc6b968cbb57a62d4c3831acd2a8996a18191d1b653c1cd06`。
- 在步骤 2 后使用 `--crash-after-step 2` 强制终止，磁盘保留 3 个完整快照；`--resume` 后补齐 6 步，轨迹与不中断运行逐字节一致。
- PySide6 窗口在当前 WSL 图形显示上完成真实事件循环退出验证，并在 `QT_QPA_PLATFORM=offscreen` 下完成无显示冒烟验证。
- 注入验证覆盖：字段缺失/越界、定点阈值跨界、连锁不稳定、预算降级/拒绝、信用冲突、已毁退回、pending 撤回、窗口内补偿、窗口外关闭、快照篡改、基线版本和首差异。

强制终止/恢复验证命令：

```bash
cover-replay replay --input samples/demo --output runs/crash --clean --crash-after-step 8
cover-replay replay --input samples/demo --output runs/crash --resume --json
```

进程会在成功写入第 8 步快照后以测试码 137 退出，属于预期测试钩子。

## 确定性说明

相同种子、相同参数和相同操作序列下：

- 生成器输出的 JSON、基线和轨迹逐字节一致。
- 调度事件不包含墙钟时间，只记录稳定提交序号和波次。
- 状态哈希使用排序键、紧凑分隔符和 UTF-8 规范 JSON。
- 轨迹不使用线程完成顺序或文件系统时间戳。

小数位、并行度、内存预算、损伤模型版本或场景内容任一变化都会改变场景指纹，因此快照或基线不再被视为同一场景。

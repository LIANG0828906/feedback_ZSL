# 掩体对局确定性复核工具

本项目用于把本地战术对抗录像按固定物理步重放，逐块结算掩体耐久，并与权威基线逐帧对齐。实现覆盖定点量化、分块并行预算、客户端预扣除挂账、撤回补偿、哈希链快照续算、自适应结算策略、分叉归因和 PySide6 复核界面。

所有样本、基线、快照与报告均在本地生成，不依赖网络、账号或容器。

## 快速开始

要求 Python 3.11 或以上。当前工作区已用 Python 3.14.4 验证，并安装：

- NumPy 2.5.5
- PySide6 6.12.0

使用标准虚拟环境时：

```bash
python -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python scripts/verify_environment.py
```

本环境使用 `uv` 创建了已就绪的 `.venv`：

```bash
uv venv --python 3.14 .venv
uv pip install --python .venv/bin/python -r requirements.txt
```

生成固定种子样本与基线：

```bash
.venv/bin/python scripts/generate_samples.py \
  --out artifacts/acceptance/samples --seed 20261122
```

运行完整本地验收：

```bash
.venv/bin/python scripts/run_acceptance.py \
  --out artifacts/acceptance --seed 20261122
```

运行测试：

```bash
.venv/bin/python -m unittest discover -s tests -v
```

启动桌面复核台：

```bash
.venv/bin/python scripts/launch_gui.py \
  --sample artifacts/acceptance/samples/small
```

无桌面会话时，使用下文的 `run`、`resume`、`compare` 和 `strategy-check` 脚本入口完成验收。

## 目录

```text
cover_replay/
  config.py       配置解析、版本与不可变预算
  model.py        对局/掩体/射击字段校验
  physics.py      定点损伤公式与运行时掩体状态
  scheduler.py    确定性分块波次调度、内存预算与队列水位
  engine.py       逐步结算、挂账、开裂传播、快照写入
  snapshot.py     原子快照与哈希链恢复校验
  baseline.py     基线生成、逐帧比对与策略一致性诊断
  generator.py    固定种子样本、基线与轨迹生成
  acceptance.py   端到端验收
  cli.py          脚本化入口
  gui.py          PySide6 复核界面
tests/            35 项单元与集成测试
scripts/          样本、验收、环境检查与 GUI 入口
artifacts/        实际验证产物
```

## 配置格式

`config.json` 的关键字段：

```json
{
  "physics_steps": 24,
  "fractional_digits": 3,
  "max_workers": 3,
  "memory_budget_per_step": 9,
  "memory_units_per_block": 1,
  "damage_scale": 100,
  "spread_damage_percent": 25,
  "segment_softening_percent": 10,
  "crack_threshold_segments": 2,
  "crack_propagation_damage": "1.5",
  "max_crack_iterations": 16,
  "strategy": "adaptive",
  "full_scan_cost_limit": 8192,
  "model_version": "cover-damage-v1",
  "baseline_version": "baseline-v1",
  "snapshot_version": 1
}
```

- `max_workers` 和 `memory_budget_per_step` 只取配置值。运行时没有提高并行度或预算的接口。
- `strategy` 可选 `adaptive`、`full`、`window`。`adaptive` 在 `掩体块数 × 射击数 <= full_scan_cost_limit` 时选整场全量索引，否则选按物理步窗口索引。
- `fractional_digits` 支持 0 至 6 位，所有伤害、耐久与账本以整数定点单位计算。

## 对局格式

`match.json` 的关键字段：

```json
{
  "record_version": 1,
  "match_id": "local-demo",
  "model_version": "cover-damage-v1",
  "baseline_version": "baseline-v1",
  "covers": [
    {
      "block_id": 1,
      "x": 100.0,
      "y": 80.0,
      "width": 80.0,
      "height": 50.0,
      "material_hardness": 100.0,
      "durability_max": 1200.0,
      "segment_damage_threshold": 180.0,
      "spread_radius": 90.0,
      "memory_units": 2
    }
  ],
  "shots": [
    {
      "player_id": "P01",
      "client_seq": 7,
      "local_timestamp": "t+00000333",
      "arrival_seq": 11,
      "block_id": 1,
      "hit_position": {"x": 132.5, "y": 101.25},
      "incidence_angle": 32.0,
      "momentum": 48.5,
      "declared_step": 4,
      "predicted_damage": "42.125"
    }
  ]
}
```

校验规则：

- 掩体块编号唯一，命中点必须位于目标矩形范围内。
- 入射角与动量非负，`declared_step` 必须在 `1..physics_steps`。
- 射击 `player_id`、`client_seq`、`arrival_seq`、`timestamp` 与目标块字段必须完整。
- 同一玩家 `client_seq` 不可重复。
- 缺失或越界返回 `E_RECORD_INVALID`，并带 `shot_index` 与具体 `field`。
- `predicted_damage` 可省略。省略时视为客户端准确预测服务端直接损伤；显式提供时用于挂账对账。

## 定点损伤模型

令 `F = 10^fractional_digits`，所有输入先量化为整数定点单位。

直接损伤：

```text
cos_u = round(max(0, cos(incidence_angle)) * F)
segments = accumulated_damage_u // segment_threshold_u
softening = 100 + segment_softening_percent * segments
damage_u = round(
    momentum_u * cos_u * damage_scale * softening
    / (hardness_u * 100)
)
```

实际生效损伤截断到目标剩余耐久，因此任意时刻：

```text
remaining = durability_max - accumulated_damage
```

扩散损伤：

```text
spread_u = round(primary_damage_u * spread_damage_percent / 100)
```

扩散目标为源掩体 `spread_radius` 内的相邻掩体，距离取两个矩形的最近欧氏距离。开裂条件为：

```text
accumulated_damage >= segment_damage_threshold * crack_threshold_segments
```

新开裂掩体按块编号向扩散邻域施加 `crack_propagation_damage`，迭代传播开裂。达到 `max_crack_iterations` 仍未稳定时返回 `E_CRACK_UNSTABLE`，附涉及块和传播链。

单个物理步的确定性顺序：

1. 按块编号稳定调度各掩体任务，直接损伤基于步初累计损伤计算。
2. 按到达序号写入客户端预测挂账。
3. 应用本步撤回补偿。
4. 按 `(player_id, client_seq, shot_index)` 结算直接与扩散事件。
5. 检测并传播开裂。
6. 对账预测挂账与权威耐久。

相同输入、配置与注入参数会得到逐字节一致的结果 JSON 与轨迹 JSONL。

## 调度与预算

每个掩体块是一个独立任务，带有固定内存单位。调度器按块编号贪心组成波次：

- 同一步的待运行任务超过 `max_workers` 或剩余内存预算时进入等待队列。
- 波次内并发执行，结果仍按块编号稳定提交。
- 调度轨迹记录 `step_start`、`submit`、`commit`、`step_end`，包含块编号、等待块、等待水位和峰值内存。
- 单任务超过单步预算返回 `E_MEMORY_BUDGET`，不会临时提高配置。
- 同一步重复提交返回 `E_BUSY`。

## 挂账与冲突

- 命中到达时按 `arrival_seq` 即时记入客户端预测剩余耐久。
- 权威直接损伤为 0 时，该预测退回，状态为 `refunded`。
- 显式预测与服务端权威直接损伤一致时为 `effective`。
- 不一致时为 `conflict`，严格模式返回 `E_CREDIT_CONFLICT`，包括：
  - `block_id`
  - `shot_index`
  - `predicted_remaining`
  - `authoritative_remaining`
  - 玩家标识与客户端序号
- 使用 `--allow-conflicts` 可继续完成整场结算，并在结果与 GUI 差异列表中展示冲突。

撤回补偿按 `STEP:BLOCK:AMOUNT[:REASON]` 注入。补偿只减少权威累计损伤，并在该步挂账明细中记录实际退回量。

## 快照与恢复

每个已完成物理步写入 `step-000001.json` 这类文件。写入过程为：

1. 同目录临时文件。
2. `fsync` 文件。
3. `os.replace` 原子重命名。
4. `fsync` 目录。

快照包含：

- 步号、快照版本、输入摘要。
- 掩体累计损伤、剩余耐久、开裂状态与开裂步。
- 客户端挂账账本。
- 已提交帧、调度轨迹与队列水位。
- 前序哈希与当前 `chain_hash`。

链摘要按 `SHA-256(previous_hash + "\n" + canonical_json(snapshot_body))` 计算。恢复时逐文件校验版本、输入摘要、字段完整性、前序哈希和载荷哈希。

- 缺失步、字段缺失、载荷篡改或哈希链断裂均返回 `E_RECOVERY_INVALID`。
- 错误中的 `first_broken_step` 定位首个断链步。
- 恢复只加载磁盘快照状态与已提交帧，不重新结算历史步。
- 非恢复运行遇到已有提交快照返回 `E_SNAPSHOT_EXISTS`，避免静默覆盖。

`--stop-after-step` 是协作式中止，只保留已提交快照且不写最终报告。进程被强制终止时同样只保证已完成原子重命名的快照，不承诺生成退出报告。

## 基线与分叉诊断

`baseline.json` 保存每步：

- `state_hash`
- 每块累计损伤、剩余耐久、开裂状态
- 开裂事件
- 生效射击与客户端序号序列
- 调度预算、峰值内存与等待水位
- 完整调度决策轨迹

比对按帧顺序进行，首个差异返回 `E_BASELINE_DIVERGENCE`：

- `first_mismatch_step`
- `field_path`
- `baseline_value`
- `replay_value`
- 该步 `effective_client_sequences`
- 该步 `queue_watermark` 与 `memory_budget`
- `attribution`，区分定点损伤/归约顺序、预算调度、挂账补偿、开裂传播

基线或损伤模型版本不一致返回 `E_BASELINE_VERSION`。

全量结算与窗口结算通过 `strategy-check` 比较。不一致返回 `E_STRATEGY_DIVERGENCE`，包含步号、字段路径、两侧取值与归因。

## 脚本入口

验证输入：

```bash
.venv/bin/python -m cover_replay validate \
  --record artifacts/acceptance/samples/small/match.json \
  --config artifacts/acceptance/samples/small/config.json
```

完整重放并与基线比对：

```bash
.venv/bin/python -m cover_replay compare \
  --record artifacts/acceptance/samples/medium/match.json \
  --config artifacts/acceptance/samples/medium/config.json \
  --baseline artifacts/acceptance/samples/medium/baseline.json \
  --output artifacts/acceptance/cli/medium-result.json
```

模拟协作中止并恢复：

```bash
.venv/bin/python -m cover_replay run \
  --record artifacts/acceptance/samples/small/match.json \
  --config artifacts/acceptance/samples/small/config.json \
  --snapshots artifacts/acceptance/cli/interrupted-snapshots \
  --clean-snapshots --stop-after-step 8 \
  --output artifacts/acceptance/cli/interrupted-result.json

.venv/bin/python -m cover_replay resume \
  --record artifacts/acceptance/samples/small/match.json \
  --config artifacts/acceptance/samples/small/config.json \
  --snapshots artifacts/acceptance/cli/interrupted-snapshots \
  --output artifacts/acceptance/cli/resumed-result.json
```

严格冲突测试：

```bash
.venv/bin/python -m cover_replay run \
  --record artifacts/acceptance/samples/conflict/match.json \
  --config artifacts/acceptance/samples/conflict/config.json \
  --output artifacts/acceptance/cli/conflict-result.json \
  --allow-conflicts
```

全量/窗口策略一致性：

```bash
.venv/bin/python -m cover_replay strategy-check \
  --record artifacts/acceptance/samples/stress/match.json \
  --config artifacts/acceptance/samples/stress/config.json
```

## 桌面界面

PySide6 界面展示：

- 物理步时间轴与自动播放
- 掩体累计损伤、剩余耐久、开裂状态与开裂步
- 客户端预测挂账曲线
- 调度等待队列、峰值内存与预算水位
- 基线/冲突差异列表
- 启动、暂停、撤回注入、协作式取消、从快照恢复

在 Windows 桌面会话直接运行 `scripts/launch_gui.py`。无显示会话时，`cover_replay gui` 返回 `E_NO_DISPLAY` 并提示改用脚本入口；可用 `QT_QPA_PLATFORM=offscreen` 做界面烟测。

## 业务码

| 业务码 | 含义 |
|---|---|
| `E_RECORD_INVALID` | 对局、掩体、射击字段缺失或越界 |
| `E_CONFIG_INVALID` | 物理步、预算或策略配置非法 |
| `E_BUSY` | 同一物理步重复提交 |
| `E_MEMORY_BUDGET` | 单块任务超过配置单步预算 |
| `E_CREDIT_CONFLICT` | 客户端预测与权威结算不一致 |
| `E_RECOVERY_INVALID` | 快照版本、字段、输入摘要或哈希链错误 |
| `E_CRACK_UNSTABLE` | 开裂传播超过迭代上限 |
| `E_BASELINE_VERSION` | 基线或损伤模型版本不匹配 |
| `E_BASELINE_DIVERGENCE` | 基线逐帧比对出现首个差异 |
| `E_STRATEGY_DIVERGENCE` | 全量与窗口结算结果不一致 |
| `E_SNAPSHOT_EXISTS` | 非恢复运行遇到已有提交快照 |
| `E_NO_DISPLAY` | 无桌面会话且尝试启动 GUI |

## 实际验证结果

验证环境：Python 3.14.4、NumPy 2.5.5、PySide6 6.12.0，WSL2 Linux。

- `python -m unittest discover -s tests -v`：35 项测试全部通过。
- `cover_replay demo --seed 20261122`：18 项端到端检查全部通过，报告位于 `artifacts/acceptance/acceptance-report.json`。
- 固定种子重复生成的样本目录摘要一致。
- small 16 步、medium 24 步、stress 30 步均通过基线比对与全量/窗口策略一致性检查。
- 三档样本中断后续算结果与未中断结果逐字节一致。
- CLI 实测完整结果与恢复结果 SHA-256 均为 `32ccd90e0b45e36cc237dcec9f531fa16960c4d7792cc2e0fcb82d5c7cf25ed5`。
- CLI 实测完整轨迹与恢复轨迹 SHA-256 均为 `62971ab8444b00a540fe825677d35fca1ffef6318bc58e5044344aad6c48c05b`。
- stress 全量与窗口结算帧摘要均为 `bfc604c77cff52d8f46c9ad07a738fb2b0327d86dac892f8dc99847e0c102380`。
- conflict 样本返回 `E_CREDIT_CONFLICT`，包含块 `3`、射击 `30`、预测剩余 `1532.472`、权威剩余 `1512.289`。
- 快照第 3 步载荷篡改被定位为 `E_RECOVERY_INVALID`，`first_broken_step = 3`。

验收产物：

```text
artifacts/acceptance/acceptance-report.json
artifacts/acceptance/samples/{small,medium,stress,conflict}/
artifacts/acceptance/snapshots/{small,medium,stress}/
artifacts/acceptance/cli/full-result.json
artifacts/acceptance/cli/resumed-result.json
artifacts/acceptance/cli/full-trajectory.jsonl
artifacts/acceptance/cli/resumed-trajectory.jsonl
```

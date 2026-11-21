# 本地赛事复盘服务

这是一个离线、确定性的多人对战录像重放服务。它把 JSON 录像解析为权威房间状态，分别用定点（`decimal.Decimal`）和双精度浮点（IEEE-754 binary64）两条数值路径推进固定时间步，输出每步状态、状态哈希链、逐字段误差上界、缓存命中和异常归因结果。

实现只使用 Python 标准库：`json`、`hashlib`、`tempfile`、`os`、`decimal`、`math`、`unittest`。当前环境没有 `pip`/NumPy；核心数值路径刻意不依赖 NumPy，保证离线、可复现和可逐字节比较。若环境中另装 NumPy，不改变本实现的语义。

## 快速开始

```bash
# 生成确定性样本、基线，运行完整本地验收
python3 -m replay verify --out-dir artifacts/replay-verification --seed 20261122

# 只生成样本与基线
python3 -m replay generate --out-dir artifacts/samples --seed 20261122

# 两数值路径推进与偏差报告
python3 -m replay compare \
  --recording artifacts/samples/case-01-v2-mixed/recording.json \
  --out-dir /tmp/replay-compare

# 固定步快照
python3 -m replay run \
  --recording artifacts/samples/case-01-v2-mixed/recording.json \
  --mode fixed --out-dir /tmp/replay-run --snapshot-dir /tmp/replay-snapshots

# 从最近有效快照恢复
python3 -m replay recover \
  --recording artifacts/samples/case-01-v2-mixed/recording.json \
  --snapshot-dir /tmp/replay-snapshots --mode fixed --out-dir /tmp/replay-recover

# 基线逐帧比对
python3 -m replay check-baseline \
  --recording artifacts/samples/case-01-v2-mixed/recording.json \
  --baseline artifacts/samples/case-01-v2-mixed/baseline.json \
  --out-dir /tmp/replay-baseline

# 完整测试
python3 -m unittest discover -s tests -v
```

所有业务错误都以 JSON 输出，包含稳定的业务码、字段路径、步号和可选的注入点。退出码为 `2`；成功为 `0`。

## 录像格式

```json
{
  "recording_version": "v2",
  "match_id": "match-01",
  "config": {
    "total_steps": 48,
    "timestep": "0.1",
    "flip_threshold": "1",
    "resource_diff_threshold": "0.002",
    "coord_limit": "1000",
    "fixed_param_decimals": 3,
    "fixed_accum_decimals": 5,
    "late_window_steps": 1,
    "alive_default": true,
    "tolerances": {
      "position": "0.000001",
      "velocity": "0.000001",
      "heading": "0.000001",
      "resource": "0.000001"
    }
  },
  "initial_state": {
    "room": {"resource": "0"},
    "entities": [
      {
        "id": "p1",
        "position": {"x": "0.0625", "y": "-0.125"},
        "velocity": {"x": "0", "y": "0"},
        "heading": "0",
        "alive": true
      }
    ]
  },
  "operations": [
    {
      "id": "op-0001",
      "entity_id": "p1",
      "client_seq": 1,
      "arrival_seq": 1,
      "arrival_step": 1,
      "effective_step": 1,
      "speed": "0.25",
      "heading": "0.1",
      "resource_delta": "0.125"
    }
  ]
}
```

### 输入语义

- 所有十进制参数按字符串/`Decimal` 读取，避免 JSON 浮点解析提前引入二进制误差。
- `effective_step` 必须在 `[1, total_steps]`。
- 同一实体的 `client_seq` 必须唯一。
- `arrival_step - effective_step > late_window_steps` 视为迟到超窗。
- `speed >= 0`；`heading` 的单位是弧度；坐标绝对值不得超过 `coord_limit`。
- 同一时间步按 `(effective_step, arrival_seq, client_seq, op_id, source_index)` 稳定结算。协议数组乱序不会改变结算顺序。
- `v1` 录像没有 `alive`；升级到 `state-v2` 时按 `config.alive_default` 补齐，并在 `derived_fields` 中标注 `source: "v1_default"`。
- `alive` 是权威状态字段；当前模型没有隐式死亡规则，v2 输入显式携带该字段。

### 推进规则

每个操作在 `effective_step` 的固定时间步 `dt` 内执行：

1. 用操作速度与朝向计算速度向量。
2. 更新实体位置：`position += velocity * dt`。
3. 更新房间资源：`resource += resource_delta`。
4. 若更新后 `max(|x|, |y|) > flip_threshold`，触发朝向翻转：`heading += π` 并归一化，速度按翻转后的朝向重算；位置不因翻转再次移动。

定点路径先按 `fixed_param_decimals` 量化操作参数，再按 `fixed_accum_decimals` 量化每步速度、位置和资源累积量。量化使用 `ROUND_HALF_EVEN`，日志记录每个参数的舍入方向 `up` / `down` / `none`。浮点路径使用 Python `float`（binary64）和 `math.sin/cos`。

## 状态哈希与协议顺序

状态哈希使用固定字段顺序，而不是输入 JSON 的键顺序：

```text
state:  state_model_version, step, room, entities
room:   resource
entity: id, position, velocity, heading, alive   # v2
entity: id, position, velocity, heading          # v1
vector: x, y
```

实体按 `id` 字典序排列。状态值在哈希前规范化为字符串，浮点使用 17 位有效数字。每步：

```text
state_hash = SHA-256(canonical_state_json)
chain_hash = SHA-256(previous_chain_hash + "\n" + state_hash)
```

初始链前缀为 64 个 `0`。快照同时保存 `hash_chain_prefix`（前一步链哈希）、`state_hash` 和 `chain_hash`，恢复时重新计算并校验。

## 误差传播与归因

定点路径为每个实体维护位置、速度、朝向以及房间资源的累积误差上界。因子链固定为：

```text
position: param.velocity.quantization
          param.heading.quantization
          trig.approximation
          accum.velocity.quantization
          accum.position.quantization

resource: param.resource_delta.quantization
          accum.resource.quantization
```

每个因子的量化误差使用半个末位单位；朝向误差通过 `|v| * Δθ * dt` 传播，三角函数误差以保守界计入。每一步输出每个字段的误差上界。

- 位置误差上界跨过翻转阈值时，该步标记为不可信，并归因到误差链中最早使累积误差跨过阈值缺口的因子。
- 资源误差上界超过 `resource_diff_threshold` 时，同样标记并归因。
- 若上界跨过阈值但不存在可归因因子，抛出 `E_ATTR_UNRESOLVED`，错误详情中带候选因子列表。
- 标记不可信不会中止正常推进；它是轨迹和报告中的显式质量标记。只有无法归因时才中止。

## 每步快照与恢复

每个时间步提交一个 `step-000001.json` 形式的快照：

- 先在同目录写临时文件并 `fsync`，再用 `os.replace` 原子重命名。
- 包含步号、状态、数值模式、配置摘要、哈希链前缀、状态哈希、链哈希、误差上界摘要、完整误差状态和生效操作。
- 恢复时从 1 开始检查快照连续性和每个哈希链前缀，缺失字段、断链或截断 JSON 都返回 `E_RECOVERY_INVALID`，并定位首个断链步。
- 恢复从最近有效步继续；`effective_step <= snapshot.step` 的操作不会再次应用。
- `--cancel-after N` 在第 N 步快照提交后、下一时间步边界协作式取消，保留最后一次已提交快照。
- 同一 `match_id` 用原子 `O_CREAT|O_EXCL` 锁文件互斥；默认锁目录是工作目录下的 `.replay-locks/`，可用 `--lock-dir` 覆盖；重复启动返回 `E_BUSY`。

## 基线比对

基线包含 `baseline_version`、`state_model_version`、`match_id`、`config_digest` 和逐帧 `state_hash`、状态、生效操作集、误差摘要。比对顺序：

1. 校验基线与状态模型版本；不匹配返回 `E_BASELINE_VERSION`。
2. 逐帧比较状态哈希、存活标记、房间资源以及完整状态字段。
3. 返回首个不一致步号、字段路径、两侧取值、本地生效操作集和数值模式。
4. 若基线帧缺少 `effective_ops`，报告 `attribution_available: false`，不推断分叉原因。

## 异常注入

注入只发生在数据处理入口或对文件的事后截断，不修改结算/快照模块：

| 注入 | 业务码 | 定位 |
|---|---|---|
| `dup_client_seq` | `E_DUP_CLIENT_SEQ` | `operations[i].client_seq` |
| `out_of_bounds_coord` | `E_COORD_RANGE` | `initial_state.entities[i].position` |
| `nonfinite_heading` | `E_NONFINITE` | `operations[i].heading` |
| `late_operation` | `E_LATE_OP` | `operations[i].arrival_step` |
| `truncated_snapshot` | `E_RECOVERY_INVALID` | 被截断快照的步号/路径 |
| `corrupt_baseline` | `E_BASELINE_CORRUPT` | `frames[i].state_hash` |

CLI 示例：

```bash
python3 -m replay run --recording ... --mode fixed --out-dir ... \
  --inject dup_client_seq --inject nonfinite_heading

python3 -m replay recover --recording ... --snapshot-dir ... \
  --mode fixed --out-dir ... --truncate-latest

python3 -m replay check-baseline --recording ... --baseline ... \
  --out-dir ... --inject-baseline corrupt_baseline
```

## 业务码

| 业务码 | 含义 |
|---|---|
| `E_INPUT_INVALID` | 录像字段缺失、类型错误、步号越界等 |
| `E_MODE_INVALID` | 数值模式不是 `fixed` / `float` |
| `E_ATTR_UNRESOLVED` | 误差上界越界但无法归因 |
| `E_VERSION_LOSS` | v1 降级导出无法表达字段 |
| `E_RECOVERY_INVALID` | 快照截断、断链、版本/配置不匹配或字段缺失 |
| `E_BASELINE_VERSION` | 基线版本或状态模型版本不匹配 |
| `E_BASELINE_CORRUPT` | 基线 JSON/帧字段损坏 |
| `E_DUP_CLIENT_SEQ` | 同实体客户端序号重复 |
| `E_COORD_RANGE` | 初始或推进后坐标越界 |
| `E_NONFINITE` | 非有限数值（如 NaN/Infinity） |
| `E_LATE_OP` | 操作超过迟到窗口 |
| `E_BUSY` | 同一对局已有推进任务 |

## 实际验证结果

本机执行：

```bash
python3 -m unittest discover -s tests -v
# Ran 35 tests ... OK

python3 -m replay verify --out-dir artifacts/replay-verification --seed 20261122
# summary ok = true
```

关键结果（完整报告见 `artifacts/replay-verification/verification.md` 和 `.json`）：

| 用例 | 定点最终状态哈希 | 浮点最终状态哈希 | 基线 | 快照恢复 | 偏差条数 |
|---|---|---|---|---|---|
| case-01-v2-mixed | `83381da2d6ef69ad8b2b785783a4cc10850193634091ce507ad90d97469f717c` | `a5f4f54cdc2e2b7acaa7a03962fd9d3321a0695e05a1fab808340d0c8d34db38` | PASS | PASS | 682 |
| case-02-v1-upgrade | `af3cf5f976849b903e21456baeaa7e29aa0c1be09568633f8e416e39583621a2` | `fbd732a22fdb6cfb4321c090195025a453546ac0232e25929eb2a16c08577a46` | PASS | PASS | 346 |
| case-03-v2-precision | `237a19274689d895a96e89a0660718f64ff8415ce424d40b51c773a276a94509` | `931c01bb192d764c07c1429f81a0fefa141a879107b23b0ad35e75d2ed608976` | PASS | PASS | 1239 |
| case-04-exact-dual | `6f591c54bc7870fa25fe4b7f3199252b51ed9d40ad0b0e344d1b0b14cd28eb1c` | `6f591c54bc7870fa25fe4b7f3199252b51ed9d40ad0b0e344d1b0b14cd28eb1c` | PASS | PASS | 0 |

每类异常均返回独立业务码并带注入点；截断快照在 4 个用例中均返回 `E_RECOVERY_INVALID`；基线损坏、版本不匹配、非法模式和并发占用分别返回 `E_BASELINE_CORRUPT`、`E_BASELINE_VERSION`、`E_MODE_INVALID`、`E_BUSY`。

### 确定性说明

- 对同一模式、同一输入重复运行，轨迹日志逐字节一致；报告中的 `deterministic_fixed` / `deterministic_float` 均为 `true`。
- `case-04-exact-dual` 的所有步状态哈希在定点与浮点路径下完全一致（32/32），因为它只使用二进制精确可表示的初值、速度、时间步和资源增量，且量化精度足够。
- 一般十进制样本的两条路径会有可测量偏差，这是双路径验收本身要检测的对象。完整轨迹日志包含 `mode` 和定点舍入方向字段，因此跨模式整份日志不应逐字节相同；逐字节一致性针对同一模式的重复执行。状态哈希是否跨模式一致由样本数值是否可精确表示决定。

## 代码结构

```text
replay/
  validation.py    输入解析、校验、v1 -> v2 升级
  model.py         状态模型、固定字段顺序、哈希
  numeric.py       Decimal 定点量化、Decimal 三角函数、浮点路径
  error_model.py   误差上界链与最早因子归因
  engine.py        固定时间步推进、缓存、取消
  snapshot.py      原子快照、哈希链校验、恢复
  baseline.py      基线与逐帧比对
  compare.py       双路径偏差与确定性报告
  versioning.py    v1 降级导出与 E_VERSION_LOSS
  injection.py     入口异常注入与快照截断
  lock.py          E_BUSY 互斥
  trajectory.py    确定性 JSONL 轨迹
  generator.py     确定性样本/基线生成器
  verify.py        端到端本地验收
tests/test_replay.py
```

# Level Generation Workbench

离线、确定性的关卡生成与诊断工作台。输入为种子、约束集合和生成参数，输出满足全部硬约束的网格布局、双精度/定点误差报告、冲突归因，以及可逐事件复放的哈希链轨迹。

## 环境与安装

本项目已在 Python 3.11 下验证：

```bash
uv python install 3.11
PY311="$(uv python find 3.11)"
"$PY311" -m venv .venv
.venv/bin/python -m pip install --upgrade pip
.venv/bin/pip install -r requirements.txt
source .venv/bin/activate
```

`requirements.txt` 包含 `numpy` 与 `PySide6`。若不使用 `uv`，也可以用系统安装的 Python 3.11：

```bash
python3.11 -m venv .venv
.venv/bin/pip install -r requirements.txt
```

## 运行

生成主样本并写出 `artifacts/run/level.json`、`artifacts/run/trace.json`：

```bash
python -m levelworkbench --config samples/config.json
```

可读格式输出：

```bash
python -m levelworkbench --config samples/config.json --pretty
```

复放轨迹并校验布局哈希、归因哈希和事件哈希链：

```bash
python -m levelworkbench --replay artifacts/run/trace.json --out artifacts/replay
```

打开图形工作台：

```bash
python -m levelworkbench --config samples/config.json --gui
```

运行验收测试：

```bash
python -m unittest discover -v
```

重新生成样本、基线哈希和注入基线：

```bash
python scripts/make_samples.py
```

## 配置模型

主配置由 `version`、`seed`、`grid`、`generation`、`quantization`、`evaluation`、`limits` 和 `constraints` 组成。每个约束都有唯一的 `id`、`priority`、`hard` 标志和对应参数。

支持的约束类型：

- `room_count`：`min`、`max`，硬约束。
- `connectivity`：`scope` 为 `all_rooms` 或 `start_to_end`，硬约束。
- `key_chain`：`keys`、`coefficients`、`threshold`，硬约束；可达性与数值系数条件都参与判定。
- `no_lock_unreachable_end`：终点不得通过无锁路径到达，硬约束。
- `area_target`：`target`、`tolerance`、`coefficient`、`weight`，默认软目标。
- `door_count_target`：`target`、`tolerance`、`coefficient`、`weight`，默认软目标。

房间是整数网格矩形；门有 `both`、`forward`、`reverse` 三种方向并可绑定 `locked_by`；标签有 `start`、`end`、`key`、`lock`、`reward`。

配置样例：

```json
{
  "version": 1,
  "seed": 20261121,
  "grid": {"width": 28, "height": 20},
  "quantization": {"decimals": 2, "rounding": "half_up"},
  "evaluation": {"mode": "fixed", "uncertain_policy": "allow"},
  "constraints": [
    {"id": "rooms", "type": "room_count", "priority": 100, "hard": true, "min": 7, "max": 10},
    {"id": "key_chain", "type": "key_chain", "priority": 100, "hard": true,
     "keys": ["k1", "k2"], "coefficients": {"k1": 0.95, "k2": 0.95}, "threshold": 0.5}
  ]
}
```

## 生成管线

生成严格分为四个阶段：

1. `room_layout`：在整数网格内放置互不重叠的房间，并划分钥匙链区段。
2. `door_assignment`：建立区段内连通树，在被锁区段之间放置锁门，再添加不跨越锁边界的额外门。
3. `tag_placement`：放置起点、终点、钥匙、锁和奖励标签。
4. `key_chain`：通过唯一的 `analyze_key_chain` 实现做最终可达性及全部硬约束验证。

随机源是输入派生的 SplitMix64 序列，种子材料由 `(seed, constraints, params)` 规范化 JSON 哈希得到。没有使用 `random` 全局状态、系统时间或外部随机源。

每次阶段尝试先记录入口内容哈希；失败时记录 `phase_failure`，恢复阶段入口状态并记录 `phase_rollback`。重试和整轮重试从同一确定性序列派生不同 nonce，因此回滚不会产生不可解释的分支漂移。

## 可达性与误差归因

`analyze_key_chain` 以起点房间为源，在 `(房间, 已收集钥匙集合)` 状态空间上搜索。锁门只会在对应钥匙已经收集后通过，最终路径与解锁顺序写入评估结果。生成阶段最终校验直接复用它，不存在第二套可达性算法。

定点路径用 `Decimal` 按约束或全局 `decimals` 量化，并记录 `up`、`down`、`none` 舍入方向。误差树包含：

- 系数、目标、容差叶子节点；
- 面积、门数的多步求和；
- 面积/门数系数乘法、密钥链逐把钥匙的乘法累积；
- 差值、绝对值与阈值比较节点。

每个节点都有 `exact`、`quantized`、`lower`、`upper` 和 `width`。比较节点给出 `fixed_truth`、`float_truth`、`truth_lower`、`truth_upper`、`uncertain`，并在区间跨阈值时归因到最早引入不确定性的量化因子。

`samples/quantized_probe.json` 是端到端误差样本：面积条件在双精度下为真、定点下为假，报告包含约束标识、量化参数、上下界和 `area coefficient` 的首因归因。

## 轨迹格式

轨迹包含 `meta` 和按序追加的 `events`：

- `meta`：轨迹版本、生成器版本、种子派生哈希、参数哈希、配置快照与配置哈希、事件数、终局布局哈希、归因哈希、总校验和。
- 事件：阶段开始/完成/失败/回滚、每次整数随机抽样、约束检查结论、最终归因记录。
- 每个事件包含 `seq`、`prev_hash`、内容哈希。终局 `trace_end` 记录布局哈希、归因哈希和总事件数。

复放时先验证版本、配置快照、事件序号、哈希链、事件数和总校验和，再使用轨迹中的随机值驱动同一生成器。终局布局哈希或归因哈希不一致会以 `E_REPLAY_MISMATCH` 中止。轨迹截断、断链或版本缺失以 `E_TRACE_INVALID` 中止。两个错误都发生在写出关卡文件之前。

## 异常注入

通过 `--inject` 在配置入口解析阶段注入损坏输入：

| 模式 | 业务码 | 注入点示例 |
| --- | --- | --- |
| `room_bounds` | `E_INJECT_ROOM_BOUNDS` | `/input_layout/rooms/0` |
| `dangling_door` | `E_INJECT_DANGLING_DOOR` | `/input_layout/doors/0` |
| `key_cycle` | `E_INJECT_KEY_CYCLE` | `/input_layout/key_dependencies` |
| `constraint_limit` | `E_INJECT_CONSTRAINT_LIMIT` | `/constraints` |
| `truncated_input` | `E_INJECT_TRUNCATED_INPUT` | `$` |

示例：

```bash
python -m levelworkbench \
  --config samples/config.json --inject dangling_door --out artifacts/should-not-exist
```

该命令返回独立业务码；生成器和可达性源码不感知注入模式，且不会创建输出目录或关卡文件。

## 图形工作台

`levelworkbench/gui.py` 提供：

- 网格画布：房间、门方向、锁门、起点/终点/钥匙/锁/奖励标签；
- 侧栏：种子、约束 JSON、异常注入选择；
- 生成、取消、加载轨迹、复放最后提交轨迹；
- 冲突房间/门高亮；
- 轨迹事件浏览器。

生成和复放在 `QThread` 后台执行。`TaskGate` 保证同一工作台只有一个任务；重复触发返回 `E_BUSY`。取消通过事件通知生成器，工作线程结束后释放任务门并保留最后一次成功提交的轨迹。

## 实际验证结果

环境：

```text
Python 3.11.18
numpy 2.4.6
PySide6 6.12.0
```

自动化测试：

```text
Ran 20 tests
OK
```

覆盖项包括确定性哈希、硬约束、钥匙链路径、锁门前置条件、定点/双精度差异、逐节点归因、冲突最小证据、阶段回滚后的轨迹复放、轨迹截断/断链/版本缺失、五类注入无写出、`E_BUSY` 与取消。

样本基线（完整记录见 `samples/baselines.json`）：

| 样本 | 房间/门 | 布局哈希前缀 | 结果 |
| --- | --- | --- | --- |
| `config.json` | 10 / 9 | `cdfbb47ab7eb9f94` | 全部硬约束通过 |
| `small.json` | 4 / 4 | `04d1913d379bd295` | 全部硬约束通过 |
| `medium_chain.json` | 9 / 10 | `9e53ea65b74de54e` | 三把钥匙链通过 |
| `tight_grid.json` | 6 / 6 | `120dcff9f64c9caf` | 全部硬约束通过 |
| `quantized_probe.json` | 1 / 0 | `ebe8435f09da2878` | 定点假/双精度真，已列入误差报告 |
| `conflict_rooms_vs_chain.json` | — | — | `E_CONSTRAINT_CONFLICT`，证据指向 `rooms` 与 `key_chain` |

注入基线见 `samples/injection_baselines.json`。五个模式均返回各自业务码并带有 JSON 定位信息，`blocked_before_output` 均为 `true`。

## 边界说明

- 当前生成器采用“钥匙链分段 + 仅区段内额外门”的拓扑，天然防止绕过锁边界；这是确定性求解策略，不限制数据结构表达其他拓扑。
- 为保持诊断清晰，当前配置最多允许一个 `room_count` 和一个 `key_chain` 约束。
- `evaluation.mode` 默认 `fixed`，即定点结果作为分支判定；可用 `float` 切换。两种模式始终同时计算并报告差异。
- `input_layout` 是入口解析和异常注入验证用的布局夹具，不用于正常生成输出。

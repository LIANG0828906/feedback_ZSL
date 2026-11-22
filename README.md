# TickArena

TickArena 是一套纯本地、确定性的 tick 级多人对战规则引擎，以及独立的对局回放录制/复推校验组件。项目不依赖 Docker、服务端或网络，可在 Windows 原生 Python 3.10+ 和 Linux/WSL 上运行。

交付内容包括：

- 逐 tick 规则引擎：移动、技能、采集、道具、冷却、资源、伤害、暴击/命中波动、死亡复活、比分与胜负锁定。
- 整数定点数值与坐标化确定性随机：没有进程级 RNG，随机判定只由种子、tick、输入事件序号和判定标签派生。
- 交互式 `step()` 与批量 `run_batch()` 两条推演路径，以及逐 tick 自动比对。
- 流式回放录制、格式版本/初始表/事件流指纹、内容哈希、逐 tick 指标与定间隔快照。
- 不使用引擎或录制器代码路径的独立复推校验器，可返回最早偏离 tick、规则项、录制值与复推值。
- 原子、单调、串行化的检查点，支持暂停、取消、恢复；指纹或版本失配时废弃快照并从头推演。
- 有界线程池、可运行示例、确定性事件生成器、自动化测试及实际 Windows/WSL 验证记录。

详细设计见 `docs/DESIGN.md`，线协议见 `docs/FORMAT.md`。

## 环境验证

本次交付实际验证环境：

| 环境 | Python | NumPy | 结果 |
| --- | --- | --- | --- |
| WSL2 Ubuntu 26.04 | 3.14.4 | 2.5.5 | 26/26 测试通过；5,000 + 100,000 tick 示例通过 |
| Windows 原生（PowerShell） | 3.10.9 | 2.2.6 | 26/26 测试通过；5,000 + 100,000 tick 示例通过 |

Windows 入口：

```powershell
cd <repo>
py -3.10 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
.\.venv\Scripts\python -m pip install -e .
.\.venv\Scripts\python -m unittest discover -s tests -v
.\.venv\Scripts\python examples\demo.py
```

Linux/WSL 入口：

```bash
cd <repo>
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m pip install -e .
.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python examples/demo.py
```

`examples/demo.py --quick` 使用 500 / 5,000 tick 快速演示；默认使用 5,000 / 100,000 tick，并把持久化产物写入 `runtime/`。

## 目录

```text
tickarena/
  engine.py       规则状态机、交互/批推进、检查点协调
  verifier.py     独立复推校验器
  replay.py       流式回放写入、格式与哈希校验
  checkpoint.py   原子检查点存储
  events.py       JSONL/内存事件源
  generator.py    确定性事件流生成器
  model.py        初始属性表校验与规范化
  numeric.py      整数定点、规范化 JSON、指纹
  rng.py          (seed, tick, event, purpose) 随机派生
  compare.py      逐 tick 模式比对
  concurrency.py  有界线程池
examples/demo.py
tests/
docs/
```

## 事件生成器

```bash
python -m tickarena.generate_cli \
  --seed 20261123 \
  --players 4 \
  --ticks 100000 \
  --event-density 0.8 \
  --win-score 1000000 \
  --map-size 32 \
  --output runtime/events/large.events.jsonl
```

| 参数 | 含义 |
| --- | --- |
| `--seed` | 无符号 64 位对局种子；决定单位初始布局之外的随机判定 |
| `--players` | 玩家/单位数，生成器支持 2..16，红蓝两队交替 |
| `--ticks` | 对局最大 tick 数，可覆盖数千到数十万 |
| `--event-density` | 每 tick 平均事件数，支持最多 4 位小数 |
| `--win-score` | 队伍击杀比分阈值；设为很大值可完整跑到 tick 上限 |
| `--map-size` | 正方形地图边长 |
| `--output` | 原子写入的 JSONL 事件文件 |

同一组参数产生逐字节相同的事件文件；命令会输出事件数、种子、初始表指纹和事件流指纹。

## 最小 API 示例

```python
from tickarena import Engine, JsonlEventSource, verify_replay

source = JsonlEventSource("runtime/events/small.events.jsonl")
engine = Engine(
    source.match.normalized_initial,
    source,
    replay_path="runtime/replays/small.replay.jsonl",
    snapshot_interval=100,
)

while not engine.done:
    step = engine.step()
    assert step.code == "E_OK"

engine.close()
report = verify_replay(
    "runtime/replays/small.replay.jsonl",
    "runtime/events/small.events.jsonl",
)
assert report.code == "E_OK", report
```

批推进：

```python
while not engine.done:
    result = engine.run_batch(1024)
    if result.code not in {"E_OK", "E_MORE"}:
        raise RuntimeError(result)
```

同一事件流自动比对：

```python
from tickarena import compare_modes, generate_events, make_initial_state

initial = make_initial_state(seed=7, players=4, tick_count=10000)
events = list(generate_events(initial, event_density="1.0"))
result = compare_modes(initial, events, batch_ticks=128)
assert result.code == "E_OK", result
```

## 规则摘要

每 tick 的固定顺序：

1. 到期单位在出生点复活，重置冷却。
2. 存活单位冷却减一，获得被动资源。
3. 按输入文件原始顺序结算该 tick 事件。
4. 达到比分阈值时立即锁定结果；同 tick 后续事件记为 `E_FINISHED`。
5. 到达最后 tick 尚未锁定时，按“比分、总生命、总资源”字典序判胜，仍并列则平局。
6. 生成逐 tick 指标；定间隔生成完整快照。

支持的输入事件：

- `move`：`dx/dy` 整数，受 `move_speed` 与地图边界约束。
- `skill`：检查存活、阵营、技能存在、冷却、资源、射程；结算命中、暴击和整数伤害波动。
- `gather`：获得资源，并按确定性掉落判定获得药水。
- `item`：使用药水，按最大生命比例整数回血。

单条非法事件只拒绝该事件，不终止整局。事件摘要会进入对应 tick 回放，并计入 `rejected_total`。

## 确定性与数值

- 随机判定使用 BLAKE2b 地址化派生：
  `derive_u64(seed, tick, event_index, purpose)`。
- 没有可变的全局随机源；推进路径和线程调度不会改变同一坐标的随机值。
- 所有规则数值均为整数。比例使用 10,000 基点与“半入”整数运算。
- 事件中任意浮点值（包括 `1.0`、NaN、inf）在规则执行前返回/记录 `E_NUM`。
- 批量推进与逐 tick 推进调用同一个 tick 状态机，批次大小只影响调度，不改变结算顺序。

## 回放与独立校验

回放是规范 JSONL：

- `header`：版本、协议、种子、最大 tick、初始表指纹、事件流指纹、快照间隔、事件缓冲上限、完整初始表。
- 每 tick 一条 `tick`：位置、生命、资源、存活、冷却、比分、结果、事件摘要、事件消费偏移。
- 固定间隔一条 `snapshot`：完整单位状态、物品、复活时间、RNG 决策计数、拒绝计数。
- `footer`：正文 SHA-256、记录数、最终 tick、最终状态和关键指标。

`verify_replay()` 先做一次完整格式/哈希扫描，再用独立状态机做第二遍复推。它不复用 `Engine` 或 `ReplayRecorder` 的推演/录制代码路径。任何语义不一致返回：

```text
code=E_DIVERGE, tick=<最早偏离 tick>, rule=<字段路径>,
recorded=<回放值>, recomputed=<独立复推值>
```

版本未知、正文哈希不符、页脚缺失、初始表指纹错误等返回 `E_REPLAY`。

独立命令行：

```bash
python -m tickarena.verify_cli \
  --replay runtime/replays/large.replay.jsonl \
  --events runtime/events/large.events.jsonl
```

成功退出码为 0；偏离或格式错误输出 JSON 诊断并退出 2。

## 检查点与恢复

`CheckpointStore` 每次提交：

1. 在进程内按检查点路径加锁。
2. 拒绝不大于已持久化 tick 的写入，返回 `E_ORDER`。
3. 同目录临时文件写入、`flush`、`fsync`、关闭。
4. `os.replace` 原子替换；Windows 下对短暂文件共享锁做有界重试，不采用非原子的先删后改名。
5. POSIX 下额外 `fsync` 目录；Windows 依赖原子替换语义。

检查点保存：

- 完整动态引擎状态。
- `rng_decisions` 子随机源进度。
- 已消费事件序号与最后已见 tick。
- 事件流指纹、初始表指纹、回放格式版本、引擎资源上限。
- 回放 partial 文件字节数、记录数、正文哈希和最后 tick。

恢复时任何关键指纹或版本失配都会把检查点改名为 `checkpoint.invalid.json`，重置事件源并从 tick 0 新建推演；不会拼接旧回放。

## 状态码

| 编码 | 含义 |
| --- | --- |
| `E_OK` | 成功/校验通过 |
| `E_ORDER` | tick 乱序、过期检查点提交或记录顺序错误 |
| `E_UNIT` | 单位不存在、已死亡或目标非法 |
| `E_COOLDOWN` | 技能仍在冷却 |
| `E_RES` | 资源不足 |
| `E_NUM` | 浮点回退或非法整数 |
| `E_DIVERGE` | 回放/模式复推最早偏离 |
| `E_REPLAY` | 回放格式损坏或版本未知 |
| `E_STATE` | 状态/检查点不可恢复 |
| `E_MOVE` / `E_RANGE` / `E_ITEM` / `E_ACTION` | 移动、射程、道具、动作参数非法 |
| `E_BUFFER` | 单 tick 事件超过缓冲上限，超额事件被拒绝 |
| `E_MEMORY` | 估算内存超过水位 |
| `E_FINISHED` | 结果已锁定后的后续事件 |
| `E_PAUSED` / `E_CANCELLED` | 中断状态 |
| `E_MORE` | 本批完成但整局尚未结束 |

## 并发边界

`BoundedParallel(max_workers, queue_limit)` 同时限制工作线程数和未完成提交任务数。独立对局可并行；单个 `Engine` 必须单线程持有。示例中“100,000 tick 批推进”和“小回放独立校验”在两个工作线程中并行，二者均为 `E_OK`。

## 实际验证结果

完整 WSL2 运行（2026-11-23）：

- 小规模交互模式：5,000 tick；WSL2 耗时 0.2002 s，Windows 原生耗时 0.4912 s，回放校验均为 `E_OK`。
- 大规模批模式：100,000 tick、80,028 条事件、98 批；WSL2 耗时 2.4761 s，Windows 原生耗时 6.3516 s；在 tick 9,216 暂停并恢复；最终校验均为 `E_OK`。
- 大规模回放约 46 MB，逐 tick 记录 100,000 条；回放校验未整体载入内存。
- WSL2 与 Windows 原生生成的 100,000 tick 回放逐字节相同，SHA-256 均为
  `29c71c5bf925ec52ee8460bbf362f69c7be3c6b5eb8233cc78422aa852089e27`；
  事件流 SHA-256 均为
  `0daf2b38f99de78203acef856851db9bcc9b387b57f510b1dfe928e1aa036a32`。
- 异常演示观测到 `E_ORDER`、`E_UNIT`、`E_COOLDOWN`、`E_RES`、`E_NUM`，录制后复验 `E_OK`。
- 取消演示在 tick 39 返回 `E_CANCELLED`，检查点 tick 为 39。
- 全量测试：WSL2 与 Windows 原生均为 26/26 通过。

## 已知边界

- 当前规则模型为“一玩家一单位”，但状态和快照均按单位 ID 扩展。
- 事件源索引保留每条事件的字节偏移，内存为 `O(事件数)`；事件正文按 chunk 读取，不整体驻留。
- 回放按 tick 记录关键曲线，因此大规模回放体积会随 tick 数线性增长。
- 检查点串行化针对同一进程内同一路径；多进程同时写同一检查点应在业务层再加跨进程锁。

# Formation Coupling Lab

面向战术决策题材游戏的多单位编队协同路径规划、逐 tick 推演与增量缓存诊断工作台。
项目完全离线运行，地图、槽位、成员和起终点均由固定种子确定性生成。

## 快速启动（Windows 10+）

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
python -m pip install -r requirements.txt

python -m formation_workbench generate --output samples\formation_scene.json
python -m pytest -q
python run_workbench.py
```

`run_workbench.py` 默认读取 `samples/formation_scene.json`，并将检查点写入
`artifacts/formation_checkpoint.json`。也可以通过 CLI 启动：

```powershell
python -m formation_workbench gui `
  --scene samples\formation_scene.json `
  --checkpoint artifacts\formation_checkpoint.json
```

核心算法只使用 Python 标准库；NumPy 列为环境依赖以符合题设环境，PySide6 仅用于
可视化工作台。无显示器环境可用 CLI 完成全部计算验收。

## 一分钟验收

先生成确定性样本，再执行叶节点编辑、全量一致性与轨迹导出：

```powershell
python -m formation_workbench diagnose `
  --scene samples\formation_scene.json `
  --checkpoint artifacts\formation_checkpoint.json `
  --edit 11,7,4 `
  --compare `
  --export artifacts\trajectory_export.json
```

样本中的 `(11, 7)` 只被 `delta` 路径实际经过。输出应显示：

- `direct_path_members = ["delta"]`
- `alpha/bravo/charlie` 路径与推演区间复用
- `comparison.ok = true`

根成员路径编辑示例 `(13, 9)` 会按队形树失效其后代闭包：

```powershell
python -m formation_workbench diagnose --edit 13,9,8 --compare
```

封锁调整示例：

```powershell
python -m formation_workbench diagnose --blockade blk-crossing:on --compare
```

完整可重复诊断报告：

```powershell
python scripts\run_diagnostics.py
```

结果写入 `artifacts/verification_report.json`。

## 工作台操作

- 点击地图格子后，可把该格代价提交为一次增量编辑；`-1` 表示不可通行并会同步改写为
  不可通行地形标识。
- 选择 `blk-crossing` 等封锁并切换启用状态，可演示区域封锁触发的增量失效。
- `Members` 表展示成员状态、路径缓存 HIT/MISS、重算耗时、推演缓存和当前 tick
  队形偏差；选中成员会高亮其路径。
- `Invalidation` 表展示失效成员、失效范围、原因、复用状态和被失效的 tick 区间。
- tick 滑杆查看每个成员的定点位置、朝向、路径段和瞬时队形偏差。
- `Compare full` 从空缓存执行全量重算，并逐成员、逐路径格、逐拐点、逐 tick 比对。
- `Retry member` 仅重试选中成员及其必要父链；其他成员缓存不被污染。
- `Save checkpoint` / `Load / rebuild` 验证原子快照与版本门禁。
- `Export JSON` 导出路径、拐点、逐 tick 轨迹、队形偏差、依赖图和失效清单。

## 核心语义

- 所有坐标使用 `1/1000` 格定点整数；角度使用 `1/1000` 度；代价为有符号整数，
  累加上界 `2^63-1`，越界返回结构化错误。
- A* 以目的地格代价累加，支持 4/8 邻域和可配置启发式权重。每次搜索先以反向
  Dijkstra 校验启发式，不满足可采纳性时返回 `E_HEUR`，并报告首个高估格。
- 平滑只保留视线可达折线，输出网格坐标和连续世界坐标双读数；原始最小累计代价作为
  下界保留，不允许平滑后报告更低代价。
- 队形采用槽位父子树：子成员相对父成员保持 `slot[child] - slot[parent]` 偏移，
  同时受槽位容差与最大间距约束。失效沿队形树精确向下传播。
- 推演按成员步长与转向速率逐 tick 推进。队形偏差超过软上界时减半步长并记录原因；
  超过硬上界、越线、跳段或瞬转返回 `E_ADV`。
- 代价升高或封锁启用时，只失效实际经过该格/区域的成员。代价降低、不可通行格开放
  或封锁解除时，额外按该成员 A* 实际检查过的搜索依赖格失效，以避免遗漏新出现的
  更优路线，同时仍不做整图或整编队无条件重算。
- 未失效成员的原始路径、平滑路径和推演区间直接复用。失效成员的路径按成员拓扑序
  计算，全部结果在串行提交区一次性原子替换。
- 检查点包含地图哈希、队形/搜索/推演/定点/算法版本、槽位版本、成员路径、推演区间、
  依赖图和稳定提交顺序。任一版本不匹配时整个快照作废并全量重算。

## 项目结构

```text
formation_workbench/
  models.py         固定点模型、地图、队形树、场景
  validation.py     E_SCHEMA / E_RANGE / E_REF 严格校验
  search.py         分层 A*、可采纳性检查、搜索依赖
  smoothing.py      视线平滑、槽位约束、双读数拐点
  simulation.py     逐 tick 协同推演与区间
  engine.py         缓存、失效闭包、全量比对、检查点
  executor.py       有界并行与稳定顺序
  checkpoint.py     跨进程串行、原子快照
  generator.py      确定性本地样本
  gui/              PySide6 地图画布与诊断面板
samples/            本地确定性场景
tests/              验收测试
scripts/            样本生成与诊断报告
docs/               架构与验证记录
```

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_SCHEMA` | 缺字段、类型错误、重复成员、不可通行格缺少地形标识 |
| `E_RANGE` | 数值越界、坐标越界、槽位偏移非有限值 |
| `E_REF` | 成员/槽位/父节点/封锁/地形引用不存在 |
| `E_HEUR` | 启发式高估、搜索展开或代价上界溢出 |
| `E_PARA` | 步长、转向速率、容差、邻域或并发参数越界 |
| `E_FORM` | 平滑违反槽位容差或最大间距 |
| `E_ADV` | 越线、跳段、瞬转、队形崩解或超过 tick 上限 |
| `E_DIVERGE` | 增量缓存与空缓存全量重算首个不一致 |
| `E_NO_PATH` | 成员起终点之间不存在可行路径 |
| `E_IO` | 场景或检查点 I/O 失败 |
| `E_STATE` | 单单元内部状态异常，可由重试隔离 |

## 持久化与强制终止

快照使用同目录临时文件、`fsync` 与 `os.replace` 原子替换，并以线程锁和文件锁串行化
写入。进程被强制终止时，最多丢失正在写入的快照；重启只能依据最近一次完整且版本
匹配的快照恢复。系统不承诺强制终止后仍能产出未落盘的完整轨迹报告。

## 实际验证

当前工作区实际验证环境、命令与结果见 [docs/verification.md](docs/verification.md)。

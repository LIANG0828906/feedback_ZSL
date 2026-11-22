# 技术设计

## 1. 分层

工作台分为六层，所有层只通过不可变模型和显式返回值通信：

1. `generator`：固定种子工程与真值，不依赖 UI。
2. `validation`：把 JSON 解析为强类型模型，并在进入模拟前拦截非法工程。
3. `combat`：纯整数公式区间求值和逐场战斗。
4. `budget → scheduler → state → checkpoint`：资源规划、并行调度、状态推进、原子持久化。
5. `compare → report`：三路径一致性与可读输出。
6. `ui`：只负责展示、触发和导出，不重新实现数值逻辑。

## 2. 工程模型

工程包含：

- `classes`：职业和有序技能 ID。
- `skills`：基础伤害、成长系数、冷却、资源消耗、命中上下界、公式 ID、选择优先级。
- `formulas`：公式 DAG 节点，参数可为技能字段、攻守属性、常量或其他公式。
- `loadouts`：职业与固定点属性。
- `scenarios`：单挑/小队、主配装、队伍、对手、最大回合、种子、单场耗时估计。
- `validation_budget`：槽位、每轮组合上限、总内存缓冲、单场景时长上限。
- `truth`：公式伤害区间、校准胜率带、关键属性阈值。

校验顺序经过专门安排，使错误码稳定：

1. 根字段和数组结构。
2. 职业、技能、公式、配装逐个解析。
3. 技能/职业引用、公式引用、公式环检测。
4. 配装属性、预算边界。
5. 场景引用与主配装一一对应。
6. 真值结构和引用。

非法工程在 `parse_project()` 阶段抛 `BalanceError`，不会创建调度器。

## 3. 定点公式

每个公式返回 `(min, max)` 整数区间：

- `MUL_BP(a,b) = a*b//10000`
- `DIV_BP(a,b) = a*10000//max(1,b)`
- `ADD/SUB/MAX/MIN` 做区间传播，`SUB` 下限钳制到 0。

技能伤害先由 DAG 得到基础区间，再按场景种子/回合派生的命中因子采样。实际伤害：

```text
raw = raw_min + hash32(seed, round, side) % (raw_max - raw_min + 1)
hit = hit_min_bp + hash_hi32(seed, round, side) % (hit_max_bp - hit_min_bp + 1)
damage = raw * hit // 10000
```

资源、生命和累计伤害均为整数。线程调度和进程批次不会改变单场输入，因此逐回合结果可重复。

## 4. 战斗流程

每回合：

1. 存活单位恢复资源，封顶到 `resource_max`。
2. A 方按配装顺序、B 方按配装顺序行动。
3. 每个单位按技能优先级选择首个冷却完成且资源足够的技能。
4. 目标为敌方首个存活单位。
5. 扣除资源、设置冷却、结算伤害并记录动作区间与精确伤害。
6. 回合结束递减冷却；一方全灭即结束。
7. 达到最大回合时按剩余生命比例判定。

小队场景使用相同规则，多单位按声明顺序行动。

## 5. 候选方案

候选修改只作用于主验证的 A 方主配装/技能表，B 方保持基准。修改支持：

- 技能字段：`base_damage`, `growth_coeff`, `cooldown`, `resource_cost`
- 配装字段：`hp`, `attack`, `defense`, `resource_max`, `resource_regen`, `speed`
- 操作：`scale_bp` 或 `add`

构造 `CandidateRuntime` 时生成独立数值表，原工程不被修改。已提交结果保存在冻结状态机中，后续探针不会覆盖。

## 6. 预算规划

组合顺序：候选优先级降序 → 候选声明顺序 → 场景声明顺序。

每个组合：

```text
per_thread_buffer_kb = memory_buffer_kb // parallel_slots
memory_cap = (buffer_bytes - overhead) // bytes_per_sample
time_cap = max_scenario_seconds*1000 // estimated_ms_per_match
hard_cap = min(target_samples, memory_cap, time_cap)
```

分配：

1. 最小覆盖。
2. 逐轮公平扩样，直到目标、硬上限或总预算。
3. 被削减组合记录内存/时间/总预算原因。
4. 槽位和每轮上限只影响派发波次。

预算决策是纯函数，因此实时路径与离线重放完全一致。

## 7. 并行调度

- `ThreadPoolExecutor(max_workers=parallel_slots)`，默认最多 8。
- 每轮提交至多 `max_matches_per_round` 个组合任务。
- 任务执行完成但按计划顺序收集、提交，保证提交顺序稳定。
- 队列剩余组合阻塞到下一轮，不丢弃。
- `actual_peak_slots` 由原子活跃计数记录；串行覆盖参数可用于一致性回归。

## 8. 状态机

方案：

```text
PENDING → EVALUATING → SUBMITTED
                     ↘ FAILED
```

组合：

```text
PENDING → SIMULATING → SUBMITTED
                     ↘ FAILED
```

提交和失败状态为终态。非法迁移抛 `E_STATE`，当前状态不变。已提交组合保存结果哈希，重复提交或调整均被拒绝。

## 9. 检查点

SQLite 表 `commits` 追加写入：

- `seq`：稳定提交顺序。
- `kind`：`combo` 或 `candidate`。
- `payload`：完整组合结果或方案摘要。
- `content_hash`：payload 规范 JSON 哈希。
- 完整版本信封：工程哈希、候选集哈希、预算版本、算法版本、schema 版本。

写入使用进程内锁和 `BEGIN IMMEDIATE`，保证串行且原子。加载时逐行校验版本和哈希；任何异常都会在一个事务内删除所有行。候选提交晚于其全部组合提交，加载还会验证该顺序，避免半数恢复。

## 10. 三路径比对

比较按预算计划顺序进行，逐组合、逐采样、逐回合比较：

- 采样数
- 胜者
- A→B / B→A 回合伤害
- 完整动作序列
- 聚合胜负与置信区间

首个差异返回：

```json
{
  "candidate_id": "candidate.baseline",
  "scenario_id": "scene.duel.iron",
  "sample_index": 0,
  "round_index": 3,
  "category": "damage_a_to_b"
}
```

## 11. 界面

PySide6 主窗口包括：

- 工程选择与内容哈希。
- 依赖图：按职业过滤，滚轮缩放、拖拽平移。
- 候选表：状态与胜率。
- 胜率图：观测条、95% CI、真值带。
- 伤害曲线：A/B 双方逐回合均值。
- 预算页：槽位/采样/排队水位、决策序列、组合采样明细。
- 逐方案页：场景统计、阈值命中。
- 报告页：Markdown 预览与 JSON/Markdown 导出。
- 系数探针：选择技能、字段和百分比，独立评估新候选。

运行、探针和三路径校验均使用 `QThread`，避免阻塞界面。暂停事件在调度轮边界生效；应用关闭时请求暂停并等待有限时间。


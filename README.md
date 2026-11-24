# 本地多人对战战斗数值平衡验证管线

这是一个仅依赖 Go 标准库的确定性战斗数值验证工具。它会生成固定种子的职业、技能、公式 DAG、配装、场景、候选方案与预算，完成定点结算、受限并行调度、预算取舍、双层状态机、原子检查点、报告和独立校验。

## 快速开始

要求 Go 1.22 或以上。

```bash
go build ./...
go vet ./...
go test ./...

go run ./cmd/balance generate --out artifacts/demo/project.json --seed 42
go run ./cmd/balance validate --project artifacts/demo/project.json
go run ./cmd/balance demo --dir artifacts/demo --seed 42
```

`demo` 会重新生成工程，并依次执行：

1. 一次性全量模拟。
2. 带检查点的实时模拟。
3. 仅提交 3 个组合后暂停，再从检查点续跑到完成。
4. 使用 `CompareReports` 自动比对全量/实时、全量/恢复两条路径。
5. 仅读取工程清单和检查点，执行独立验证。

Windows PowerShell 使用相同的 `go` 命令；代码不使用平台专用系统调用或外部数据。

## 目录

| 路径 | 内容 |
| --- | --- |
| `generate.go` | 固定种子确定性工程生成器 |
| `validate.go` | JSON 字段存在性、结构、引用、环、范围校验 |
| `formula.go` | 定点公式求值、候选调整与伤害区间准备 |
| `simulate.go` | 逐场定点战斗、冷却/资源/命中/复活 |
| `budget.go` | 槽位/轮次/内存预算、优先级取舍、CI 与削减审计 |
| `engine.go` | 逐轮并行调度、重试、确定顺序提交、恢复编排 |
| `state.go` | 方案级与组合级双层状态机及冻结载荷 |
| `checkpoint.go` | 原子快照、版本校验、串行提交 |
| `report.go` | 报告、阈值、伤害真值区间和三路径差异定位 |
| `verify.go` | 独立检查点验证器 |
| `cmd/balance/main.go` | CLI |
| `artifacts/demo/` | seed=42 的实际验收输出 |

## 定点与确定性

- 所有战斗数值使用 `int64` 定点整数，`FixedScale=1000`，即 JSON 中 `1000` 表示 `1.000`。
- 百分比使用 BPS（分母 `10000`），统计使用 PPM（分母 `1_000_000`）。
- 乘法/除法通过大整数中间值计算后检查 `int64` 范围，不产生浮点累积误差。
- 单场命中波动只由以下三元组派生：全局工程种子、场景实例标识、行动回合编号。场景实例标识为 `scenarioId#matchIndex`。
- 不使用线程局部随机、时间或调度顺序作为随机输入。比赛被调度到任何槽位、任何轮次，结果都不变。
- 置信区间使用 Hoeffding 上界，宽度只依赖样本数，因此随样本增加单调不增（允许相等）。

## 工程结构

工程 JSON 顶层字段：

`schemaVersion`、`seed`、`algorithmVersion`、`budgetVersion`、`classes`、`skills`、`formulas`、`builds`、`scenarios`、`plans`、`thresholds`、`budget`。

核心约束：

- `skills[].formulaId`、`skills[].classId`、`builds[].rotation[]`、`scenarios[].leftBuilds/rightBuilds[]`、`plans[].targetScenarios[]`、`plans[].adjustments[].skillId` 必须完整引用。
- 每个 `build` 必须恰好被一个场景的一侧使用，形成场景/配装一一对应。
- `duel` 场景每侧恰好 1 个 build；`squad` 场景每侧至少 1 个 build。
- 公式节点支持 `const`、`attr`、`skill_base`、`skill_growth`、`add`、`sub`、`mul`、`div`、`min`、`max`、`formula`。
- 所有公式依赖边（`inputs` 和 `formula.ref`）必须无环。
- 成长系数范围 `0..10.000`，资源消耗/基础伤害不得为负，命中波动范围限制在 `[-5000,5000]` BPS。
- 预算字段必须非负；实际运行要求槽位、每轮上限、内存缓冲、轮次和目标样本均为正。

默认 seed=42 的工程包含 3 个职业、5 个技能、25 个公式节点、6 套配装、2 个场景和 3 个候选方案。

## 结算规则

- 每个存活单位按配装 `rotation` 选择第一个“冷却已结束且资源足够”的技能。
- 资源在每回合开始恢复并截断到上限；使用技能时立即扣减。
- 技能冷却按回合数推进；冷却中的技能不会被正常选中。
- 每个组合的每个样本拥有独立的场景实例标识，命中波动在技能的 `hitVarianceMinBps..hitVarianceMaxBps` 内确定派生。
- 单位 HP 归零后死亡；场景 `reviveRounds > 0` 时，在指定回合后以半血复活并清空冷却。
- 一方所有单位永久死亡时结束；达到最大回合时按存活人数、剩余 HP、总伤害依次判定，仍相同则为平局。
- 每场报告保存逐回合左右方伤害序列、伤害总量、伤害区间、复活次数和 SHA-256 摘要。

## 预算与取舍

默认预算故意小于完整目标：

| 参数 | 值 |
| --- | ---: |
| 模拟槽位 | 4 |
| 每轮最多场次 | 6 |
| 每线程缓冲 | 2 |
| 最大预算轮次 | 10 |
| 每组合目标样本 | 8 |
| 最大总场次 | 36 |
| 优先级规则 | `priority_first` |

有效每轮容量为 `min(maxMatchesPerRound, slots * memoryBuffer)`。默认目标需要 `3 plans * 2 scenarios * 8 = 48` 场，但总容量只有 36 场，因此必须削减。

支持的声明式规则：

- `priority_first`：先保证每个组合至少 1 场，再把剩余容量优先分配给高优先级方案，低优先级组合被削减并记录原因。
- `equal_coverage`：先保证覆盖，再在所有组合间均匀增加/削减样本。

每个组合报告实际请求数、分配数、完成数、逐轮累计样本、胜率、CI 宽度、相对上轮变化、削减原因与规则。`ValidateBudgetReductions` 会拒绝没有规则说明的重复削减并返回 `E_BUDGET`。

## 状态机与检查点

方案状态：`pending -> running -> committed|failed`，`failed -> running` 仅用于显式重试。

组合状态：`pending -> running -> committed|failed`，`failed -> running` 仅用于显式重试。

非法迁移返回 `E_STATE`，状态保持不变。已提交组合的载荷不可再改写；检查点也不允许覆盖同一组合。

检查点特征：

- 每个组合和方案提交后写出整份快照，而不是拼接可能交错的日志。
- 写入使用同目录临时文件、`fsync`、原子 `rename`，进程内由互斥锁串行化。
- 快照包含工程内容哈希、预算哈希、预算版本、算法版本、单调提交序号、逐场摘要、胜率统计、随机源进度和方案摘要。
- 启动新的实时运行或恢复时发现快照无效，会先写入空快照，避免旧结果残留。
- 恢复时任何工程哈希或版本不匹配都会丢弃整份快照并全量重算，不进行部分混用。
- CLI `run` 捕获 `Ctrl+C`/`os.Interrupt`；取消后只保留已提交结果，未完成组合不会写入检查点。
- 用户取消或强制终止后，只能恢复已原子持久化的提交；工具不承诺终止时生成完整报告。

## CLI

```bash
# 生成与校验
go run ./cmd/balance generate --out project.json --seed 42
go run ./cmd/balance validate --project project.json

# 全量、实时、断点恢复
go run ./cmd/balance run --project project.json --mode full --report full.json
go run ./cmd/balance run --project project.json --mode realtime \
  --state checkpoint.json --report realtime.json
go run ./cmd/balance run --project project.json --mode realtime \
  --state checkpoint.json --stop-after 3 --report partial.json
go run ./cmd/balance run --project project.json --mode resume \
  --state checkpoint.json --report resumed.json

# 三路径自动比对与独立验证
go run ./cmd/balance compare --expected full.json --actual resumed.json
go run ./cmd/balance verify --project project.json --state checkpoint.json
```

故障注入与预算覆盖：

```bash
go run ./cmd/balance run --project project.json --mode full \
  --inject resource --inject-plan baseline --inject-scenario duel \
  --inject-match 1 --inject-turn 2 --report injected.json

go run ./cmd/balance run --project project.json --mode full \
  --budget-slots 1 --budget-max-per-round 4 --budget-memory 1 \
  --budget-max-rounds 5 --budget-target 8 --budget-max-total 20 \
  --budget-priority equal_coverage
```

故障类型：`unit`、`cooldown`、`resource`、`internal`。前三种会立即进入对应错误链路；`internal` 首次尝试失败、第二次重试成功，以验证单场隔离。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_SCHEMA` | 字段缺失、未知枚举、引用缺失或结构不满足 |
| `E_CYCLE` | 公式依赖图存在环 |
| `E_RANGE` | 定点值、预算值或计算溢出越界 |
| `E_BUDGET` | 容量不足、优先级冲突、重复无说明削减或 CI 单调性冲突 |
| `E_STATE` | 非法状态迁移或修改已冻结结果 |
| `E_DIFF` | 三路径自动比对发现首个不一致 |
| `E_UNIT` | 技能/单位引用缺失 |
| `E_COOLDOWN` | 冷却未结束仍尝试施放 |
| `E_RES` | 资源不足仍尝试消耗 |
| `E_INTERNAL` | 单场内部异常且重试耗尽 |
| `E_CHECKPOINT` | 检查点结构/写入问题 |
| `E_IO` | 本地文件读写失败 |

运行时错误包含 `planId`、`scenarioId`、`matchIndex`、`turn` 等定位字段。

## 独立验证

`verify` 只读取工程 JSON 与检查点 JSON，不依赖运行内存、报告文件或调度队列。它会重新计算：

- 每个组合的胜/负/平、胜率和 CI。
- 每个组合的预算分配、完成场次和削减记录。
- 方案级聚合、提交序号唯一性和方案间提交关系。
- 逐场伤害序列总量、伤害摘要、真值区间、随机源进度和摘要哈希。
- 总预算消耗是否超过容量。

验证失败时输出 `firstPlanId`、`firstItem` 和违规列表；CLI 退出码为非零。

## 实际验收记录

本工作区已执行：

```text
go build ./...      通过
go vet ./...        通过
go test ./...       通过
go test -race ./... 通过
GOOS=windows GOARCH=amd64 go build ./... 通过
go run ./cmd/balance demo --dir artifacts/demo --seed 42
```

seed=42 的实际 `artifacts/demo/summary.json` 结果：

| 项目 | 结果 |
| --- | --- |
| 工程哈希 | `c63b65d3000ec6eded6f647cae4d5a01f0fe5f615e39cdbdd46cfed08390c0af` |
| 全量/实时逐场一致 | `true` |
| 全量/断点恢复逐场一致 | `true` |
| 独立验证 | `ok=true`，无违规 |
| 提交序号 | 9（6 个组合 + 3 个方案） |
| 预算请求/分配/完成 | 48 / 36 / 36 场 |
| baseline 胜率 | 56.25%（单挑 4W4L，小队 5W3L） |
| baseline 阈值 | `win_rate_ppm`、`avg_damage`、`max_ci_width_ppm` 全部命中 |
| 削减组合 | `burst_control/duel` 从 8 减到 3，`burst_control/squad` 从 8 减到 1 |
| 样本复活覆盖 | 17 场出现至少一次复活 |
| 异常链路实测 | `E_RES plan=baseline scenario=duel match=1 turn=2` |

生成的完整报告、部分报告、恢复报告、两份检查点、独立验证摘要、注入报告与错误信封均在 `artifacts/demo/`；对应文件为 `verification.json`、`injection-report.json`、`injection-result.json`。

## 边界说明

- 默认样本的 CI 使用 5% Hoeffding 保守上界；8 场样本时宽度会被 100% 上限截断是预期行为。
- 检查点以组合/方案为提交粒度，不保存“模拟中”状态。强制终止后未提交组合会重新计算。
- 该工具面向本地确定性样本和标准库文件存储，不包含网络、数据库、容器或外部服务。

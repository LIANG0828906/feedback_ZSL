# 技能冷却与资源预算编排内核

这是一个可独立集成的 Go 领域组件，覆盖实时对战技能的规则加载、单位换算、
逐 tick 冷却与资源结算、同 tick 争用裁决、依赖失效、SQLite 窗口检查点和
确定性诊断。仓库不依赖容器、外部数据集或在线服务，样本、意图和真值均由
固定种子生成器在本地产出。

## 快速开始

要求 Go 1.22 或更高版本。SQLite 使用纯 Go 驱动 `modernc.org/sqlite`。

```bash
go mod tidy
go build ./...
go test ./...

# 本地确定性验收：三版本、往返、逐 tick 真值、三路径、失效与恢复
go run ./cmd/skilldemo verify -seed 42

# 生成 v1/v2/v3 工程、意图序列和独立真值
go run ./cmd/skilldemo generate -seed 42 -dir artifacts/seed-42

# 本地回环诊断页；拒绝 0.0.0.0 等非 loopback 地址
go run ./cmd/skilldemo serve -seed 42 -addr 127.0.0.1:0
```

诊断页包含 `/`、`/api/report` 和 `/healthz`，只监听回环地址，不加载外部资源。

## 目录

| 路径 | 内容 |
| --- | --- |
| `loader.go` | 三格式版本检测、严格必填字段检查、统一规则模型 |
| `export.go` | 升降级、补齐痕迹、`E_LOSSY` 清单与回读校验 |
| `units.go`, `expr.go` | tick/ms、千分比/万分比、整数换算和表达式树 |
| `engine.go` | 逐 tick 资源、冷却、施法、可用状态和争用裁决 |
| `deps.go` | 依赖图、按编辑 tick 的失效计划、编辑感知缓存键 |
| `windowfsm.go` | 待结算、结算中、已提交、失败状态机 |
| `checkpoint/` | SQLite 原子检查点、串行提交、三路径执行器 |
| `samplegen/` | 固定种子工程、意图和独立构造真值 |
| `diagnostics/` | 验收报告和 loopback HTTP 页面 |
| `cmd/skilldemo/` | 本地演示 CLI |

## 内部规则模型

所有版本先映射为同一内部模型：

- 时间统一为 **tick**；工程显式声明 `tickMillis`。
- 资源金额、成本和缩放系数统一为 **1/10000** 定点整数。
- 冷却缩减系数为 `cdrBp`，范围 `[0,10000]`。
- 冷却与资源回复统一为表达式树；旧版的扁平常量会映射为 `const`。
- 数组按标识排序，任何输出都不依赖 map 遍历顺序。

### 三格式差异

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 字段命名 | `snake_case` | `camelCase` | 紧凑名、嵌套对象 |
| 时间单位 | tick | 毫秒 | tick |
| 资源刻度 | 千分比 | 万分比 | 万分比 |
| 冷却缩减字段 | `cooldown_reduction_permille` | `cdrBp` | `cdrBp` |
| 回复/冷却 | 扁平常量 | 扁平常量 | 表达式树 |
| 可选字段 | 最小集合 | `enabled`、`description` | `enabled`、`description`、`tags`、`group`、`conflictPolicy` |

版本检测分别读取 `schema_version`、`schemaVersion`、`format=skill-rules + version`。
未知版本或必填字段缺失返回 `E_VERSION`，`Error.Path` 指向具体字段，例如
`skills[0].cooldown_ticks`。

### 换算与往返

`MulDivRounded` 与 `MulDivExact` 全程使用整数和任意精度中间值，没有浮点累积。
声明取舍方向支持 `floor`、`ceil`、`nearest`、`trunc`。溢出返回 `E_OVER`，并携带
`Field` 和 `Unit`。

导出到低版本时要求值可精确表达；不可精确落入目标单位/刻度时返回 `E_LOSSY`
而不是静默舍入。v3 → v2 → v3 和 v3 → v1 → v3 的样本往返均恢复相同内部语义。

表达式树降级规则：

- 无 `tick` 变量的表达式可安全折叠为低版本扁平常量，并记录 `fold-to-flat`。
- 含 `tick` 变量、无法用低版本字段表达时，返回 `E_LOSSY` 且不产出误导性文件。
- `tags`/`description` 等非语义字段可丢弃，损失项记录 `drop-optional`。
- 会改变可用性的损失（例如 v1 无法表达 `enabled=false`）直接拒绝导出。

升级默认值补齐会写入 `FillTrace`，包含字段路径、旧值 `<absent>`、默认值和
默认值来源。导出产物会立即被对应版本加载器回读，并校验冷却/资源语义。

## 逐 tick 语义

每个 tick 固定按以下顺序执行：

1. 每个资源池按回复表达式增加资源，并截断到 `max`。
2. 所有技能的冷却剩余和施法剩余各减一。
3. 收集该 tick 的意图，按 `priority` 降序、`skillId` 升序、`sequence` 升序、
   `targetId` 升序稳定排序。
4. 顺序做前置校验：禁用、冷却/施法中、共享组已使用、资源不足。
5. 成功施放时扣除资源，按 `base * (10000-cdrBp) / 10000` 计算有效冷却，
   设置施法与冷却计时；共享组成员同步获得该冷却。
6. 输出排序后的资源、技能状态和裁决明细。

技能 `available` 表示同时满足：启用、冷却为 0、施法剩余为 0、当前资源足够。
`timersReady` 与 `affordable` 分别暴露前两类原因，便于诊断。

默认冲突策略是 `priority_then_id`，因此任何冲突都有稳定裁决。工程使用
`strict_priority` 时，同 tick 争用同一资源池或共享组且优先级相同的意图会返回
`E_CONFLICT`，携带 tick、争用类型和排序后的争用技能集合。引擎先克隆调用方
状态，错误路径不会污染原状态。

## 依赖与缓存

依赖图显式连接：

- 冷却缩减系数 → 技能冷却；
- 资源回复表达式 → 资源预算；
- 技能冷却、资源预算、共享组 → 每个 tick 的状态和裁决；
- tick 状态 → 下一 tick 状态，裁决 → 下一 tick 资源/组状态；
- 所有 tick 状态与裁决 → tick 快照。

`PlanInvalidation` 从编辑 tick 开始遍历下游节点，只生成该 tick 之后的区间；
`ReusableWindows` 可直接对应到持久化窗口缓存。`EditAwareWindowKeys` 会保留未
受影响窗口的旧缓存键，只替换受影响窗口的键，因此规则编辑后仍能复用 SQLite
中的历史窗口结果。

## SQLite 检查点

窗口状态机：

```text
pending -> settling -> committed
                    \-> failed -> settling   (合法重试)
```

已提交状态为终态。非法迁移返回 `E_STATE` 并保持原状态。检查点表保存：

- 窗口前后运行时状态；
- 每 tick 技能可用状态、资源余量、冷却与施法剩余；
- 裁决明细及 SHA-256 摘要；
- 规则内容哈希、源版本、算法版本；
- 尝试次数、状态和失败原因。

存储使用单 SQLite 连接、进程内写互斥锁和事务，提交按窗口串行。某个窗口发生
错误或 panic 时只将该窗口置为 `failed`；已提交窗口不被改写，后续可合法重试。

恢复时校验窗口连续性、状态链、规则哈希、源版本和算法版本。任一项不匹配会
删除该 run 的全部窗口并从头全量重算，结果标记 `Invalidated=true`。

## 三路径与并发边界

`checkpoint.Runner` 提供：

- `RunRealtime`：按窗口推进并逐窗提交；
- `RunResume`：读取已提交前缀，从最后一个检查点继续；
- `RunFull`：不读检查点，一次性全量重算。

三条路径使用同一引擎和同一参数，测试逐 tick 比较技能可用状态、资源、冷却与
裁决。`CompareSnapshots` 自动定位首个不一致 tick、技能标识和字段名。

资源回复与有效冷却计算等纯任务进入有界 worker pool，最大并行度限制在 `[1,64]`，
每个 worker 的排队缓冲由 `BufferLimit` 控制。队列满时生产者阻塞并记录
`worker buffer full; producer blocked instead of dropping work`，不会丢任务。
具有状态依赖的 tick 顺序和窗口提交始终保持串行，以保证 tick 序稳定。

## 样本与真值

`samplegen.Generate(seed)` 使用自实现的 SplitMix64，生成：

- 同一语义的 v1/v2/v3 工程；
- 带目标选择的按键意图序列；
- 每个 tick 的预期资源、技能状态与裁决。

真值由生成器内部独立状态机直接构造，不调用内核，也不从内核输出反推。三版本
均可归一为相同 `ContentHash`。固定种子样本已生成在 `artifacts/seed-42/`：

```text
rules-v1.json
rules-v2.json
rules-v3.json
intents.json
truth.json
manifest.json
```

## 集成示例

```go
import (
    skillcore "skillbudget"
    "skillbudget/checkpoint"
)

project, err := skillcore.LoadFile("artifacts/seed-42/rules-v3.json")
engine, err := skillcore.NewEngine(project)
result, err := engine.Run(nil, intents, 28, skillcore.SimOptions{
    SampleEvery: 1,
    Workers:     4,
    BufferLimit: 1,
})

store, err := checkpoint.Open("local/checkpoint.db")
runner := checkpoint.NewRunner(store)
run, err := runner.Run(ctx, project, intents, checkpoint.RunResume, checkpoint.RunConfig{
    RunID:       "match-42",
    WindowSize:  7,
    TotalTicks:  28,
    Workers:     4,
    BufferLimit: 1,
})
```

错误可使用 `skillcore.IsCode(err, skillcore.CodeVersion)` 等判断，并读取
`*skillcore.Error` 的结构化字段。

## 实际验证结果

本次在 Linux amd64、Go 1.22.12 下执行：

```text
$ go build ./...
$ go test ./...
ok  skillbudget
ok  skillbudget/checkpoint
ok  skillbudget/diagnostics
ok  skillbudget/samplegen

$ go run ./cmd/skilldemo verify -seed 42
skillbudget verification seed=42 ruleHash=caa7797dfe7136b62449bc46857621ab71ca9d81fc9b56c466268dc66d7b4a82 ticks=28 windows=4 intents=29 decisions=29
  [PASS] version-1 semantic hash
  [PASS] version-1 truth
  [PASS] version-2 semantic hash
  [PASS] version-2 truth
  [PASS] version-3 semantic hash
  [PASS] version-3 truth
  [PASS] roundtrip v3->v2->v3                 losses=9 fillTraces=6
  [PASS] roundtrip v3->v1->v3                 losses=13 fillTraces=16
  [PASS] path full recompute
  [PASS] path realtime checkpoints
  [PASS] path resume from SQLite
  [PASS] three paths identical                resumeTick=14 resumedWindows=[0 1]
  [PASS] precise dependency invalidation      reusable=[0 1] affected=[2 3]
overall: PASS

$ go run ./cmd/skilldemo serve -seed 42 -addr 127.0.0.1:0
diagnostics: http://127.0.0.1:<ephemeral-port>/
GET /healthz -> ok
```

另有测试覆盖失败窗口合法重试、panic 隔离、源版本和规则哈希失配整体作废、
缓存跨进程重开复用、非法状态迁移保持原状态，以及多窗口并发迁移最终串行提交。

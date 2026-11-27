# SQL PlanOpt Kernel

接入列式存储引擎前的 SQL 逻辑计划验收内核：确定性样本生成、三个 schema 版本装载、规范化/谓词下推/列裁剪、定点选择率、代价与连接顺序、参数敏感性、增量重优化、有界并行以及崩溃安全检查点。

模块只依赖 Go 1.22 标准库，没有第三方依赖，也不需要网络下载。环境中实际使用的是 `go version go1.22.12 linux/amd64`。

## 快速验证

```bash
go version
go test ./...
go test -race -count=1 ./...
go run ./cmd/plandemo -seed 42 -out artifacts/demo/generated
```

演示命令会：

- 生成固定种子 `42` 的工程与解析真值；
- 输出 v1/v2/v3 JSON 到 `artifacts/demo/generated/engineering-kernel-v{1,2,3}.json`；
- 校验三版本的节点集合、列引用顺序和参数类型序列一致；
- 校验规范化算子序列、列集合、选择率容差、连接顺序和代价真值；
- 演示全局编号与每语句相对编号的绑定敏感性等价；
- 连续执行表列统计、谓词条件、算子参数三类增量编辑，并逐次与全量重优化 `reflect.DeepEqual`；
- 验证并行结果确定性、worker/算子缓冲背压码；
- 写入原子文件检查点并验证“计划状态先落盘、全局序号后推进”；
- 注入畸形节点树、占位符越界、刻度越界、截断检查点和版本不匹配快照；
- 写出完整 JSON 报告：`artifacts/demo/generated/demo-report.json`。

最近一次本地验证：

- `go test ./...`：`ok planopt`，`planopt/cmd/plandemo` 无测试文件但编译通过。
- `go test -race -count=3 ./...`：通过。
- `go run ./cmd/plandemo`：退出码 `0`，6 个计划、3 个增量步骤、6 个检查点状态全部校验通过。

## 三版本 Schema

| 版本 | 标识 | 字段风格 | 节点表达 | 参数编号 | 刻度/单位 |
| --- | --- | --- | --- | --- | --- |
| v1 | `cpq.plan_engineering/v1` | snake_case | `flat_nodes` 扁平节点表 | 工程全局绝对编号 | permille、tick、字节、有指纹 |
| v2 | `2.0` | camelCase | `nodeGroups[].nodeTrees` 递归树 | 每语句相对位置 | permyriad、毫秒、KiB、无指纹 |
| v3 | `3.0` | kebab-case | `operator-buffer` + `inputs` | 显式全局/本地编号 | permyriad、tick、字节、有指纹 |

入口为：

```go
project, err := planopt.LoadEngineeringJSON(data)
```

生成器文档可分别通过 `planopt.MarshalV1`、`planopt.MarshalV2`、`planopt.MarshalV3` 得到。仓库中已有的
`artifacts/demo/generated/engineering-v1.json` 与 `engineering-v2.json` 也有兼容装载测试。

## 规范化与语义约束

`NormalizeProject` / `NormalizePlan` 会：

- 合并相同 `compound_id` 的重复谓词，嵌套深度不参与重复乘算；
- 消除 `SUBQUERY` 包装并按依赖拓扑展开为扁平算子序列；
- 将单表谓词下推到合法的 scan/filter 区间；
- 按 OUTPUT/JOIN/PREDICATE 引用计算读取列、输出列和裁剪列；
- 保证全局未引用列不出现在任何规范化算子的 `OutputColumns` 中。

外连接约束是显式实现的：`LEFT OUTER JOIN` 的右侧可空谓词不能穿过外连接下推，稳定原因码为
`LEFT_OUTER_NULLABLE_RIGHT_SIDE_BLOCKED`。样本计划 5 专门覆盖这一点。

## 定点选择率与代价

选择率统一使用 64 位有符号整数和 `1_000_000` PPM 定点刻度，不使用浮点数参与估算：

- `EQ`：优先使用直方图桶的 `row_count / distinct_count`；
- `GT/GTE/LT/LTE/BETWEEN`：按桶边界和桶内离散值跨度做整数加权；
- 命中 `upper_bound` 时记录 `bucket_boundary=true`；
- permille 与 permyriad 分别按 `×10`、`PPM = permyriad × 100` 转换；
- 声明容差为 `planopt.SelTolerancePPM = 5000`，即 0.5 个百分点。

代价为整数 milli 单位，包含：

- 顺序扫描的原始行数；
- 索引访问的选择后行数和 seek 成本；
- 列裁剪后的读取宽度；
- join CPU 与中间结果宽度/行数。

连接顺序枚举所有连通且满足外连接方向的排列。代价位于声明的 1% tie band 内时，按表 ordinal 的声明顺序固定裁决，并在
`JoinCandidate.DecisionBasis` / `OptimizedPlan.TieBreakReason` 记录依据。

## 参数敏感性

`CompareBindings` 对 before/after 两组绑定分别优化，输出每个计划：

- 连接顺序是否变化；
- 访问路径是否变化；
- 导致选择率变化的谓词 ID；
- 变化前后代价、差额和绝对差。

`GlobalRelativeBindingEquivalence` 用同一组参数分别以全局绝对编号和 `map[planSeq]map[localPosition]value`
相对编号执行，要求结论完全一致。演示样本中把计划 6 的 `order_id` 阈值从默认 `11000` 改为 `1000`，会稳定触发
`pk_orders -> seq_orders` 的访问路径切换。

## 增量重优化

`IncrementalEngine` 维护以下依赖键：

- `table:<table>:stats`
- `predicate:<seq>:<id>`
- `node:<seq>:<id>`
- `normalize:<seq>`
- `estimate:<seq>:<predicate>`
- `tablesel:<seq>:<table>`
- `path:<seq>:<table>`
- `optimize:<seq>`

每次编辑返回 `IncrementalStep`，包含失效来源、反向依赖传播闭包、失效/复用缓存键以及受影响算子区间。未出现在闭包中的计划和键保留旧缓存；受影响计划重算后，结果与编辑后工程的一次性 `OptimizeProject` 做深度比较。

## 并行与背压

`Scheduler` 通过显式 worker channel 限制并发，并通过 `MaxOperatorsPerPlan` 限制单计划算子缓冲。

- `TryProcessPlan` 在 worker 耗尽时立即返回 `E_BACKPRESSURE_WORKER_BUDGET`；
- 原始节点数或规范化算子数超过缓冲时返回 `E_BACKPRESSURE_BUFFER`；
- `RunAll` 按 `plan_seq` 写回固定下标并排序结果，goroutine 调度顺序不影响输出；
- 超大计划产生确定的错误 outcome，不会静默丢失。

## 检查点

标准库没有内置可直接导入的 SQLite 驱动；按任务说明采用标准库文件持久化替代。检查点目录包含：

- `plans/plan-XXXX.json`：单个计划的规范化结果、优化结果和 SHA-256；
- `checkpoint-manifest.json`：格式版本、`committed_seq`、已提交计划列表和重优化缓存。

提交顺序固定为：

1. 临时文件写入计划状态；
2. `fsync` + 原子 `rename`；
3. 更新包含全局提交序号和缓存的 manifest；
4. 再次原子替换。

如果进程在步骤 2 后、步骤 3 前终止，新计划文件只是 orphan state；恢复只读取 manifest 中已提交的序号，因此不会读到未完成计划。格式版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`，调用方应整段重优化；manifest/plan 被截断或校验失败返回 `E_CHECKPOINT_TRUNCATED`。

`FileCheckpoint.RecoverOrRebuild` 已固化版本策略：版本匹配时返回检查点结果；版本不匹配时自动执行全量重优化并重新提交当前版本检查点；截断错误仍保持可观测，不做静默丢弃。

## 稳定业务码

| 代码 | 含义 |
| --- | --- |
| `E_SCHEMA_UNKNOWN` | 无法识别 schema envelope/version |
| `E_UNRECOGNIZED_FIELD_COMBINATION` | 严格字段或字段组合无法识别 |
| `E_MALFORMED_NODE_TREE` | 节点树缺 ID、缺子节点、根缺失或存在环 |
| `E_PLACEHOLDER_OUT_OF_RANGE` | 谓词参数编号未声明或越界 |
| `E_SCALE_OUT_OF_RANGE` | permille/permyriad/null 刻度越界 |
| `E_CHECKPOINT_TRUNCATED` | 检查点 JSON/状态被截断或 checksum 不符 |
| `E_CHECKPOINT_VERSION_MISMATCH` | 检查点版本不匹配 |
| `E_BACKPRESSURE_WORKER_BUDGET` | worker 预算耗尽 |
| `E_BACKPRESSURE_BUFFER_LIMIT` | 单计划算子缓冲上限耗尽 |
| `E_INVALID_INCREMENTAL_EDIT` | 增量编辑目标或字段不合法 |

## 代码地图

- `model.go`：规范领域模型和单位/刻度常量。
- `generate.go`、`truth.go`、`truth_build.go`：固定种子样本和构造真值。
- `schema.go`、`schema_v1.go`、`schema_v2.go`、`schema_v3.go`：schema 探测、严格装载和编码。
- `normalize.go`：去重、下推、拓扑扁平化和列裁剪。
- `selectivity.go`：整数定点选择率和参数绑定解析。
- `cost.go`：路径、代价、连接枚举和 tie-break。
- `sensitivity.go`：绑定敏感性与全局/相对编号等价校验。
- `incremental.go`：依赖图、传播闭包、缓存和增量编辑。
- `parallel.go`：有界调度器和背压。
- `checkpoint.go`：原子文件检查点与恢复。
- `inject.go`：异常注入助手。
- `acceptance_test.go`：验收测试。
- `cmd/plandemo/main.go`：本地一键演示和 JSON 报告。

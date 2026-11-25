# 冷启动与并发控制可验收内核 — 技术说明

## 1. 目标与边界

本内核在接入真实 Serverless 平台之前，独立验证“请求到达 → 并发闸门 → 排队/拒绝 →
预热复用/冷启动 → 增量重算 → 检查点恢复”这条链路。全部输入由固定种子的确定性
生成器产出，不访问网络、云平台或容器运行时；SQLite 使用纯 Go 驱动
`modernc.org/sqlite`，`CGO_ENABLED=0` 即可构建。

## 2. 包结构

| 包 | 职责 |
| --- | --- |
| `internal/errcode` | 稳定 `E_xxx` 业务码与可定位上下文 |
| `internal/fixedpoint` | 声明位宽整数运算、溢出/饱和、单位与预算刻度换算、容差核验 |
| `internal/model` | 版本无关的统一函数/请求/逐时刻结果模型 |
| `internal/gen` | splitmix64 确定性 PRNG、工程规格、三 schema 快照构造 |
| `internal/loader` | 三版本装载、字段/语义/单位映射、声明校验 |
| `internal/fncore` | 单函数确定性状态机（无 goroutine，真值与引擎共用） |
| `internal/oracle` | 单线程参考驱动，唯一真值来源 |
| `internal/engine` | 有界 worker + 有界缓冲的并行驱动与背压码 |
| `internal/incremental` | 编辑描述、失效传播闭包、缓存复用与续算 |
| `internal/checkpoint` | SQLite 两阶段提交、版本校验、截断检测与恢复 |
| `internal/runner` | 信号处理、逐 tick 落盘、提交点恢复 |
| `internal/sample` / `testutil` | 三版本样本构造与跨版本/跨引擎一致性比较 |

## 3. 三版本映射口径

- v1：`tick` 时刻、绝对并发上限、扁平预热池表、可选 ‰ 预算。
- v2：毫秒时刻、`remaining_available + baseline_occupied` 剩余并发语义、
  按函数分组的嵌套预热池、‱ 预算；装载时反算绝对上限。
- v3：千分之一秒（毫秒量级整数）时刻、绝对上限、嵌套预热池、‱ 预算。

三个版本统一映射为整数 tick 与绝对槽位；预热池成员由
`gen.WarmInstanceID(funcID, idx)` 确定性命名，三版成员集合完全一致。
无法识别的版本/单位/字段组合返回 `E_SCHEMA_VERSION`、`E_UNIT_UNKNOWN`、
`E_SCHEMA_FIELD` 等，不做静默降级。

## 4. 闸门与排队规则

每个函数维护 `occupied / reservedUsed / sharedUsed`：

- 放行条件：`occupied+slots <= limit`，且 RESERVED 请求满足
  `reservedUsed+slots <= reserved`，SHARED 请求满足
  `sharedUsed+slots <= limit-reserved`。预留槽不会被共享请求挤占。
- 不能放行时记录被违反约束（`C1_RESERVED_EXHAUSTED`、`C2_SHARED_EXHAUSTED`、
  `C3_TOTAL_EXHAUSTED`）；队列未满则入队并记录 1 起始位次，队列满返回
  `E_QUEUE_FULL`。
- 队列 FIFO 晋升，遇第一个放不下的请求即停止；超过 `WaitLimitTicks` 返回
  `E_WAIT_TIMEOUT`。

## 5. 冷启动与预热复用

- 放行时优先弹出预热空闲实例，复用不消耗冷启动预算，请求结束后实例回收进池。
- 无空闲实例则按每函数独立 RNG 流在 `[ColdMin, ColdMax]` tick 抽冷启动耗时；
  槽位在放行即占用，创建完成前计入在途而不计入可用实例。
- 耗时超过 `ColdTimeoutTicks` 返回 `E_COLD_TIMEOUT` 并立即释放槽位、回滚计数。
- 冷启动消耗预算，预算耗尽返回 `E_BUDGET_EXHAUSTED`；预算绝对值由
  `ratio(‱) * 请求数 / 10000` 的定点乘除得到。

## 6. 定点换算与误差界

所有换算走 `internal/fixedpoint` 的带符号检查运算：

- 加/减/乘按真实符号判溢出，溢出时饱和到声明位宽边界并标记 `E_OVERFLOW`。
- 除零与 `MIN/-1` 显式作为溢出处理，不 panic。
- 毫秒↔tick 的整除余数进入 `Accumulator`，与声明 `Tolerance` 比较；
  ‰→‱ 为精确 10 倍刻度换算。
- `fixedpoint.CriticalCases` 输出临界换算的 got/want/误差/容差标注，
  累加误差不参与任何放行裁决，因此不会改变结论。

## 7. 增量失效闭包

缓存键为 `function/tick`。`incremental.Closure` 显式计算受影响集合：

- 并发上限、预留、请求增删：从声明的 `FromTick` 传播到 horizon。
- 预热池规模、冷启动预算属于 tick 0 即建立的函数级初始状态，闭包从 tick 0 起。

受影响函数在最早失效 tick 恢复上一 tick 末保存的 `fncore.State`
（含预热实例、在途冷启动、队列），随后只续算闭包内单元格；其余单元格直接复用。
增量结果与一次性全量 `oracle.Simulate` 逐请求、逐函数状态比较一致，
并统计 `Recomputed/Reused` 证明未受影响区间未重算。

## 8. 并行与背压

`engine.Run` 为每 tick 建立容量 `FuncBuffer` 的有界工作通道和 `Workers` 个
持久 worker；每函数是一个独立工作项，拥有独立 core 与 RNG 流。缓冲满时
非阻塞发送失败，返回稳定 `E_BACKPRESSURE`，不越界分配、不静默丢请求。
每函数记录 wall-time 诊断（`Timing`），仅用于观测，不参与正确性。
测试以 workers ∈ {1,2,4,16}、不同批次与缓冲组合验证结果完全一致。

## 9. 检查点与恢复

单事务内严格按序写入：

1. 先写全部 `function_state`（含占用、预热、在途、队列、冷启/复用/预算计数）
   与 `tick_outcomes`；
2. 再更新唯一的 `committed_tick` 全局序号。

因此任何强制终止后重启都不会读到未完成 tick。`SIGINT/SIGTERM` 作为可处理信号
优雅停在完整提交点；`SIGKILL` 不可捕获，仅依赖已持久化状态恢复且不承诺退出报告。
恢复校验检查点 schema 与工程来源，不匹配返回 `E_CHECKPOINT_VERSION` 整段重算，
截断/损坏返回 `E_CHECKPOINT_TRUNCATED`。

## 10. 验证入口

- `go run ./cmd/gensample`：在 `testdata/samples` 产出三版本 JSON 与规格。
- `go run ./cmd/demo`：三版装载、逐 tick 闸门/冷启动、定点容差、并行确定性、
  增量重算、检查点恢复对照。
- `go run ./cmd/demo -inject`：异常注入链路，输出稳定 `E_xxx`。
- `go test ./...`：单元与验收测试；`go test -race ./...`：竞态检测。

# 实际验证结果

环境：WSL2 Ubuntu、`go1.22.12 linux/amd64`、纯 Go SQLite 驱动
（`modernc.org/sqlite`），`CGO_ENABLED=0` 构建通过，未使用 Docker。

## 命令与结果

| 验证项 | 命令 | 结果 |
| --- | --- | --- |
| 纯 Go 构建 | `CGO_ENABLED=0 go build ./...` | 通过 |
| 静态检查 | `go vet ./...` | 无告警 |
| 全量测试 | `go test ./... -count=1` | `ok`（28 个顶层用例） |
| 竞态检测 | `go test -race ./... -count=1` | `ok`，无数据竞争 |
| 样本生成 | `go run ./cmd/gensample` | 写出 4 个文件、4 函数 72 请求 60 tick |
| 本地演示 | `go run ./cmd/demo` | 三版一致、增量与检查点恢复全部通过 |
| 异常注入 | `go run ./cmd/demo -inject` | 8 类异常均返回稳定 `E_xxx` |

## 验收点对照

1. 三版本装载等价：`TestThreeSchemasLoadIdentical`、
   `TestOracleIdenticalAcrossSchemas` 逐字段、逐请求、逐 tick 一致。
2. 闸门/排队/拒绝：`TestLastSlotBoundaryConsistentAcrossSchemas`
   覆盖最后槽位边界与约束编号、排队位次；`TestReservedNotSqueezedByShared`
   验证预留不被共享挤占。
3. 冷启动/预热：`TestColdTimeoutRejection`、`TestBudgetExhaustedRejection`
   与逐 tick 的 cold/reuse/inflight/available 状态比较覆盖该链路。
4. 定点量纲：`TestCheckedMulOverflow`、`TestDivByZeroAndRemainder`、
   `TestBudgetScaleConversion`、`TestCriticalCasesStayWithinTolerance`
   覆盖溢出、饱和、刻度换算与容差；演示打印临界 got/want/误差。
5. 增量失效：`TestIncrementalLimitEditMatchesFullRecompute`
   断言 `Recomputed=48 / Reused=192`，并与全量重算逐请求一致；
   `TestIncrementalClosureIsExplicit` 校验闭包边界；预热池/预算/请求增删
   另有专项用例。
6. 并行与背压：`TestEngineMatchesOracleUnderAllBudgets` 以 workers
   1/2/4/16、不同缓冲与批次验证结果不变；`TestBackpressureCodesAreStable`
   与 `TestQueueFullRejection` 验证背压/缓冲码；每函数耗时仅作诊断。
7. 检查点：`TestCheckpointRecoveryMatchesFullRun`、
   `TestPartialRunThenResume` 验证先落函数状态再更新全局序号及恢复；
   `TestVersionMismatchForcesRecompute`、`TestTruncatedCheckpointDetected`
   验证版本不匹配整段重算与截断检测。
8. 异常注入：`TestInjectedDeclarationErrors`、
   `TestSourceProjectNotMutatedByRejection`、
   `TestCommittedStateSurvivesBadInput` 验证稳定业务码、源数据不被修改、
   已提交状态保持可用。

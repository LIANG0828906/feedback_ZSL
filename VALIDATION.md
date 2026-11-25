# 实际验证结果

验证时间：2026-11-26 00:50 (+08:00)
环境：`go version go1.22.12 linux/amd64`
完整命令输出：`artifacts/verification.log`

## 构建与静态检查

```text
go build ./...                         # BUILD_OK
CGO_ENABLED=0 go build ./...           # CGO0_BUILD_OK（SQLite Open 返回需要 cgo 的明确错误）
go vet ./...                           # VET_OK
go test ./...                          # ok colortrace
go test -race .                        # ok
go test -run 'TestRealtime|TestBudget|TestChunkException|TestSQLite' -count=5 .
                                       # ok，重复调度/SQLite 测试稳定
go test -coverpkg=./... ./...           # 聚合语句覆盖率 73.8%
```

最终 verbose 运行包含 16 个根包测试，全部 PASS、0 FAIL。

模块没有第三方 Go 依赖，`go.mod` 不含 `require`；真实 SQLite 由系统
`libsqlite3.so.0` 通过 cgo 提供。

## 本地诊断样本

运行：

```bash
go run ./cmd/diagnostic -out artifacts/diagnostic
```

实际结果：

```text
ok=true seed=20261126
truth=0b149d3c6765e1ddd65b1a783520c0157319b088ec71b7914bd1cf7fcf33019f
first band: stage=1 plane=Y coord=(4,1) chunk=c01x00
jump=8192/5120 operator=bit_round
truth mismatch=<nil>
bounds=30
review_ok=true
commits realtime/recovery/full=8/8/8
recovery_skipped=3
budget waits=4 flushes=352 max_parallel=1
dag invalid=4 reusable=8
```

30 条边界检查来自 15 个真值采样点 × 2 个处理级。8 个提交对应第 0 级 6 块和
第 1 级 2 块。取消恢复演示在 3 个提交后取消，重开 SQLite 后准确复用这 3 块，
剩余 5 块重算，最终提交序列仍为规范的 8 项。

## 自动化测试覆盖

- `TestGeneratedTruthOutputsAttributionAndBounds`：生产分块输出与解析真值逐字段一致；分解和等于总偏差；边界通过；色带级/块/主导算子正确。
- `TestSchemaMissingFieldPath`：删除 `stages[1].bits` 后返回 `E_SCHEMA`，路径为 `$.stages[1].bits`。
- `TestParameterBitsOutOfRange`：位深越界返回 `E_PARA`，含级序号和合法范围。
- `TestAmbiguousAttributionListsShares`：均分贡献返回 `E_AMBIG` 和四类份额。
- `TestBoundViolationCode`：真值偏差越界返回带级/坐标的 `E_BOUND`。
- `TestIllegalStateTransitionPreservesState`：非法状态迁移返回 `E_STATE` 且原状态保持 `pending`。
- `TestDecodeTruncationCode`：截断贴图二进制返回 `E_DECODE` 和字段路径。
- `TestOrderInversionCode`：完成早于开始的时序倒置返回 `E_ORDER`。
- `TestDependencyGraphPreciseInvalidation`：色度参数只失效色度链；重排产生顺序失效清单。
- `TestRealtimeCancelResumeAndFullPathsMatch`：实时并行、取消恢复、全量重算的逐采样点输出、归因和提交序列一致。
- `TestBudgetQueueWaitAndSampleBuffer`：队列预算耗尽时记录 `pending_queue_budget`，并验证采样缓冲 flush 和最大并行度。
- `TestChunkExceptionIsolatesAndRetries`：注入 `E_DECODE` 后仅目标块失败，其他已提交结果不被污染，重试后与全量路径一致。
- `TestSQLiteRestartReuseReviewAndSnapshotInvalidation`：真实 SQLite 写入、重启复用、独立复核和版本篡改后整体作废。
- `TestSQLiteConcurrentCommitsAreSerial`：20 个并发提交产生连续有序的提交序号。
- `TestReviewReportsFirstTamperedField`：复核器定位首个被篡改的级/块/坐标/字段。
- `TestSQLiteHashMismatchInvalidatesSnapshot`：内容哈希不符时重开数据库清空旧快照。

## 异常码诊断演示

`cmd/diagnostic` 还会实际触发并确认：

```text
E_SCHEMA=true  E_STATE=true  E_PARA=true
E_DECODE=true  E_ORDER=true  E_AMBIG=true
```

`E_BOUND` 由 `TestBoundViolationCode` 覆盖；正常诊断样本的所有边界检查均通过。

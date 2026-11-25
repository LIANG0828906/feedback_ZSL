# 实际验证记录

验证时间：2026-11-25 11:08–11:10（Asia/Shanghai）。

验证环境：Linux/amd64，`go version go1.22.12 linux/amd64`，`GOFLAGS=-mod=mod`。依赖解析使用 `GOPROXY=off`，证明构建和测试不需要联网下载模块。SQLite 驱动为本地模块缓存中的纯 Go `modernc.org/sqlite v1.34.5`。

## 自动化测试

命令：

```bash
GOPROXY=off go test -v -timeout 60s ./...
GOPROXY=off go vet ./...
GOPROXY=off go test -race -timeout 180s .
```

实际结果：

- `go test ./...`：`ok macrokit`，17 个顶层测试全部通过；往返和三路径测试另含子用例。
- `go vet ./...`：通过，无输出。
- `go test -race .`：`ok macrokit 1.226s`，未发现数据竞争。

覆盖的验收点：

- `TestThreeFormatsMatchGeneratedTruth`：三份样本 × 三个发行格式均匹配期望触发帧、步骤序列、取消/压制状态，且三格式逐帧结果一致。
- `TestIntegerTimeRoundTrip`：60 FPS 下 0–1000 帧整数换算往返不变，并覆盖样本中的延时、窗口和步骤间隔。
- `TestHighLowHighFormatRoundTripPreservesFramesAndTriggers`：覆盖 `v3→v1→v3`、`v2(ms)→v1(frame)→v2(ms)`、`v3→v2→v3`，延时帧、窗口帧、间隔、触发帧和完整轨迹不变。
- `TestConditionEvaluationDeterministicAndOrderIndependent`：64 个并发只读求值结果和哈希一致，并验证按键窗口外变为 false。
- `TestPriorityAdjudicationAndConflictInjection`：验证 `explicit_priority:20>5` 裁决；严格模式 equal priority 返回带冲突集合的 `E_CONFLICT`。
- `TestDeterministicStableIDTieBreak`：默认策略在优先级和条件复杂度相同时用稳定 ID 裁决，依据写入 `Basis`。
- `TestDependencyGraphPreciseConditionInvalidation`：编辑 `d_a` 条件只失效 `d_a/d_b/d_c`，`d_ind` 复用缓存；增量与空缓存全量回放提交序列一致。
- `TestDependencyGraphPreciseDelayInvalidation`：编辑 `d_b` 延时只失效 `d_b/d_c`，`d_a/d_ind` 复用缓存。
- `TestPersistentBranchCacheAcrossRestart`：SQLite 分支缓存关闭重开后可加载，失效项从事务表删除，未受影响项保留。
- `TestRealtimeReplayAndCheckpointResumeParity`：三个样本的实时路径、全量回放、写首个窗口后关闭 SQLite、重开恢复路径通过 `CompareResults` 逐帧一致。
- `TestCheckpointFailureRetryCorruptionAndVersionInvalidation`：注入窗口失败后存在 1 个 committed、1 个 failed 窗口；其他窗口可读；重试后真值通过且 failed 清零；哈希篡改为 `E_CORRUPT`，版本篡改为 `E_VERSION`，随后快照行全部作废。
- `TestComparatorLocatesEDiff`：篡改一个动作后返回 `E_DIFF`，含帧 `4`、分支 `b_basic_q` 和字段 `frames[4].executed_steps[0].action`。
- `TestUnknownFormatReturnsSchemaPath`、`TestMissingRequiredFieldsReturnSchemaPath`、`TestFlatAndNestedReferenceBounds`：覆盖 `E_SCHEMA` 字段路径和 `E_REF` 引用标识。
- `TestConversionOverflowIdentifiesBranch`：最大 `int64` 延时返回带分支 ID 的 `E_OVF`。

## CLI 端到端验证

命令：

```bash
GOPROXY=off go run ./cmd/macrotool generate
GOPROXY=off go run ./cmd/macrotool verify
GOPROXY=off go run ./cmd/macrotool inject -case all
```

实际结果：

```text
generated seed=20261125 projects=3 root=examples/sample-projects
PASS basic-timing
PASS dependency-chain
PASS priority-conflict
VERIFY PASS projects=3
```

异常注入均返回预期错误码：

```text
INJECT PASS schema -> E_SCHEMA
INJECT PASS ref -> E_REF
INJECT PASS overflow -> E_OVF
INJECT PASS conflict -> E_CONFLICT
INJECT PASS diff -> E_DIFF
INJECT PASS fault-window -> E_FAULT
INJECT PASS corrupt-checkpoint -> E_CORRUPT
```

CLI 验证会在 `data/<project>.sqlite` 创建本地检查点数据库；`data/` 和 `*.sqlite*` 已加入 `.gitignore`。

## Windows 兼容性

已在 Linux 沙箱执行：

```bash
GOOS=windows GOARCH=amd64 GOPROXY=off go build ./...
```

结果通过，并额外生成过 9.5 MB 的 Windows amd64 CLI 可执行文件。SQLite 驱动为纯 Go，不需要 CGO/GCC。当前沙箱没有 `cmd.exe`/`powershell.exe` 互操作入口，因此未直接执行 Windows 终端；在 Windows 终端中可运行：

```powershell
go env -w GOFLAGS=-mod=mod
.\scripts\verify.ps1
```

或手动执行 `go test ./...`、`go run .\cmd\macrotool verify`。

补充：时间策略驼峰映射补丁后，已再次执行 `go test ./...`、`go vet ./...`、`go test -race .`、Windows 测试二进制交叉编译和 `go run ./cmd/macrotool verify`，结果均通过。

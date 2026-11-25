# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）

环境：

- Go：`go version go1.22.12 linux/amd64`
- SQLite：`modernc.org/sqlite v1.29.10`（纯 Go，已写入 `go.mod` 并 `vendor/`）
- 工作区：`/home/liang/feedback/uid42a8691e-default-359`

## 1. 全量测试

命令：

```bash
go test -count=1 ./...
```

实际结果：

```text
ok  replaybench                    0.242s
ok  replaybench/internal/checkpoint 0.003s
?   replaybench/cmd/replaydemo     [no test files]
?   replaybench/internal/diagnostics [no test files]
?   replaybench/internal/diff      [no test files]
?   replaybench/internal/eerr      [no test files]
?   replaybench/internal/engine    [no test files]
?   replaybench/internal/fixed     [no test files]
?   replaybench/internal/generator [no test files]
?   replaybench/internal/graph     [no test files]
?   replaybench/internal/loader    [no test files]
?   replaybench/internal/model     [no test files]
?   replaybench/internal/runtime   [no test files]
?   replaybench/internal/scheduler [no test files]
?   replaybench/internal/timeconv  [no test files]
```

覆盖的验收点：

- `TestThreeFormatsLoadAndGeneratedTruth`：三格式加载、源格式标识、生成器真值、三格式快照一致和胜负判定。
- `TestSchemaErrorsPointAtFieldPath`：未知格式号与必填字段缺失返回 `E_SCHEMA`，路径如 `$.format`、`$.replay_id`、`$.units[0].primary_int`。
- `TestConditionalBitFlagConversion`：`flags.execute_flag=0` 换算为假，`a_execute` 被记录为 `condition_flag/skipped` 且不生效。
- `TestTimeConversionOverflowAndRoundTrip`：整型往返；溢出为 `E_OVF`，含字段和两个操作数。
- `TestHighLowHighFormatRoundTripPreservesSemantics`：格式 3 → 格式 1 → 格式 3 后指令、单位权重/冷却和快照完全一致。
- `TestPrecedenceCycleReturnsEConflict`：无法裁决的优先级环返回 `E_CONFLICT` 和争用技能集合。
- `TestWeightEditInvalidatesPreciseIntervalAndMatchesFull`：权重编辑仅失效回合 `[2,3]`，第 1 回合持久化命中；增量与全量逐项相等。
- `TestReorderEditGraphPropagation`：插入重排指令的触发节点为 `edit:reorder@2`，失效 `[2,3]`。
- `TestDiffLocatesFirstMismatch`：差异返回 `E_DIFF`，定位回合 2 与字段 `states.alpha.hp`。
- `TestCheckpointResumeVersionInvalidationAndThreePaths`：实时/检查点/恢复路径一致；版本不匹配清空窗口。
- `TestRecoverInvalidatesBadWindowAndSuffix`：第 2 窗口版本失配后仅保留第 1 窗口，第 2、3 窗口作废。
- `TestCancelBeforeCommitLeavesNoPartialWindow`：取消后无已提交窗口，重新运行与全量一致。
- `TestBoundedPoolConcurrencyBudgetAndWaits`：32 个并发任务下最大并行度不超过配置 4，记录 `queue_budget_exhausted` 等待。
- `TestPoolCancelWhileQueued`：排队中的取消立即返回。
- `TestForceKillCheckpointRecoveryBoundary`：测试子进程在窗口 1 提交后收到真实 `SIGKILL`；父进程确认信号、恢复窗口 1、续算提交序列 `[1,2,3]` 且等于全量。
- `TestDiagnosticsEndpoints`：`/healthz`、`/` 和所有 JSON API 均 200，增量诊断显示 `full_diff_matched=true`；`/api/full?format=99` 返回 422 与 `E_SCHEMA`。
- `TestDeterministicSnapshotAcrossRepeatedRuns`：同一输入重复 20 次结果深度相等。

## 2. Race 检测

命令：

```bash
go test -race -count=1 . ./internal/checkpoint ./internal/scheduler
```

实际结果：

```text
ok  replaybench                     1.530s
ok  replaybench/internal/checkpoint 1.026s
?   replaybench/internal/scheduler  [no test files]
```

## 3. CLI 一键验收

命令：

```bash
go run ./cmd/replaydemo -samples-dir artifacts/samples
```

完整 JSON 报告写入 `artifacts/verification-report.json`。关键结果：

```json
{
  "ok": true,
  "seed": 20261126,
  "invalid_rounds": [2, 3],
  "reused_rounds": [1],
  "intervals": [{"from": 2, "to": 3}],
  "commit_sequence": [1, 2],
  "scheduler_max_concurrency": 3,
  "scheduler_wait_records": 8
}
```

报告内所有 check 均为 `"ok": true`，包括三格式真值、格式往返、增量全量比对、检查点提交/恢复和有界调度器。

样本文件：

- `artifacts/samples/format-1.json`
- `artifacts/samples/format-2.json`
- `artifacts/samples/format-3.json`
- `artifacts/samples/truth.json`

## 4. HTTP 诊断页

启动前用 `ss -ltn` 确认 `127.0.0.1:18080` 未被占用，然后：

```bash
go build -o artifacts/replaydemo ./cmd/replaydemo
./artifacts/replaydemo -serve -addr 127.0.0.1:18080 -db /tmp/rb-http.db
```

实际请求结果：

```text
GET /healthz                 -> {"status":"ok"}
GET /api/overview            -> 200，包含版本、种子、定点 scale 和端点
GET /api/full?format=2       -> 200
GET /api/incremental         -> "full_diff_matched":true
GET /api/checkpoint          -> "full_diff_matched":true
GET /                        -> 200，HTML 诊断台 1928 bytes
```

## 5. 真实 SIGKILL 与续算

强杀：

```bash
./artifacts/replaydemo -db /tmp/rb-crash-demo.db -crash-after-window=1
```

实际结果：

```text
committed window 1; sending SIGKILL to self
/bin/bash: line 1: ... Killed ...
exit_code=137
```

恢复：

```bash
./artifacts/replaydemo -db /tmp/rb-crash-demo.db -recover-only
```

实际结果：

```json
{
  "commit_sequence": [1, 2, 3],
  "ok": true,
  "recovered_window": 1
}
```

恢复路径内部已用 `diff.Snapshots` 与一次性全量结果比对；不一致会返回非零退出和 `E_DIFF`。

## 6. 无网络离线构建

依赖已 vendor。命令：

```bash
GOPROXY=off GOFLAGS=-mod=vendor CGO_ENABLED=0 go test -count=1 ./...
GOPROXY=off GOFLAGS=-mod=vendor CGO_ENABLED=0 go build -o /tmp/replaybench-offline ./cmd/replaydemo
```

实际结果：全部包测试通过，并输出：

```text
OFFLINE_BUILD_OK
```

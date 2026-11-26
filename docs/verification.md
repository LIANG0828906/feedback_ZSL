# 实际验证记录

验证时间：2026-11-27（Asia/Shanghai）。

环境：

```text
go version go1.22.12 linux/amd64
```

Go module 无第三方依赖，`go.mod` 声明 Go 1.21。

## 自动化测试

命令：

```bash
go vet ./...
go test ./...
go test -race ./...
go test ./internal/kernel -cover
```

实际结果：

```text
?       quotakernel/cmd/quota-diag      [no test files]
ok      quotakernel/internal/kernel
?       quotakernel/cmd/quota-diag      [no test files]
ok      quotakernel/internal/kernel     1.189s   # -race
ok      quotakernel/internal/kernel     0.032s   coverage: 75.9% of statements
```

详细测试全部通过：

```text
--- PASS: TestThreeSchemaVersionsMapToSameCanonicalProject
--- PASS: TestLedgerHardCapAndNodeInvariants
--- PASS: TestFastExactAndParallelPathsMatch
--- PASS: TestFairOrderIsStableUnderEnumerationAndParallelism
--- PASS: TestGuaranteeTranchesAlwaysPrecedeBurstLayer
--- PASS: TestIncrementalEditsMatchFullRecompute
--- PASS: TestSourceProjectIsNotMutatedByIncrementalEdit
--- PASS: TestCheckpointRoundTripTruncationAndVersionMismatch
--- PASS: TestDiagnosticSuiteAllExpectedCodes
--- PASS: TestFullVerificationPasses
--- PASS: TestEveryCommittedTickCanResume
--- PASS: TestSQLiteCheckpointRoundTripSmoke
--- PASS: TestLongHorizonAdmissionRemainsStable
--- PASS: TestGateSinglePreDeductionNeverCrossesHardCap
--- PASS: TestVersionMismatchRecoveryRecomputesFromStart
--- PASS: TestSignalClassification
--- PASS: TestSmokeGenerateAndLoad
```

## 固定种子端到端验收

命令：

```bash
go run ./cmd/quota-diag all --seed 42 --out samples --artifacts artifacts
```

实际输出：

```text
schema_equivalent=true
path_equivalence exact/pre/parallel=true/true/true allocations=22
incremental weight_event       equal=true recomputed=[5 6 7 8] reused=5 closure=10
incremental tenant_quota       equal=true recomputed=[0 1 2 3 4 5 6 7 8] reused=0 closure=20
incremental tenant_guarantee   equal=true recomputed=[0 1 2 3 4 5 6 7 8] reused=0 closure=20
incremental node_capacity      equal=true recomputed=[0 1 2 3 4 5 6 7 8] reused=0 closure=20
checkpoint tick=3 roundtrip=true suffix_resume=true path=artifacts/checkpoint.sqlite
diagnostics=17/17 passing
ALL_OK=true
```

机器可读结果：

- `artifacts/verification-report.json`
- `artifacts/diagnostic-report.json`
- `samples/truth.json`
- `samples/critical-cases.json`

## 异常注入业务码

`go run ./cmd/quota-diag diagnose --seed 42` 实际结果：

```text
PASS three_schema_equivalence               expected=E_OK                             got=E_OK
PASS fast_exact_parallel_equivalence        expected=E_OK                             got=E_OK
PASS unknown_field                          expected=E_UNKNOWN_FIELD                  got=E_UNKNOWN_FIELD
PASS negative_quota                         expected=E_NEGATIVE_QUOTA                 got=E_NEGATIVE_QUOTA
PASS guarantee_exceeds_quota                expected=E_GUARANTEE_EXCEEDS_QUOTA        got=E_GUARANTEE_EXCEEDS_QUOTA
PASS burst_percent_out_of_range             expected=E_BURST_PERCENT_OUT_OF_RANGE     got=E_BURST_PERCENT_OUT_OF_RANGE
PASS event_tick_out_of_order                expected=E_EVENT_TICK_OUT_OF_ORDER        got=E_EVENT_TICK_OUT_OF_ORDER
PASS unsupported_project_schema             expected=E_SCHEMA_VERSION_UNSUPPORTED     got=E_SCHEMA_VERSION_UNSUPPORTED
PASS burst_expression_version_conflict      expected=E_BURST_EXPRESSION_CONFLICT      got=E_BURST_EXPRESSION_CONFLICT
PASS worker_budget_out_of_range             expected=E_WORKER_BUDGET_INVALID          got=E_WORKER_BUDGET_INVALID
PASS queue_budget_out_of_range              expected=E_QUEUE_BUDGET_INVALID           got=E_QUEUE_BUDGET_INVALID
PASS queue_backpressure_runtime             expected=E_BACKPRESSURE_QUEUE_EXHAUSTED   got=E_BACKPRESSURE_QUEUE_EXHAUSTED
PASS worker_backpressure_runtime            expected=E_BACKPRESSURE_WORKER_EXHAUSTED  got=E_BACKPRESSURE_WORKER_EXHAUSTED
PASS causal_reference_cycle                 expected=E_CAUSAL_REFERENCE_CYCLE         got=E_CAUSAL_REFERENCE_CYCLE
PASS fixed_point_tolerance_and_overflow     expected=E_TOLERANCE_EXCEEDED             got=E_TOLERANCE_EXCEEDED
PASS truncated_checkpoint                   expected=E_CHECKPOINT_TRUNCATED           got=E_CHECKPOINT_TRUNCATED
PASS checkpoint_version_mismatch            expected=E_CHECKPOINT_VERSION_MISMATCH    got=E_CHECKPOINT_VERSION_MISMATCH
```

## SQLite 文件交叉验证

Go 标准库不含 SQLite 驱动；本仓库的最小原生写入器生成的
`artifacts/checkpoint.sqlite` 已用环境中的 Python SQLite 打开：

```text
integrity_check= ok
tables= ['meta', 'tenant_state', 'node_state', 'hold', 'queue_item']
tenant_columns= ['tick', 'tenant_id', 'quota_bytes', 'guarantee_bytes',
  'burst_limit_bytes', 'used_bytes', 'held_bytes', 'available_quota_bytes',
  'guarantee_remaining_bytes', 'burst_remaining_bytes', 'hard_headroom_bytes',
  'priority', 'weight_bp']
```

## 复现说明

```bash
command -v go
go version
go test ./...
go run ./cmd/quota-diag all --seed 42
```

不需要 Docker、外部数据库、网络服务或账号。`artifacts/checkpoint.sqlite` 是实际
SQLite 文件；`artifacts/checkpoint.sqlite.sql` 是提交顺序审计脚本/JSON 注释，不依赖
它执行恢复。

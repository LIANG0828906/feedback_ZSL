# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）。

## 环境

```text
go version go1.22.12 linux/amd64
sqlite3 3.50.2 2025-06-28 ... (64-bit)
gcc-15 (Ubuntu 15.2.0-16ubuntu1) 15.2.0
github.com/mattn/go-sqlite3 v1.14.24
```

## 单元与验收测试

命令：

```bash
gofmt -l cmd internal tests
go vet ./...
GOPROXY=off go test -count=1 ./...
```

结果：`gofmt` 无未格式化文件；`go vet` 退出码 0（仅有 mattn/sqlite3 C 编译期 const 限定符 warning，不影响构建）；离线测试退出码 0。

```text
?       mvccworkbench/cmd/mvccwb        [no test files]
?       mvccworkbench/internal/diagnostics [no test files]
?       mvccworkbench/internal/domain    [no test files]
?       mvccworkbench/internal/generator [no test files]
ok      mvccworkbench/internal/checkpoint
ok      mvccworkbench/internal/engine
ok      mvccworkbench/internal/incremental
ok      mvccworkbench/internal/schema
ok      mvccworkbench/internal/workbench
ok      mvccworkbench/tests
```

详细模式统计：

```bash
go test -count=1 -v ./...
```

实际结果：`45` 条 `--- PASS` 记录（含子测试），`0` 条 `--- FAIL`。

关键测试：

- `TestCanonicalProjectsMatchGeneratorTruth`：三批工程的 reads/writes/commits/GC 与生成器真值逐项 `DeepEqual`。
- `TestThreeSchemaEditionsRoundTripToSameCanonicalProject`：三批 × v1/v2/v3 装载后 canonical 工程完全一致。
- `TestAcceptanceThreeEditionsAndTruth`：三批 × canonical/v1/v2/v3 × sequential/parallel 全部真值匹配。
- `TestParallelResultsMatchTruthAcrossWorkerBudgets`：worker `1/2/4/8` 下结果一致，并记录每数据项耗时。
- `TestCheckpointResumeOnlyFromMatchingVersion`：seq=7 中断后仅续算 8..21，后缀 read/GC 与真值一致。
- `TestCheckpointVersionMismatchForcesFullReplay`、`TestRecoverUsesFullReplayForVersionMismatch`。
- `TestInjectedCheckpointExceptionsAreStable`、`TestInjectedBadSnapshotCanBeQuarantined`。
- `TestTruncatedCheckpointCanFullReplayAndRemainAvailable`。
- `TestCancelStopsAtLastCompleteCheckpoint`。
- `TestIsolationChangeRecomputesDependencyClosureOnly`、`TestStartChangeAndAppendDeleteMatchFullRecompute`。
- `TestAcceptanceInjectedInputExceptions`。

Race 验证：

```bash
go test -race -count=1 ./internal/engine ./internal/incremental ./internal/checkpoint ./tests
```

实际结果：全部 `ok`，无 data race 报告。

## CLI 全量真值验收

构建：

```bash
go build -o /tmp/mvccwb ./cmd/mvccwb
```

顺序：

```bash
/tmp/mvccwb verify-all
```

```json
{
  "checked": 12,
  "failed": 0,
  "parallel": false
}
```

并行：

```bash
/tmp/mvccwb verify-all --parallel
```

```json
{
  "checked": 12,
  "failed": 0,
  "parallel": true
}
```

这里的 12 个目标 = 3 批工程 × 4 种装载入口（canonical + v1 + v2 + v3）。

## 输入异常码

命令：

```bash
/tmp/mvccwb inject --batch B1-visibility --kind duplicate_item
/tmp/mvccwb inject --batch B1-visibility --kind bad_delete_ref
/tmp/mvccwb inject --batch B1-visibility --kind time_out_of_range
/tmp/mvccwb inject --batch B1-visibility --kind unknown_schema
```

实际稳定码与路径：

```text
E_INPUT_DUPLICATE_ITEM     path=items[2].id
E_INPUT_BAD_DELETE_REF     path=versions[0].delete_txn, context.delete_txn=999999
E_INPUT_TIME_OUT_OF_RANGE  path=transactions[0].start_ms, context.value=10000100
E_SCHEMA_UNKNOWN           path=schema_version, context.schema_version=mvcc.project/v9
```

## 检查点异常注入

```bash
/tmp/mvccwb checkpoint-inject --db /tmp/truncated.db --kind truncated_checkpoint --item alpha
/tmp/mvccwb checkpoint-inject --db /tmp/bad-snapshot.db --kind version_mismatched_snapshot
```

实际返回：

```text
E_CHECKPOINT_TRUNCATED       at checkpoint_chains.chain_json
E_SNAPSHOT_VERSION_MISMATCH  at checkpoint_snapshots.snapshot_json
```

坏快照可通过 `Store.QuarantineIncompatibleSnapshots` 隔离；测试确认隔离后最后完整 meta/chains/seq 仍可读取。

## 中断恢复与 SQLite 落盘

命令：

```bash
/tmp/mvccwb checkpoint-demo --batch B1-visibility \
  --db /tmp/mvcc-accept.db --crash-at 7
sqlite3 /tmp/mvcc-accept.db \
  "SELECT format_version, project_version, batch_id, committed_op_seq FROM checkpoint_meta;"
```

实际结果摘要：

```json
{
  "crash_at_op": 7,
  "checkpoint_seq_at_crash": 7,
  "recovery_mode": "VERSION_MATCHED_RESUME",
  "final_seq": 21,
  "processed_after_resume": [8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21]
}
```

```text
mvcc-checkpoint/v1|mvcc-project-canonical/v1|B1-visibility|21
```

SQLite 表：

```text
checkpoint_chains  checkpoint_meta  checkpoint_snapshots
```

## Web 工作台

```bash
/tmp/mvccwb serve --addr 127.0.0.1:18765
curl 'http://127.0.0.1:18765/api/batches'
curl 'http://127.0.0.1:18765/api/verify?batch=B2-conflict-gc&schema=mvcc.project/v2&parallel=1&workers=4'
```

实际 `/api/batches` 返回三批 ID；v2 并行验证返回：

```text
{'batch_id': 'B2-conflict-gc', 'schema': 'mvcc.project/v2', 'execution': 'parallel',
 'ok': True, 'read_count': 4, 'write_count': 4, 'commit_count': 3, 'gc_count': 3}
```

## 结论

验收要求中的三版工程真值一致、异常稳定码、并行确定性、SQLite 版本匹配恢复、信号/取消边界落盘和增量重算一致性均有自动化测试或上述实际命令记录支撑。

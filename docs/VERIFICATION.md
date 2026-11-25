# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）。

完整原始日志保存在：

- `artifacts/validation/go-test.log`
- `artifacts/validation/go-vet.log`
- `artifacts/validation/cgo0-build.log`
- `artifacts/validation/race-test.log`
- `artifacts/validation/verify.json`
- `artifacts/validation/demo.log`

## 环境

```text
$ go version
go version go1.22.12 linux/amd64
```

SQLite 驱动为纯 Go 的 `modernc.org/sqlite v1.29.10`；`CGO_ENABLED=0 go build ./...` 退出码为 0。

## 自动化测试

```text
$ go test -count=1 ./...
ok  samplingkernel/internal/acceptance  0.058s
ok  samplingkernel/internal/budget      0.001s
test_exit=0

$ go vet ./...
vet_exit=0

$ go test -race -count=1 ./internal/acceptance ./internal/budget
ok  samplingkernel/internal/acceptance  2.003s
ok  samplingkernel/internal/budget      1.007s
race_exit=0
```

验收测试覆盖：

- 三版 schema 装载后的 canonical 工程 `cmp.Diff` 为空；
- 三版观测报告与生成器真值一致；
- 未知字段返回 `E_SCHEMA_UNKNOWN_FIELD` 和完整字段路径；
- 同一请求在 200 个 goroutine、1000 次并发决策中完全一致；
- 0/10000 比率不翻转；
- 多尾条件优先级、强制保留和服务额度隔离；
- 全局/服务预算拒绝码、跨秒窗口和强制越界；
- 重复跨度、乱序因果序、隔离孤儿和挂接孤儿；
- 五类增量编辑的核心报告与逐事件视图均等于全量重算；
- worker/buffer 背压码和参数越界码；
- seq=20 崩溃注入后仅有两条 complete trace 可读；
- 匹配版本检查点从 seq=20 恢复并复现真值；
- 版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH` 并整段重算；
- 截断检查点返回 `E_CHECKPOINT_TRUNCATED`；
- 九类异常注入码和源工程未被修改。

## 内置验收命令

```text
$ go run . verify
... 12 项 check 全部 `"ok": true`
verify_exit=0
```

12 项检查包括：

- 1 项生成器真值与内核一致性；
- v1/v2/v3 各 1 项工程等价、1 项观测等价，共 6 项；
- 5 项增量编辑（0%、100%、42%、分摊下调、尾条件阈值修改）与全量重算一致。

增量检查同时比较 `Events` 视图，输出中包含 `event_views_equal=true`。

## 演示入口

```text
$ go run . demo
...
recovery action=resume_matching_version loaded_seq=20 checkpoint_completed_traces=2 final_events=58 truth_match=true
version probe expected=sampling.project/v9 code=E_CHECKPOINT_VERSION_MISMATCH
demo_exit=0
```

演示生成：

- `artifacts/demo/project-v1.json`
- `artifacts/demo/project-v2.json`
- `artifacts/demo/project-v3.json`
- `artifacts/demo/truth-report.json`
- `artifacts/demo/checkpoint.db`
- `artifacts/demo-run.log` / `artifacts/validation/demo.log`

CLI 装载三版样本均得到相同对象规模：

```text
loaded sampling.project/v1: services=5 edges=6 events=58 allocations=5
loaded sampling.project/v2: services=5 edges=6 events=58 allocations=5
loaded sampling.project/v3: services=5 edges=6 events=58 allocations=5
```

## 异常码核验

```text
E_RATE_OUT_OF_RANGE              rate_out_of_range
E_TAIL_CONDITION_CYCLE           tail_condition_cycle
E_ALLOC_SUM_EXCEEDED             allocation_sum_exceeded
E_EDGE_SERVICE_MISSING           edge_service_missing
E_RUNTIME_WORKER_BUDGET_INVALID  worker_budget_out_of_bounds
E_RUNTIME_BUFFER_BUDGET_INVALID  buffer_budget_out_of_bounds
E_SCHEMA_UNKNOWN_FIELD           unknown_schema_field
E_CHECKPOINT_TRUNCATED           truncated_checkpoint
E_CHECKPOINT_VERSION_MISMATCH    version_mismatch_snapshot
```

## 结论

首个可验收版本已在本地独立跑通；三版装载、确定性采样、尾采/强制保留、预算、归并、增量重算、并行背压、SQLite 检查点恢复和异常注入均有自动化测试或可重复 CLI 演示覆盖。

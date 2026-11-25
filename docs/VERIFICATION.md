# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）。完整命令输出已写入 `artifacts/verification-run.log`。

## 环境

```text
$ go version
go version go1.22.12 linux/amd64
```

SQLite 驱动：`modernc.org/sqlite v1.29.10`（纯 Go，无系统 SQLite 服务依赖）。

## 构建、静态检查与测试

```text
$ go build ./...
# 退出码 0

$ go vet ./...
# 退出码 0

$ go test -race -count=1 ./...
ok  grayrelease  1.699s
?   grayrelease/cmd/grayrelease             [no test files]
?   grayrelease/internal/acceptance         [no test files]
?   grayrelease/internal/checkpoint         [no test files]
?   grayrelease/internal/durable            [no test files]
?   grayrelease/internal/engine             [no test files]
?   grayrelease/internal/fault              [no test files]
?   grayrelease/internal/incremental        [no test files]
?   grayrelease/internal/loader             [no test files]
?   grayrelease/internal/model              [no test files]
?   grayrelease/internal/sample             [no test files]
?   grayrelease/internal/scheduler          [no test files]
```

普通全量验证：

```bash
bash scripts/verify.sh
```

最终输出：

```text
VERIFICATION PASSED; log=artifacts/verification-run.log
```

## 确定性样本

```text
$ go run ./cmd/grayrelease gen --dir samples
{
  "ok": true,
  "sample_dir": "samples",
  "seed": 4208691
}
```

生成文件：

- `samples/project-v1.json`
- `samples/project-v2.json`
- `samples/project-v3.json`
- `samples/requests-v1.json`
- `samples/requests-v2.json`
- `samples/requests-v3.json`
- `samples/truth.json`

样本包含 4 个服务、8 条规则、16 个稳定键、6 个请求时刻，共 `384` 条请求；模拟额外推进影子排空时刻，因此有 `8` 个 tick 报告。

## 验收报告

```text
$ go run ./cmd/grayrelease demo --samples samples --out artifacts/demo-report.json
```

以下检查全部为 `ok: true`：

- `three-schema-load-equivalence`
- `three-schema-decision-equivalence`
- `decision-count`
- `main-never-blocked-by-shadow`
- `mirror-caps`
- `stable-assignment`
- `zero-full-edges`
- `migration-truth`
- `parallel-determinism`
- `route-buffer-backpressure`
- `worker-budget-code`
- `incremental-sample-equivalence`
- `incremental-weight-equivalence`
- `checkpoint-roundtrip`

摘要：

```text
decisions=384
tick_reports=8
buffer_backpressure=384
sample_edit_equivalent=true
weight_edit_equivalent=true
checkpoint_tick=7
ok=true
```

完整 JSON：`artifacts/demo-report.json`。

## 并行与背压

```text
$ go run ./cmd/grayrelease parallel-check --samples samples
{
  "decisions": 384,
  "ok": true,
  "peak_workers_1": 1,
  "peak_workers_8": 5
}

$ go run ./cmd/grayrelease backpressure-demo --samples samples --route-buffer 2 --workers 4
processed=48
backpressure_count=336
codes.E_ROUTE_BUFFER_FULL=336
```

`peak_workers_8` 是极短任务下实际采样到的峰值；worker 池预算为 8。`go test -race` 下 worker 1 与 worker 8 的有序决策完全一致。

## 增量失效

CLI 示例对 `checkout/R-CHK-BETA` 从 tick 3 调整到 `6500‱`：

```text
closure.services=[checkout inventory ledger payment]
start_tick=3
end_tick=7
stable_keys=[key-003 key-007 key-010 key-014]
recomputed_ticks=20
reused_ticks=12
recomputed_requests=192
reused_requests=192
ok=true
```

`stable_keys` 与 `samples/truth.json` 的迁移真值一致。完整报告：`artifacts/incremental-report.json`。

## 异常注入

所有注入命令均以退出码 `1` 返回 JSON 错误，且只写 `artifacts/inject/` 下的注入副本，不改 `samples/` 源工程：

| 注入 | 实际业务码 |
| --- | --- |
| `overlap` | `E_RULE_OVERLAP` |
| `bad-scale` | `E_SCALE_RANGE` |
| `missing-service` | `E_SERVICE_MISSING` |
| `ambiguous-scale` | `E_FIELD_AMBIGUOUS` |
| `truncated-checkpoint` | `E_CKPT_TRUNCATED` |
| `version-mismatch` | `E_VERSION_MISMATCH` |
| `worker-budget` | `E_WORKER_BUDGET` |
| `worker-budget-exhausted` | `E_WORKER_BUDGET_EXHAUSTED` |

## 进程中断与重启

```text
$ bash scripts/recovery_check.sh
== crash at after-routes ==
  recovered after-routes -> committed_tick=7
== crash at after-queues ==
  recovered after-queues -> committed_tick=7
== crash at before-commit ==
  recovered before-commit -> committed_tick=7
== crash at after-commit ==
  recovered after-commit -> committed_tick=7
all recovery crash points passed
```

一次手动 `after-routes` 演练中，强杀后的数据库状态为：

```text
committed=0
orphan_routes_t1=64
queues_t1=0
```

重启恢复输出：

```text
committed_tick=7
started_at_tick=1
recovery.deleted_future_routes=64
recovery.deleted_future_queues=0
```

这证明先写入但未提交全局时刻的分流状态不会被当作已完成时刻读取。

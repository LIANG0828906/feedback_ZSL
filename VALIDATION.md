# 实际验证记录

验证时间：`2026-11-26 07:58:55 +0800`

环境：

```text
go version go1.22.12 linux/amd64
SQLite driver: modernc.org/sqlite v1.34.4 (pure Go)
```

## 自动测试

执行：

```bash
gofmt -l .
go test -count=1 -cover ./...
go vet ./...
go test -race -count=1 ./...
```

实际结果：

```text
gofmt -l .: 无输出（全部格式化）
ok  downsample  0.058s  coverage: 79.5% of statements
?   downsample/cmd/demo  [no test files]
go vet ./...: vet: OK
?   downsample/cmd/demo  [no test files]
ok  downsample  1.668s
```

测试覆盖：

- 三版 schema 的指标顺序、点顺序、时间戳还原和窗口边界等价。
- 左闭右开边界点归属、已知均值、加权均值和计数。
- 三版聚合结果、填充状态、权重总量和全量真值逐窗口一致。
- 千分比/万分比精确换算、舍入误差、16 位饱和溢出和反复换算固定点。
- 函数集合编辑、顺序配置编辑、追加采样的增量结果与全量重算一致。
- 跨指标派生闭包、跨指标边和闭包边界输出。
- SQLite 正常恢复、两阶段提交之间崩溃后的 orphan row 不可见。
- 配置编辑持久化后，新版本检查点仍能读到复用窗口。
- 版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH` 并整段重算。
- 非法编辑不改变已提交窗口。
- 全部 12 个异常注入场景返回预期稳定码。
- `RunDemo` 端到端生成报告且 `AllPassed=true`。
- 可取消流在中断后只恢复最后完整提交点。

## 本地演示

执行：

```bash
go run ./cmd/demo --out artifacts/demo --verify
```

实际结果摘要：

```text
windows=28 injections=12 all_passed=true
PASS schema v1
PASS schema v2
PASS schema v3
PASS generator truth comparison
PASS edit function_set_cpu incremental == full
PASS edit window_width_mem incremental == full
PASS edit alignment_cpu incremental == full
PASS edit weight_scale_cpu incremental == full
PASS checkpoint recovery and commit order
PASS injection window_width_zero -> E_WINDOW_WIDTH_ZERO
PASS injection relative_offset_out_of_range -> E_RELATIVE_OFFSET_OUT_OF_RANGE
PASS injection weight_scale_out_of_range -> E_WEIGHT_SCALE_OUT_OF_RANGE
PASS injection function_cycle -> E_FUNCTION_TABLE_CYCLE
PASS injection worker_budget_out_of_range -> E_BUDGET_WORKERS_OUT_OF_RANGE
PASS injection buffer_budget_out_of_range -> E_BUDGET_BUFFER_OUT_OF_RANGE
PASS injection worker_backpressure -> E_BACKPRESSURE_WORKER_BUDGET_EXHAUSTED
PASS injection buffer_backpressure -> E_BACKPRESSURE_WINDOW_BUFFER_EXHAUSTED
PASS injection checkpoint_truncated -> E_CHECKPOINT_TRUNCATED
PASS injection checkpoint_version_mismatch -> E_CHECKPOINT_VERSION_MISMATCH
PASS injection unknown_field -> E_SCHEMA_UNRECOGNIZED_FIELD
PASS injection stream_interrupt -> E_STREAM_INTERRUPTED
```

生成文件：

- `artifacts/demo/report.md`
- `artifacts/demo/demo-report.json`
- `artifacts/demo/project-v1.json`
- `artifacts/demo/project-v2.json`
- `artifacts/demo/project-v3.json`
- `artifacts/demo/windows.json`
- `artifacts/demo/weights.json`
- `artifacts/demo/demo-checkpoint.sqlite`
- `artifacts/demo/demo-crash-order.sqlite`

## 关键数值抽查

`cpu.usage` 的 W0 包含 3 个真实点；时间戳恰为 W0 右边界的点计入 W1，因此 W0 计数为 3。

```text
W0 avg = 12
W0 weighted_sum_raw = 76000
W0 weight_total_raw = 6000
W0 weighted_mean = 12.666666666666666
```

权重临界用例：

```text
123/1000 -> 1230/10000 exact
12345/10000 -> 1235/1000, delta=0.5, within_tolerance=true
30000/1000 with int16 target -> saturated 32767, E_WEIGHT_FIXED_OVERFLOW
12345/10000 repeated round trip -> fixed at 12350/10000
```

## 信号实测

构建二进制后启动慢速流，并在约 350ms 时发送 `SIGTERM`：

```bash
go build -o /tmp/downsample-demo ./cmd/demo
/tmp/downsample-demo --signal-demo --out <dir>
kill -TERM <pid>
```

实际输出：

```json
{
  "code": "E_STREAM_INTERRUPTED",
  "committed_seq": 22,
  "message": "stream stopped at last complete commit point"
}
```

这证明可处理退出信号到达后，流停止在下一个未提交样本之前，SQLite 中保留最后一个完整提交点。

## 未做事项

- 未使用 Docker 或外部监控/存储服务。
- 未访问网络服务做运行时验证；Go 依赖已固定在 `go.mod`/`go.sum` 且本机模块缓存可用。
- 未执行真实 `kill -9` 子进程测试；强杀中间提交点由两阶段提交顺序测试和 orphan row 恢复测试等价覆盖。

# 实际验证记录

验证时间：2026-11-27 06:01–06:02 (+08:00)

环境：

- `Linux LAPTOP-9TI5T1QQ 6.18.58.1-microsoft-standard-WSL2 x86_64`
- `Ubuntu 26.04.1 LTS`
- `go version go1.22.12 linux/amd64`
- SQLite：`modernc.org/sqlite v1.29.10`，纯 Go 驱动，本地文件数据库

完整命令日志：`artifacts/verification.log`；详细验收测试日志：`artifacts/acceptance-tests.log`。

## 自动化结果

```text
$ go test ./... -count=1
ok      meshpolicy      0.020s

$ go vet ./...
# 无输出，退出码 0

$ go test -race ./... -count=1
ok      meshpolicy      1.145s

$ go build -o /tmp/meshbench-final ./cmd/meshbench
build-ok
```

详细根包测试：

```text
--- PASS: TestThreeSchemaEquivalenceAndGateAnchors (0.00s)
--- PASS: TestIncrementalMatchesFullRecompute (0.00s)
--- PASS: TestAdmissionReplacementTruthAndGuards (0.00s)
--- PASS: TestCheckpointVersionCrashAndTruncation (0.01s)
--- PASS: TestSchedulerBudgetAndBackpressureCodes (0.00s)
--- PASS: TestHTTPWorkbenchAndStableInjectionError (0.00s)
--- PASS: TestGateDeniesWithoutCommittedBaseline (0.00s)
```

## 验收点对照

- 三版装载：`TestThreeSchemaEquivalenceAndGateAnchors` 对比策略优先级次序、尝试次序、选择器匹配集合、传播到达集合和最终判定；三版规范工程版本哈希也一致。
- 逐时刻裁决与门控：固定样本锚点覆盖 A-01～A-15，包含高优先级胜出、区间端口、未到达 marker、到达未确认、确认后应用新判定和旧判定回退。
- 无基线拒绝：`TestGateDeniesWithoutCommittedBaseline` 验证首个连接在 marker 未完成时返回 `E_GATE_DENIED_NO_BASELINE`，并带 marker 编号和 G1。
- 增量重算：`TestIncrementalMatchesFullRecompute` 覆盖优先级、动作、选择器标签和工作负载标签编辑；增量合并结果与全量重算 `reflect.DeepEqual` 级一致，离散容差为 0。
- 准入：`TestAdmissionReplacementTruthAndGuards` 核对两条真值替换规则 `R-REPLACE-EXPIRED-WINDOW`、`R-REPLACE-CONFIRMED-DEAD`，并验证零上限、优先级越界和活跃条目替换码。
- 检查点：`TestCheckpointVersionCrashAndTruncation` 验证 SQLite 保存/读取、版本匹配、事务内“子表已写但全局序号未提交”的模拟崩溃回滚、截断文件 `E_CHECKPOINT_TRUNCATED` 和版本不一致 `E_CHECKPOINT_VERSION_MISMATCH`。
- 预算/背压：`TestSchedulerBudgetAndBackpressureCodes` 验证越界 worker/buffer、worker 槽耗尽 `E_WORKER_BUDGET_EXHAUSTED` 和每命名空间缓冲满 `E_BACKPRESSURE`。
- HTTP：`TestHTTPWorkbenchAndStableInjectionError` 验证首页、V3 装载和 `E_ACTION_CONFLICT` 的 JSON 错误信封。

## 最终二进制 HTTP 冒烟

使用命令：

```bash
/tmp/meshbench-final -addr 127.0.0.1:18081 -db /tmp/meshbench-final-smoke.db
```

API 冒烟结果：

```text
equivalence True pv-622b5284630f1918
incremental True recomputed 12 reused 3 diffs 12
checkpoint 4 pv-81d472763286a0f4 workloads 6 queue 1
inject_http=500
inject_code E_CHECKPOINT_TRUNCATED
```

最终 UI 冒烟（`127.0.0.1:18082`）：

```text
home=200 bytes=13237
diff-panel=ok
{"ok":true,"data":{"service":"mesh-policy-workbench","status":"ready"}}
```

说明：冒烟中的增量编辑先修改了内存工程，因此随后保存的检查点版本哈希不同于原始三版等价哈希；这是预期行为。截断检查点注入返回 HTTP 500 和稳定码 `E_CHECKPOINT_TRUNCATED`。

## 手动异常注入巡检

以下异常类型均通过 `/api/inject` 返回稳定 `E_xxx`：

```text
overlapping_selector       E_SELECTOR_OVERLAP
missing_workload           E_WORKLOAD_MISSING
exited_workload            E_WORKLOAD_EXITED
selector_cycle             E_SELECTOR_CYCLE
invalid_port_range         E_PORT_RANGE_INVALID
action_conflict            E_ACTION_CONFLICT
policy_limit_zero          E_POLICY_LIMIT_ZERO
policy_limit_invalid       E_POLICY_LIMIT_INVALID
policy_limit_exceeded      E_NAMESPACE_POLICY_LIMIT
priority_out_of_range      E_PRIORITY_RANGE
replace_active             E_REPLACE_TARGET_ACTIVE
invalid_worker_budget      E_WORKER_BUDGET_INVALID
invalid_buffer_budget      E_BUFFER_BUDGET_INVALID
worker_exhausted           E_WORKER_BUDGET_EXHAUSTED
buffer_full                E_BACKPRESSURE
truncated_checkpoint       E_CHECKPOINT_TRUNCATED
version_mismatch           E_CHECKPOINT_VERSION_MISMATCH
unknown                    E_INJECTION_UNKNOWN
```

## 运行方式

```bash
go run ./cmd/meshbench -addr 127.0.0.1:18080 -db data/checkpoint.db
# 浏览器打开 http://127.0.0.1:18080/
```

工作台点击“装载 V1/V2/V3”“三版等价核对”，随后可逐时刻查看表格、施加增量编辑、执行准入替换、保存/恢复检查点并注入异常。

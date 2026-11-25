# 实际验证记录

验证时间：2026-11-26（Asia/Shanghai）

环境：

- Go：`go version go1.22.12 linux/amd64`
- 驱动：`modernc.org/sqlite v1.29.10`（纯 Go，无系统 SQLite/容器依赖）
- 模块缓存：工程内 `.gocache/mod`，构建缓存 `.gocache/build`
- 离线开关：`GOPROXY=off`

## 1. 单元与验收测试

命令：

```bash
source scripts/env.sh
go test ./... -count=1
```

实际结果：

```text
?       featurekernel/cmd/featurectl    [no test files]
?       featurekernel/internal/gen      [no test files]
?       featurekernel/internal/kernel   [no test files]
ok      featurekernel                   0.265s
ok      featurekernel/internal/checkpoint 0.089s
ok      featurekernel/internal/engine   0.003s
ok      featurekernel/internal/incremental 0.011s
ok      featurekernel/internal/schema   0.008s
```

竞态检测：

```bash
go test -race ./... -count=1
```

实际结果：根包、`checkpoint`、`engine`、`incremental`、`schema` 均 `ok`。

覆盖的关键测试：

- `internal/schema`：三版 schema 规范化后特性、记录、时刻、权重、聚合次序一致；未知字段与不支持版本返回稳定码。
- `internal/engine`：生成真值比对、整数/非整数归属、半开区间边界、阈值相等回退重扫、双路径等价、worker/buffer 背压、异常水位。
- `internal/checkpoint`：SQLite 分批提交、关闭重开续算、JSON/SQLite 截断、版本不匹配后整段重算。
- `internal/incremental`：事件改/追/删与宽度/聚合函数编辑后的闭包失效、缓存复用、增量与全量逐窗口一致。
- 根包：固定种子两次生成逐字节一致；编译真实 CLI 子进程验证 SIGTERM 与 SIGKILL 注入恢复。

## 2. 确定性生成

同一固定种子 `20261126` 输出到两个临时目录后：

```bash
diff -r /tmp/fk-seed-a.*/ /tmp/fk-seed-b.*/
# 无差异
BYTE_IDENTICAL
```

每批样本规模：

- 4 个特征、1 个派生指标
- 78 条连续流记录（含 2 个水位/屏障）
- 70 个非空窗口结果
- 28 个迟到重算窗口
- 28 条按“事件 × 归属窗口”记录的丢弃明细

当前 `data/` 中生成 3 批工程，每批包含 `v1/v2/v3` 样本与 truth JSON。

## 3. 本地 verify

命令：

```bash
source scripts/env.sh
go run ./cmd/featurectl verify --data data
```

实际结果：3 个 batch 均输出 `PASS`，三版装载结果、生成器真值窗口聚合、重算集合与丢弃集合一致。

## 4. 异常业务码诊断

命令：

```bash
go run ./cmd/featurectl diagnose
```

实际结果：`"all_stable": true`，覆盖：

- `E_DEFINITION_NEGATIVE_WIDTH`
- `E_DEFINITION_ZERO_STEP`
- `E_AGG_WEIGHT_OUT_OF_RANGE`
- `E_WATERMARK_BEFORE_COMMITTED_WINDOW`
- `E_BUDGET_WORKERS_OUT_OF_RANGE`
- `E_BUDGET_BUFFER_OUT_OF_RANGE`
- `E_BACKPRESSURE_BUFFER_FULL`
- `E_BACKPRESSURE_THREAD_EXHAUSTED`
- `E_SCHEMA_UNKNOWN_FIELD`
- `E_CHECKPOINT_TRUNCATED`
- `E_CHECKPOINT_VERSION_MISMATCH`

每个错误均带可定位 `context`，诊断只写内存工程和临时检查点，不修改 `data/samples` 源样本。

## 5. 强制终止恢复

自动化测试：`TestCLIGracefulSignalAndForcedCrashRecovery`。

- 优雅信号：`consume --term-after-commit 12` 在第 12 个提交点后给自己发 SIGTERM，输出 `graceful_stop: true` 且 `committed_seq=12`；重启从 12 续算。
- 强制终止：`--crash-point after_window_states` 在 SQLite 事务内写完窗口状态、尚未更新全局序号时发送真实 SIGKILL；事务回滚，重启读到的 `start_cursor=0`，不会读到半提交窗口。
- 后续分批续算得到与 truth 相同的 70 个窗口和 28 条丢弃明细。

## 6. 离线重复命令

```bash
make cache     # 首次联网填充 .gocache
make test      # GOPROXY=off go test ./... -count=1
make verify    # 离线校验 data/
make diagnose  # 离线异常码诊断
make demo      # 生成、校验、诊断、SQLite 续算与 JSON 快照
```

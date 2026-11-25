# Feature Kernel：流式特征窗口与中断续算验收内核

纯 Go 实现的可集成领域组件和本地诊断 CLI。它用固定种子生成三版 schema 的同一批特征工程、解析真值与检查点，覆盖 schema 归一化、滑动窗口归属、聚合、水位/迟到、并行背压、SQLite 崩溃续算和增量失效。

不依赖 Docker、系统 SQLite、外部特征存储、消息系统或账号；SQLite 使用纯 Go 驱动 `modernc.org/sqlite`。

## 快速开始

要求 Go 1.22+。已在 WSL/Linux、`go1.22.12 linux/amd64` 验证。

```bash
# 可选：把依赖填入工程内 .gocache（首次需要网络）
make cache

# 后续可完全离线
source scripts/env.sh
make test
make verify
make diagnose
```

一键本地演示：

```bash
make demo
```

它会重新生成 `data/`、校验三版样本、输出异常码诊断、写入 `data/checkpoints/demo.sqlite`，并导出 JSON 快照。

## CLI

```bash
go run ./cmd/featurectl generate --data data --seed 20261126 --batches 3
go run ./cmd/featurectl verify   --data data
go run ./cmd/featurectl diagnose

go run ./cmd/featurectl consume \
  --db data/checkpoints/demo.sqlite \
  --sample data/samples/batch-001.v3.json \
  --batch-size 7

go run ./cmd/featurectl snapshot \
  --db data/checkpoints/demo.sqlite \
  --out data/checkpoints/batch-001.snapshot.json \
  --project-id feature-project-001 \
  --batch-id batch-001
```

崩溃/信号诊断参数：

- `--crash-point after_window_states`：SQLite 事务内写完窗口状态后发送真实 SIGKILL。
- `--term-after-commit N`：第 N 个提交点后发送 SIGTERM，验证优雅停止停在最后完整提交点。

## 目录

- `internal/kernel`：规范模型、错误码、窗口公式、聚合器。
- `internal/schema`：v1/v2/v3 DTO、严格解码和双向转换。
- `internal/gen`：确定性工程与 truth 生成器。
- `internal/engine`：窗口 Runtime、双聚合路径、并行调度和背压。
- `internal/checkpoint`：SQLite 事务、恢复 Runner、JSON 快照。
- `internal/incremental`：编辑失效、传播闭包、缓存复用。
- `cmd/featurectl`：本地 CLI。
- `data/samples`：三版 schema 样本。
- `data/truth`：逐窗口真值、重算集合和丢弃记录。
- `docs/DESIGN.md`：口径与实现说明。
- `docs/VERIFICATION.md`：实际测试和验证结果。

## 关键口径

- 窗口半开 `[start, end)`，归属规则为 `R-WIN-INT-RATIO` 或 `R-WIN-FRACTIONAL-RATIO`。
- 路径规则为 `PATH-WIDTH-STEP-RATIO-GT-THRESHOLD`；比值大于阈值才增量累积，相等时按声明回退逐窗重扫。
- 三版 schema 的时刻、对齐基准、权重刻度和水位表达最终映射到同一 canonical model；生成器保证首点落在整点，因此两种对齐基准边界相同。
- 迟到量超过允许延迟时输出稳定记录码 `E_LATE_BEYOND_ALLOWED`，并带窗口编号和迟到毫秒数。
- 所有输入/参数异常返回 `E_*` 业务码和可定位 context，不静默降级。

## 验证状态

`go test ./... -count=1` 与 `go test -race ./... -count=1` 均通过；固定种子两次生成目录 `diff -r` 无差异。详细真实输出见 `docs/VERIFICATION.md`。

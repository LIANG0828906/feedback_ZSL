# 实际验证记录

验证时间：2026-11-27 03:18 (+08:00)

环境：

- Linux/WSL Ubuntu 工作区，`linux/amd64`
- `go version go1.22.12 linux/amd64`
- 系统 SQLite 运行库：Python `sqlite3.sqlite_version = 3.46.1`
- 未使用 Docker、外部数据库服务或第三方 Go 模块；`go.mod` 无 `require`

## 1. 自动化测试

命令：

```bash
go test ./...
go test -race ./...
CGO_ENABLED=0 go test ./...
go vet ./...
```

实际结果：

```text
?       vpfstack/cmd/vpfd       [no test files]
?       vpfstack/internal/sqlite [no test files]
ok      vpfstack        ...
```

顶层测试共 15 个，全部 PASS；其中增量测试含 3 个子场景，SQLite 两个测试在 cgo/linux + `libsqlite3.so.0` 可用时实际运行：

- `TestThreeSchemaLoadersAndTreeEquivalence`
- `TestSchemaErrorsPointAtFieldAndVersion`
- `TestIntegerConversionsOverflowTruncationAndRoundTrip`
- `TestHighLowHighExportKeepsChannelsAndTime`
- `TestTreeMismatchReturnsECodeAndFirstLayer`
- `TestRenderIsPixelDeterministic`
- `TestThreePathsPixelAndSequenceIdentical`
- `TestIncrementalPreciseInvalidationScenarios`
- `TestChunkFailureIsolationAndRetry`
- `TestSchedulerQueueBudgetWaitRecorded`
- `TestDefaultFillTraceRecordsOldAndDefault`
- `TestCompareFramesLocatesChunkAndPixel`
- `TestTruthSetVerifiesEveryAnalyticSample`
- `TestSQLiteCheckpointThreePaths`
- `TestSQLiteIncrementalOpacity`

`go test -race ./...` 通过；`CGO_ENABLED=0 go test ./...` 也通过，此时内置 SQLite cgo 桥不注册，SQL 测试自动 skip，内存/文件存储路径仍完整测试。

## 2. 样本生成与 CLI 总验证

命令：

```bash
go run ./cmd/vpfd generate
go run ./cmd/vpfd verify
```

生成文件：

```text
samples/seed-20261127/project.v1.json
samples/seed-20261127/project.v2.json
samples/seed-20261127/project.v3.json
samples/seed-20261127/truth.json
samples/seed-20261127/loss-v1.json
samples/seed-20261127/loss-v3.json
```

`verify` 的实际摘要：

```json
{
  "incremental": {
    "group_visibility": [0],
    "opacity_badge": [0],
    "ripple_source": [0, 1, 3, 4]
  },
  "ok": true,
  "pixel_identical": true,
  "realtime_digest": "ccb5814145ced813df01248e7efb487233b3ba3c10030f33fdaeb4bc96215833",
  "report": "artifacts/verification.json",
  "sequence_identical": true,
  "sqlite_verified": true
}
```

完整报告中 `"truth_verified": true`，共核对 18 个逐帧涟漪权重采样点。

完整机器报告写入 `artifacts/verification.json`。三版本字段痕迹数量分别为：

```text
v1: 22
v2: 25
v3: 33
```

平坦表、父引用节点表和嵌套树解析出的活动层序均为：

```text
ground_base -> ripple_mask -> slate_badge
```

隐藏的 `guide_overlay` 因祖先 `guides_group.visible=false` 不进入合成栈。

## 3. 异常码输出

`artifacts/verification.json` 中实际捕获：

| 场景 | 代码 | 字段路径 | 来源版本 |
| --- | --- | --- | --- |
| `{"version":9}` | `E_SCHEMA` | `$.version` | 9 |
| v1 删除 `project_id` | `E_SCHEMA` | `$.project_id` | 1 |
| tick 乘法越界 | `E_OVF` | `$.demo.overflow_ticks` | 含两个操作数 |
| 1 像素缓冲容纳 1024 像素块 | `E_BUDGET` | 无（结构化 Error） | - |
| 交换前两层 | `E_TREE` | `$.resolved[0]` | 左 `ground_base` / 右 `ripple_mask` |
| v1 降级导出 | `E_LOSSY` 报告 | 5 条组层级 + 4 条 alpha 约定 | - |

## 4. SQLite 外部可读性检查

CLI 生成 `artifacts/vpf-checkpoints.sqlite` 后，用系统 `python3` 的标准库 `sqlite3` 独立查询，实际得到 6 行：

```text
(0, 0, 'f000-c000', 1024, 4096, 2, 1, 2, 7)
(0, 1, 'f000-c001', 1024, 4096, 1, 1, 1, 2)
(0, 2, 'f000-c002', 1024, 4096, 1, 1, 1, 3)
(0, 3, 'f000-c003', 1024, 4096, 1, 1, 1, 4)
(0, 4, 'f000-c004', 1024, 4096, 1, 1, 1, 5)
(0, 5, 'f000-c005', 1024, 4096, 1, 1, 1, 6)
```

列顺序为 `frame_index, chunk_index, chunk_id, pixel_count, length(pixel_blob), stack_snapshot_version, ripple_source_version, param_version, commit_seq`。

块 0 初次提交序号为 1；编辑徽章透明度后，未受影响块 1–5 保持原记录，块 0 在同一事务中替换并得到新序号 7、`stack_snapshot_version=2`、`param_version=2`。这验证了精确失效与旧行未被重写。

## 5. 诊断页面冒烟

命令：

```bash
go build -o /tmp/vpfd ./cmd/vpfd
/tmp/vpfd serve -addr 127.0.0.1:18080
curl -fsS http://127.0.0.1:18080/ -o /tmp/vpf-page.html
curl -fsS http://127.0.0.1:18080/api/state -o /tmp/vpf-state.json
```

实际结果：

```text
page_bytes=6661
state_bytes=5937
页面包含：分块检查点状态、失效传播示意、ground_base、ripple_mask
/api/state verification:
{'pixel_identical': True, 'sequence_identical': True,
 'invalid_chunks': [0], 'reused_chunks': [1, 2, 3, 4, 5]}
```

## 6. 复现建议

验收者可直接执行：

```bash
go test -v ./...
go run ./cmd/vpfd generate
go run ./cmd/vpfd verify
go run ./cmd/vpfd serve
```

然后打开 `http://127.0.0.1:18080`，或用任意 SQLite 工具检查 `artifacts/vpf-checkpoints.sqlite`。

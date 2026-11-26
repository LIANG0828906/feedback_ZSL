# 首版验收口径追踪

| 要求 | 主要实现 | 自动化验证 / 入口 |
|---|---|---|
| 1. 加载与 `E_SCHEMA` 校验 | `internal/schema/load.go`；模型在 `internal/model/model.go` | `internal/schema/schema_test.go`；`wardrobe validate --project samples/invalid_schema.json` |
| 2. 顺序无关装配冲突判定 | `internal/assembly/assembly.go` | `internal/assembly/assembly_test.go` 对同一候选集打乱 20 次并 `DeepEqual` |
| 3. 确定性合批与偏移 | `internal/batching/batch.go` | `internal/batching/batch_test.go`；`samples/truth.json`；`wardrobe verify` |
| 4. 材质槽/预算仲裁与 `E_ARB` | `internal/batching/arbiter.go`、`internal/batching/batch.go` | `TestStrictTieReturnsEArbWithParties`；`samples/ambiguous_arb.json` |
| 5. 依赖图、缓存键、精确失效 | `internal/depgraph/graph.go`、`internal/cache/cache.go` | `internal/depgraph/graph_test.go`；`wardrobe incremental-demo` |
| 6. 增量重划分与 `E_DIFF` 定位 | `internal/pipeline/pipeline.go`、`internal/batching/rebuild.go`、`internal/compare/compare.go` | `TestThreePathsTruthIncrementalAndCheckpoint`、`TestCompareLocatesFirstMismatchAsEDiff`；`wardrobe diff-demo` |
| 7. SQLite 原子检查点与恢复版本边界 | `internal/checkpoint/store.go`、`internal/sqlitefile/`、`internal/atomicfile/atomicfile.go` | `internal/sqlitefile/sqlitefile_test.go`、`TestVersionMismatchReturnsEVer`；verify 中真实 SIGKILL 子进程 |
| 8. 三路径一致、有界并行、失败隔离 | `internal/pipeline/pipeline.go`、`internal/scheduler/scheduler.go` | `internal/scheduler/scheduler_test.go`、`TestThreePathsTruthIncrementalAndCheckpoint`；`wardrobe verify` |

## 一键验收

```bash
make verify
# 或
go run ./cmd/wardrobe verify --dir samples --work artifacts/verify
```

预期最后输出：

```json
{
  "ok": true,
  "message": "all acceptance checks passed"
}
```

## 固定样本摘要

- 种子：`20261127`
- 挂点：10
- 部件：15
- 合法装配：9
- 真值批次：4
- 冲突类别：挂点争用、显式互斥、蒙皮不相容
- 增量编辑 `waist_belt`：仅失效批次 `3`，复用批次 `1、2、4`
- SIGKILL 边界：第 2 批提交后杀死进程，重启仅见 `batch:000001`、`batch:000002`，resume 后补齐 3、4

详细实际输出见 `docs/VALIDATION.md`。

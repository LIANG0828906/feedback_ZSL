# 实际验证记录

验证日期：2026-11-26（Asia/Shanghai）

环境：

```text
go version go1.22.12 linux/amd64
CGO_ENABLED=1
SQLite driver: github.com/mattn/go-sqlite3 v1.14.24
```

## 自动化测试

执行：

```bash
go test ./...
go test -race ./...
go vet ./...
```

结果：

```text
ok  graphlab/internal/app
ok  graphlab/internal/checkpoint
ok  graphlab/internal/engine
ok  graphlab/internal/generator
ok  graphlab/internal/loader
```

`go vet` 通过；构建阶段只出现第三方 SQLite C 文件的一条警告：

```text
sqlite3-binding.c: warning: assignment discards 'const' qualifier from pointer target type
```

测试覆盖：

- 相同种子生成文档逐字节一致。
- v1/v2/v3 core digest 一致；v3 层级降级标记正确。
- 真值分片顶点顺序、边顺序、索引键集合逐项一致。
- 初始 8 查询访问顶点/边、结果、路径、状态与真值一致。
- `index` 与 `scan` 两种布局结果一致。
- worker=1 与 worker=4 的索引键、访问序列、缓存 ID 一致。
- 7 个操作逐步增量重建与全量重建逐查询一致。
- 跨分片共享索引键的引用生命周期和版本递增正确，引用归零后回收。
- 线程预算、缓冲预算返回对应业务码。
- 检查点提交/恢复、版本不匹配、失败点回滚、截断文件分类正确。
- 取消上下文的演示只保留最后完整提交点。

## 三版一致性

命令：

```bash
go run ./cmd/graphlab validate \
  -sample artifacts/sample-seed42 -out artifacts/validation \
  -workers 4 -buffer 4096
```

关键结果：

```text
PASS schema_core_equivalence
PASS hierarchy_optional_degradation
PASS v1_shard_order_and_keys
PASS v1_query_truth
PASS v2_shard_order
PASS v2_index_and_query_truth
PASS v2_query_results_equal_v1
PASS v3_shard_order
PASS v3_query_truth_with_degraded_hierarchy
PASS v3_query_results_equal_v1
PASS index_vs_scan_layout
PASS concurrency_determinism
validation PASS core=7b4200a242f9e64dba30116397397eb5490f2e8f workers=4 buffer=4096
```

即 12/12 检查通过。

初始样本分片真值：

```text
s0: 6 vertices, 8 edges, 44 index keys
s1: 6 vertices, 8 edges, 45 index keys
s2: 6 vertices, 8 edges, 44 index keys
```

## 演示流程

命令：

```bash
go run ./cmd/graphlab demo \
  -sample artifacts/sample-seed42 \
  -checkpoint artifacts/demo/checkpoint.db \
  -out artifacts/demo -workers 4 -buffer 4096
```

实际结果：

```text
loaded v1/v2/v3 core=7b4200a242f9e64dba30116397397eb5490f2e8f equivalent=true
v3 hierarchy=false degraded=true
initial checkpoint: seq=0, version=1, 3 shards, 108 keys, 8 cached queries
op=1 vertex_update  affected shard s0, invalidated 4, reused 4, incremental/full=PASS truth=PASS
op=2 vertex_add     affected shard s2, invalidated 1, reused 7, incremental/full=PASS truth=PASS
op=3 edge_add       affected shard s2, invalidated 0, reused 8, incremental/full=PASS truth=PASS
op=4 edge_update    affected shard s1, invalidated 1, reused 7, incremental/full=PASS truth=PASS
op=5 edge_delete    affected shard s2, invalidated 0, reused 8, incremental/full=PASS truth=PASS
op=6 vertex_delete  affected shard s1, invalidated 2, reused 6, incremental/full=PASS truth=PASS
op=7 reshard        affected s1/s1a/s1b, invalidated 5, reused 3, incremental/full=PASS truth=PASS
final checkpoint: seq=7, version=8
checkpoint recovered seq=7 version=8, query diffs=0
forced version mismatch -> E_CHECKPOINT_VERSION_MISMATCH, checkpoint untouched
version-mismatch fallback full rebuild query diffs=0
```

完整逐查询访问序列、失效原因、每个索引键的引用/版本/关联缓存编号见：

```text
artifacts/demo/demo-report.json
```

报告中的 `initial_shard_metrics` 与重划分操作的 `shard_metrics` 仅记录实际微秒耗时、键数和实体引用数，用于诊断；所有正确性断言都不读取这些耗时值。

## 异常注入

命令：

```bash
go run ./cmd/graphlab faults \
  -sample artifacts/sample-seed42 -out artifacts -workers 4 -buffer 4096
```

结果：

```text
PASS out_of_range_attribute_scale     E_BAD_ATTRIBUTE_SCALE
PASS edge_points_to_missing_vertex    E_MISSING_VERTEX
PASS overlapping_shard_ranges         E_OVERLAP_SHARD
PASS worker_budget_exceeded           E_THREAD_BUDGET
PASS index_buffer_exhausted           E_BUFFER_BUDGET
PASS invalid_operation_rollback       E_MISSING_VERTEX
PASS truncated_checkpoint             E_CHECKPOINT_TRUNCATED
PASS version_mismatch_snapshot        E_CHECKPOINT_VERSION_MISMATCH

source-unchanged=true
runtime-still-usable=true
checkpoint-still-usable=true
faults all-expected=true
```

详细上下文见 `artifacts/faults-report.json`。

## 复现说明

所有样本、报告和 SQLite 文件都在工作区本地生成。重新执行上述命令会得到相同业务结果；SQLite 文件内部时间字段会随运行时间变化，但不影响恢复、索引、缓存和查询断言的确定性。

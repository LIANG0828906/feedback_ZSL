# 实际验证记录

环境：Linux amd64，Go 1.22.12。

测试集共 24 个 `Test*`、4 个 `Benchmark*`。

## 静态与单元测试

```text
$ go version
go version go1.22.12 linux/amd64

$ go test ./...
ok  	spatialpick
?   	spatialpick/cmd/demo	[no test files]
?   	spatialpick/cmd/scenegen	[no test files]

$ go test -race ./...
ok  	spatialpick

$ go test -count=5 ./...
ok  	spatialpick

$ go vet ./...
（无输出，退出码 0）

$ GOOS=windows GOARCH=amd64 go build ./...
（无输出，退出码 0）
```

覆盖的验收项：

- 缺失字段 `E_SCHEMA`、父子环 `E_CYCLE`、几何/三角引用越界 `E_REF`，且失败不返回部分文档；
- 真值矩阵摘要、内容哈希和真值字段校验；
- 射线/框选/点查询与真值逐项一致，模糊位置逐项一致；
- 32 goroutine 重复查询结果逐位相同；
- `E_PARA`、`E_AMBIG` 上下文；
- 非后代缓存指针复用、旧版本 `E_STALE`、误差轨迹单调；
- 漏项失效计划 `E_DEP`，失败不改状态；
- 过期限批次与非法矩阵批次 `E_TXN` 整批回滚；
- 8 个独立 goroutine 执行同一编辑序列，int64 世界矩阵逐位相同；
- 三路径（增量、检查点恢复、全量派生重建）逐项一致；
- `E_DIVERGE` 定位首个节点/数据项；
- 单条重建失败隔离、`E_STALE`、重试恢复、并行重建期间查询可执行；
- 快照恢复、校验和损坏、场景版本/内容哈希不匹配整份回退；
- 取消写入保留旧快照、8 个并发写入后快照仍完整可读；
- 运行时几何体增删的节点局部失效、依赖图边、索引可见性、快照恢复。

## 确定性样本

```text
$ go run ./cmd/scenegen -out data
generated data/scene.json and data/truth.json
nodes=61 geometries=31 rays=12 boxes=5 points=2 batches=4
content_hash=25d2912cf821b7c3861c8ffc2bd5268a57afc3b7abedd47c7be38d31de484367
```

文件 SHA-256（生成后）：

```text
scene.json     0f3d77a7ddf49b715b124d1c43d69b540ece911b1c9243ec31179265eb907b29
truth.json     d58a397b627f3b12be59a8d16655791bda3662a919979e15173110044efe7a9a
```

## 可运行示例

```text
$ go run ./cmd/demo -data data -snapshot data/editor.snapshot -checkpoint 2
loaded scene=deterministic-editor-v1 nodes=61 geometries=31 precision=6 hash=25d2912cf821
base truth: rays=12 boxes=5 points=2 ambiguities=3
batch batch-001-translate: invalidated caches=54 index_entries=27 revision=1
batch batch-002-rotate: invalidated caches=34 index_entries=17 revision=2
batch batch-003-scale: invalidated caches=26 index_entries=13 revision=3
batch batch-004-mixed: invalidated caches=30 index_entries=15 revision=4
edited truth: matrices max_delta=4.77e-06 rays=12 boxes=5 points=2 ambiguities=2
three paths agree: checkpoint=2 revision=4 ambiguous_queries=2
snapshot restored: revision=4 index_version=5 bytes=377973
OK
```

示例中的 `ray-ambig-sphere`、`box-ambig-edge`、`point-ambig-sphere-surface` 均按真值返回 `E_AMBIG`，并在三路径间保持完全相同的查询/节点/几何体/深度位置。

## 性能基准

```text
$ go test -run '^$' -bench . -benchmem .
goos: linux
goarch: amd64
pkg: spatialpick
cpu: Intel(R) Core(TM) i7-14650HX

BenchmarkPickRay-24       1000000    1006 ns/op    928 B/op   11 allocs/op
BenchmarkSelectBox-24      103095   11118 ns/op   1765 B/op    9 allocs/op
BenchmarkPivotCommit-24      4594  330853 ns/op 317603 B/op  866 allocs/op
BenchmarkFullBuild-24        1863  640699 ns/op 476692 B/op 1460 allocs/op
```

说明：`PivotCommit` 为已加载引擎上的计划 + 提交增量路径；`FullBuild` 为从头构建全部派生与索引。基准受当前主机和 Go 版本影响，仅用于记录本环境结果。

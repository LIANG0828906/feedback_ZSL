# 验收记录

验证环境：

```text
OS: Linux amd64 (WSL Ubuntu)
Go: go1.22.12 linux/amd64
Module: layerforge
External Go dependencies: none
Container/network: not used
```

## 自动测试

命令：

```bash
gofmt -l .
go test ./... -count=1
```

结果：

```text
?   	layerforge/internal/apperr	[no test files]
?   	layerforge/internal/build	[no test files]
?   	layerforge/internal/generator	[no test files]
ok  	layerforge/cmd/layerforge
ok  	layerforge/internal/engine
ok  	layerforge/internal/loader
ok  	layerforge/internal/sqlitefile
ok  	layerforge/pkg/layerforge
```

覆盖点：

- `loader`：三版语义哈希一致、缓存键/层摘要与真值一致、未知字段稳定业务码；
- `engine`：冷构建、重启全命中、不同线程数摘要一致、2 轮中断续建、低内存/低延迟降级、轮数不足批处理、非法预算码、增量编辑、共享清理冲突、终止层清理、截断/版本错误、提交前/后故障、信号式取消；
- `sqlitefile`：SQLite 文件往返、截断检测；
- `cmd/layerforge`：完整演示验收脚本。

## 三版装载

```bash
go run ./cmd/layerforge verify
```

实际结果：

```text
project.v1.json schema=layerforge/v1 hash=sha256:7e81a33b1f5e1d8241aae12708ef24c71ee2d60d4c81caa2de507f6adc969396 truth=OK
project.v2.json schema=v2 hash=sha256:7e81a33b1f5e1d8241aae12708ef24c71ee2d60d4c81caa2de507f6adc969396 truth=OK
project.v3.json schema=v3 hash=sha256:7e81a33b1f5e1d8241aae12708ef24c71ee2d60d4c81caa2de507f6adc969396 truth=OK
verified seed=42 steps=8 layers=11
```

## 完整演示

```bash
go run ./cmd/layerforge demo -workdir .layerforge-demo
```

关键实际输出：

```text
cold hits=0 builds=8 rounds=6 codes=[E_BUDGET_LATENCY]
  round 1 built=[step:compile step:docs] deferred=[] latency=14/17ms remaining=3ms
  round 2 built=[step:test step:lint] deferred=[step:package(latency#1)] latency=8/17ms remaining=9ms
  round 3 built=[step:package] deferred=[] latency=12/17ms remaining=5ms
  round 4 built=[step:scan] deferred=[] latency=6/17ms remaining=11ms
  round 5 built=[step:image] deferred=[] latency=10/17ms remaining=7ms
  round 6 built=[step:release] deferred=[] latency=8/17ms remaining=9ms
restart hits=8 builds=0 rounds=0
incremental edits=step-4 hits=4 builds=4
  rebuild step=4 layer=step:package source=edit:command
  rebuild step=5 layer=step:scan source=upstream:4:edit:command
  rebuild step=6 layer=step:image source=upstream:4:edit:command
  rebuild step=8 layer=step:release source=upstream:4:edit:command
incremental_vs_full=identical
cleanup conflict_code=E_CLEANUP_CONFLICT removed=[step:release] rebuild_steps=[8] rebuilt_hits=7
interruption persisted_rounds=2 resumed_hits=4 resumed_builds=4
source_unchanged=true workspace=.layerforge-demo
PASS
```

异常注入结果：

```text
corrupt-field-type     E_SCHEMA_TYPE
unknown-field          E_SCHEMA_FIELD
dependency-cycle       E_GRAPH_CYCLE
missing-base           E_GRAPH_MISSING_REF
share-conflict         E_GRAPH_SHARE_CONFLICT
out-of-range-thread    E_BUDGET_THREADS
out-of-range-memory    E_BUDGET_MEMORY
buffer-backpressure    E_BACKPRESSURE_QUEUE
cleanup-conflict       E_CLEANUP_CONFLICT
truncated-checkpoint   E_CHECKPOINT_TRUNCATED
version-mismatch       E_CHECKPOINT_VERSION + reset rebuild
```

## SQLite 外部校验

项目运行不依赖 Python 或 SQLite 工具。以下检查只用于确认自写文件是真实 SQLite 数据库：

```python
import sqlite3
c = sqlite3.connect(".layerforge-demo/checkpoint.db")
assert c.execute("pragma integrity_check").fetchone()[0] == "ok"
print(c.execute("select count(*) from layers").fetchone()[0])
```

实际结果：两个检查点（正常与中断续建）均为 `integrity_check=ok`，`layers=11`、`cache_index=8`，四张表均可由 SQLite 读取。

## 提交阶段故障

使用同一个 v1 工程和 `before-rename` 故障点，在第 3 轮提交前模拟进程丢失：

```text
persisted_rounds_after_fault= 2
integrity_after_fault= ok
resume_hits= 4 resume_builds= 4 final_rounds= 6
```

即故障前两轮完整保留，重启后复用 4 个已提交层并完成剩余 4 层。另用 v2 完成冷构建后直接装载 v3 打开同一检查点，结果为 `hits=8 builds=0`，说明缓存键和层摘要不受 schema 表达差异影响。

## 本次关键结论

- 三版文档装载后的步骤顺序、层集合、基础层引用和共享关系一致；
- 缓存键和层摘要与生成器真值逐层一致；
- 1/2/8 个工作线程及中断批次不改变最终摘要；
- 低内存、低延迟、轮数不足会显式降级且结果与全量构建一致；
- 第 2 轮中断后，重启只复用已提交的 4 层并构建剩余 4 层；
- 编辑第 4 步只重建 4/5/6/8，另外 4 步命中；
- 共享层清理被拒绝，终止层清理后仅重建该层；
- 所有要求的异常均返回稳定业务码，源工程语义哈希不变。

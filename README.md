# GraphLab：三版图工程装载、分片索引、遍历缓存与检查点初版

这是一个完全本地、确定性的 Go 图存储原型，覆盖题目要求的装载、分片索引、遍历、缓存失效、增量重建、并发预算、SQLite 检查点恢复和异常注入链路。运行时不访问网络、真实图数据库或任何账号；样本与真值全部由固定种子生成器产出。

## 环境与依赖

- 已在当前 Linux/WSL 环境验证：`go version go1.22.12 linux/amd64`，`CGO_ENABLED=1`，`gcc` 可用。
- SQLite 使用 `github.com/mattn/go-sqlite3 v1.14.24`，本地 CGO 编译。
- 工作目录中执行 `go mod init graphlab`，随后 `go mod tidy` 已完成。
- Go 1.22 编译驱动时可能出现一条第三方 SQLite C 源文件的 `-Wdiscarded-qualifiers` 编译警告；不影响构建、测试或 `go vet`。

## 快速运行

```bash
# 编译检查
go test ./...
go vet ./...

# 生成固定种子样本与真值
go run ./cmd/graphlab generate -seed 42 -out artifacts/sample-seed42

# 三版一致性、分片排序/索引键、遍历、布局和并发确定性验证
go run ./cmd/graphlab validate \
  -sample artifacts/sample-seed42 -out artifacts/validation \
  -workers 4 -buffer 4096

# 装载三版、逐操作展示失效、增量/全量对照、SQLite 提交与恢复
go run ./cmd/graphlab demo \
  -sample artifacts/sample-seed42 \
  -checkpoint artifacts/demo/checkpoint.db \
  -out artifacts/demo -workers 4 -buffer 4096

# 异常注入，打印稳定 E_xxx 与定位上下文
go run ./cmd/graphlab faults \
  -sample artifacts/sample-seed42 -out artifacts -workers 4 -buffer 4096
```

也可以直接用已生成的 `artifacts/sample-seed42`、`artifacts/validation/validation-report.json`、`artifacts/demo/demo-report.json` 与 `artifacts/faults-report.json` 检查结果。`demo` 支持 `SIGINT`/`SIGTERM`；每个操作完成后立即提交检查点，收到信号时保留最后一个完整提交点。

## 三版 schema 映射

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 制品 | `project_v1.json` | `project_v2.json` | `project_v3.json` |
| 字段风格 | 扁平、snake_case | 顶点按标签嵌套树、camelCase | 扁平 `nodes`/`links`、短字段名 |
| 顶点/边清单 | 顶层 `vertices` / `edges` | `verticesByLabel` / `edgesByHierarchy` 嵌套树 | `nodes` / `links` |
| 定点属性 | 千分比 `scale=1000` | 万分比 `scale=10000` | 千分比 `scale=1000` |
| 时间 | tick，`millis_per_tick=2` | 毫秒 | tick，`tick_ms=1` |
| 容量 | byte | KiB | byte |
| 可选层级标签 | 启用 | 启用 | 字段缺失，降级为 disabled |
| 内部统一 | 毫秒、字节、百万分定点 | 同左 | 同左 |

装载后三版 core digest 实际为：

```text
7b4200a242f9e64dba30116397397eb5490f2e8f
```

v3 的 `HierarchyEnabled=false` 且 `HierarchyDegraded=true`，降级说明保存在模型与报告中；跨版本 core 对比不把可选层级字段当作核心差异。

## 关键确定性规则

- 顶点键 = 顶点 ID；边分片 = 边源顶点键。区间按 `[low, high)` 判定，重叠与空区间立即返回业务错误。
- 分片内顶点/边按生成器声明的 `decl_index` 排序，不按装载批次或 map 遍历顺序。
- 索引键由标签、属性分桶、字符串前缀和邻接源确定性生成，例如：

```text
v:label:blue
v:int:rank:b2
v:fixed:score:b1
v:string:name:p:alp
e:out:v0006
e:out:v0006:lvl:tier-c
```

- 遍历邻居按边声明次序枚举；访问序列、边序列、结果集合与真值逐查询比较。
- 缓存 ID 仅由逻辑查询规范串的 SHA-1 派生，例如 `C-658c26790f46`，不受 worker 数、调度顺序、提交批次或索引布局影响。
- 版本化索引键被多个分片引用时不会因单一分片编辑而整体删除；引用归零后才回收。每次受影响编辑递增版本并触发依赖缓存失效。

## 行程序列

固定种子 42 包含 18 个初始顶点、24 条有向边、3 个分片、8 条查询和 7 个逻辑操作：

1. 顶点属性/标签区间更新
2. 顶点新增
3. 边新增
4. 边权重与层级标签更新
5. 边删除
6. 顶点删除（级联相邻边）
7. s1 重划分成 s1a/s1b，整体替换受影响分片索引层

每个操作完成后，演示都会：

- 对全部查询输出访问顶点、访问边、结果顶点和缓存编号；
- 输出失效缓存及其命中的依赖来源；
- 执行一次全新运行时全量重建，并比较全部查询结果；
- 把已提交序号、分片索引状态、索引键引用/版本、缓存层和工程版本写入 SQLite。

## SQLite 检查点

数据库表：

- `meta`：格式版本、工程 ID/版本、全局提交序号、core digest、完整工程快照。
- `shard_index`：分片版本、顶点顺序、边顺序、分片内键贡献。
- `index_key`：全局索引键版本、引用分片集合、实体集合。
- `cache_entry`：查询缓存、结果、依赖、构建版本、命中计数。
- `commit_log`：提交序号、工程版本、core digest、时间。

单次检查点在一个事务中依次写入分片索引、索引引用、缓存、提交日志，最后更新全局提交序号并提交。进程在任意失败点终止时事务回滚，重启不会看到半提交状态。恢复前执行 `quick_check`，校验格式版本、工程 ID/版本、快照 digest 和提交日志；版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`，调用方必须整段全量重建。

## 稳定错误码

| 场景 | 业务码 |
|---|---|
| 字段/版本标记无法消歧 | `E_AMBIGUOUS_FIELD` |
| 未知 schema | `E_SCHEMA_VERSION` |
| 越界定点刻度 | `E_BAD_ATTRIBUTE_SCALE` |
| 未知属性类型/单位 | `E_UNSUPPORTED_ATTRIBUTE_KIND` / `E_UNSUPPORTED_UNIT` |
| 边端点缺失 | `E_MISSING_VERTEX` |
| 分片区间重叠/空区间 | `E_OVERLAP_SHARD` / `E_EMPTY_SHARD_KEYSPACE` |
| 线程预算越界 | `E_THREAD_BUDGET` |
| 每线程构建/遍历缓冲不足 | `E_BUFFER_BUDGET` |
| 截断/损坏检查点 | `E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_CORRUPT` |
| 检查点版本不匹配 | `E_CHECKPOINT_VERSION_MISMATCH` |

所有错误都携带结构化 `context`，例如顶点/边 ID、字段路径、分片区间、预算上限、查询 ID、检查点路径和失败阶段。

## 目录

```text
cmd/graphlab/          CLI：generate/validate/demo/faults
internal/model/        内部图模型、业务错误、分片归属、core digest
internal/generator/    固定种子生成器、三版渲染、独立参考真值
internal/loader/       三版装载、单位/刻度/层级降级、消歧与异常
internal/engine/       索引键、并行构建、遍历、缓存、操作与失效传播
internal/checkpoint/   SQLite 提交、校验、恢复
internal/app/          端到端验证、演示和异常注入报告
docs/DESIGN.md         设计细节与确定性证明
docs/VALIDATION.md     实际验证命令与结果
```

## 已验证结果

- `go test ./...`：全部通过。
- `go test -race ./...`：全部通过。
- `go vet ./...`：通过（仅第三方 SQLite C 编译警告）。
- 三版一致性、v1/v2 分片索引键、v3 降级：12/12 检查通过。
- 7 个操作逐步增量与全量查询结果一致；检查点恢复 0 查询差异；版本不匹配回退全量重建 0 查询差异。
- 8 类异常注入均返回预期稳定业务码；源工程哈希不变；失败后已提交运行时与检查点仍可用。

详见 `docs/VALIDATION.md`、`artifacts/validation/validation-report.json`、`artifacts/demo/demo-report.json` 和 `artifacts/faults-report.json`。

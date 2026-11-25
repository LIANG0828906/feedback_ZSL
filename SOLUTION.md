# 掩码质检领域组件与本地诊断应用

本交付在空 Go 工作区中实现了确定性样本生成、三版本 schema 适配、整数换算、笔刷栅格化、
alpha 合成、拓扑核验、精确失效、依赖图、纯 Go SQLite 检查点、崩溃恢复、三路径一致性比对，
以及无外部前端资源的本地 HTTP 诊断界面。

## 运行

```bash
go build ./...
go test ./...
go test -race ./...
go vet ./...

# 启动本地诊断应用（纯 Go SQLite 驱动 modernc.org/sqlite，无需 sqlite3 CLI 或数据库服务）
go run ./cmd/maskqc -addr 127.0.0.1:8080 -db maskqc-checkpoints.db
# 浏览器打开 http://127.0.0.1:8080/
```

常用接口：

- `GET /api/samples`：三批固定种子样本与真值指标。
- `GET /api/run?batch=0`：当前引擎结果、分块、图与拓扑。
- `GET /api/edit?batch=0&seq=6&kind=weight&value=900`：增量编辑与失效清单。
- `GET /api/equivalence?batch=0`：实时增量、SQLite 恢复、一次性全量三路径比对。
- `GET /api/export?batch=0&to=icon.project/v1`：版本导出、`E_LOSSY` 与读回校验。
- `GET /image?batch=0&kind=image|candidate|mask|truth`：本地生成的 PNG 视图。

## 代码结构

- `model.go`：内部工程、笔画、真值、分块、依赖图、失效报告等统一模型。
- `generator.go`：固定种子图标、近色渐变、嵌入式噪声、半透明描边、缺陷候选掩码、施工笔画和构造真值。
- `schema.go`：`icon.project/v1|v2|v3` 加载/导出、字段映射、默认值、转换痕迹和 `E_LOSSY`。
- `units.go`：序列锚点偏移坐标、权重刻度、带余数的精确反算和 `E_OVF`。
- `raster.go`：确定性线段距离场笔刷、硬度曲线、权重覆盖分量和整数 source-over alpha 合成。
- `topology.go`：4 连通域、面积/重心/外接框、孔洞、环带宽度、PCA 方向角和 `E_TOP`。
- `pipeline.go`：分块规划、缓存键、受限并行合成、依赖图和统一管线结果。
- `scheduler.go`：最大并行度、每线程像素缓冲上限、分块队列预算、排队等待记录、异常隔离和重试。
- `engine.go`：实时增量编辑、精确失效传播、持久化分块缓存复用和下游拓扑失效。
- `checkpoint.go`：SQLite 存储端口与快照身份。
- `store/store.go`：`modernc.org/sqlite` 适配器；单事务笔画分量写入、分块缓存、哈希/版本校验和整体重置。
- `diff.go`：三路径逐分块、逐像素、拓扑和提交序列比对，首个差异返回 `E_DIFF`。
- `cmd/maskqc/`：标准库 HTTP 服务和内嵌原生 HTML/JS 界面。

## Schema 语义映射

| 内部概念 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 笔画列表 | `/passes` | `/channels` | `/strokes` |
| 通道覆盖序列 | 不表达（降级 `E_LOSSY`） | `/passes`，0..255 | `/channel_coverage`，permille |
| 坐标 | `/passes/i/from,to` 为笔序号锚点偏移 `[dx,dy]` | `/channels/i/a,b` 为绝对像素 `[x,y]` | `/strokes/i/start,end` 为绝对像素对象 |
| 权重字段 | `level`，0..100 | `span`，0..255（与 v1 的半径字段位置语义互换） | `weight_permille`，0..1000 |
| 半径字段 | `span` | `level`（与 v1 的权重字段位置语义互换） | `radius_tick` |
| 硬度 | 无，升级默认 700‰并记录默认值痕迹 | 无，升级默认 700‰ | `hardness_permille` |

低版本导出始终返回可序列化产物；同时返回 `E_LOSSY` 清单，列出字段路径、旧值和处置方式。
权重缩放使用整数 floor 除法并保留 `*_rem` 余数，因此 v3 → v1/v2 → v3 后笔序号、坐标、
半径刻度和覆盖权重完全一致。余数仅作为版本化审计/反算字段，不改变对应 schema 的主刻度。

## 缓存、失效与恢复

分块缓存键由以下内容共同哈希：

- 候选掩码分块输入哈希；
- 分块坐标与笔画序号；
- 各笔画分量缓存键（含笔画标识、内容哈希、参数版本、残差摘要和算法版本）；
- 次序规则版本 `priority-then-seq-v1` 与该块的本地次序键；
- 参数版本和所有上游缓存键。

编辑坐标、权重、硬度、半径或优先级时，失效集合取旧/新笔画包围盒所覆盖分块的并集；
未进入该集合的分块键不变，直接从 SQLite `chunk_cache` 命中。与受影响分块相交的拓扑域
进入 `invalidated_topology_domains`。依赖图节点包含 `stroke:*`、`order:adjudication`、
`chunk:x:y` 和 `topology:id`。

SQLite 中每笔的所有受影响分量在一个事务内写入；连接池限制为 1，避免写入交错。重启时
`RecoverySnapshot` 校验根内容哈希、次序/参数/算法/schema 版本以及分量内容哈希；不匹配时
删除该工程的检查点、元数据和分块缓存并要求重建。测试用 `crashSink` 模拟两笔后进程终止，
重开库后只加载已提交笔画，缺失笔画续算，恢复路径与全量路径一致。

## 异常码

- `E_SCHEMA`：未知版本或必填字段缺失；错误含 `/field/path` 与 `schema_version`。
- `E_LOSSY`：降级无法表达硬度或通道覆盖序列；`details.items` 给处置方式。
- `E_OVF`：整数换算溢出；含 `field` 与两个 `operands`。
- `E_PARA`：硬度、权重、半径或缓冲尺寸越界；含 `field` 与 `legal_range`。
- `E_TOP`：连通域数量、孔洞、环带宽度、方向角、面积/重心/外接框超容差；列出域编号和实测值。
- `E_DIFF`：三路径不一致；含首个分块坐标、`field_name` 和两侧值。

## 实际验证结果

验证环境：

```text
Linux/WSL Ubuntu
go version go1.22.12 linux/amd64
modernc.org/sqlite v1.29.10（纯 Go）
```

已执行并通过：

```text
go build ./...
go vet ./...
go test -race ./...
go test -v ./...
```

详细测试结果（2026-11-26）：

```text
--- PASS: TestDeterministicGeneratorAndTopology
--- PASS: TestThreeSchemaRoundTripAndErrors
--- PASS: TestThreePathsIncrementalRecoveryAndDiff
--- PASS: TestSQLiteCrashBoundaryAndVersionInvalidation
--- PASS: TestParameterOverflowAndTopologyCodes
--- PASS: TestSchedulerIsolationAndRetry
--- PASS: TestShuffledDeclarationOrderAndDeterministicRaster
--- PASS: TestMoveEditPreciseInvalidationAndRecovery
--- PASS: TestSchedulerQueueWaitRecorded
ok  	maskqc
```

HTTP 冒烟使用独立端口和临时数据库完成：

- `/api/samples`、`/api/edit`、`/api/equivalence`、`/api/export?to=icon.project/v1`、`/image?kind=mask` 均返回成功。
- `/api/equivalence` 中 `"consistent": true`，SQLite `"valid": true`。
- v1 导出包含 `"code": "E_LOSSY"`，导出文档重新加载 `"ok": true`、`"stroke_count": 6`。

## 设计说明

真值由生成器先构造笔画覆盖和候选缺陷，再用同一套确定性核心生成像素参考与领域指标；
这保证“真值只来自生成器构造”，也使解析真值核对的是同一套声明式笔刷语义。诊断界面是
本地集成与人工排查入口；根包 `maskqc`、`maskqc/store` 可独立被其他 Go 服务集成。

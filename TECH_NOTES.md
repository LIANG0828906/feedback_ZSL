# 笔画方向与连通拓扑质检工作台 — 交付说明

## 实际环境与验证结果

- 环境：WSL Ubuntu，`go version go1.22.12 linux/amd64`。
- 模块：`maskqc`（`go mod init maskqc`），SQLite 驱动为纯 Go 的 `modernc.org/sqlite v1.29.10`。
- 固定生成种子：`424242`，生成 3 个工程：`ring-32`、`bars-48x36`、`ring-dot-64`。
- 已执行并通过：
  - `go test -count=1 ./...`
  - `go vet ./...`
  - `CGO_ENABLED=0 go build -o artifacts/maskqc ./cmd/maskqc`
  - `go run ./cmd/maskqc generate -out samples`
  - `go run ./cmd/maskqc verify -samples samples -db artifacts/maskqc.db -json artifacts/verification-report.json`
- 详细测试输出：`artifacts/go-test-v.txt`。
- 机器可读验收报告：`artifacts/verification-report.json`。

验收命令的实际结论为 3 个工程全部：

- `versions=3`：v1/v2/v3 均可装载，且 alpha 逐像素等于参考掩码。
- `threeMaskEqual=true`：实时增量、断点恢复、一次性全量重算掩码一致。
- `sequenceEqual=true`：三条路径的裁决/提交笔序一致。
- `conclusionEqual=true`：拓扑与方向报告哈希一致。
- `scheduledEqual=true`：有界并行分块结果与全量结果一致。

三条路径的 alpha 哈希示例（完整哈希见报告）：

- `ring-32`：`7a39784e7e…`
- `bars-48x36`：`d42e5e5abf…`
- `ring-dot-64`：`84037c89e5…`

## 快速运行

```bash
go test -count=1 ./...
go run ./cmd/maskqc generate -out samples
go run ./cmd/maskqc verify -samples samples \
  -db artifacts/maskqc.db \
  -json artifacts/verification-report.json
go run ./cmd/maskqc serve -addr 127.0.0.1:18099 \
  -samples samples -db artifacts/maskqc-web.db
```

浏览器打开 <http://127.0.0.1:18099>。界面为标准库 `net/http` + `//go:embed web` 的原生 HTML/CSS/JS，不依赖 CDN 或外部服务。

也可直接运行已构建的二进制：

```bash
./artifacts/maskqc verify
./artifacts/maskqc serve -addr 127.0.0.1:18099
```

## 需求与代码映射

| 需求 | 主要实现 | 验证 |
| --- | --- | --- |
| 确定性样本、图像/候选/真值 | `generator.go`、`imageio.go` | `TestGeneratedSamplesArePixelExact` |
| 三版本 schema 与字段映射 | `schema.go`、`schema_decode.go`、`schema_versions.go` | `TestThreeSchemaVersionsLoadAndVerify`、`TestSchemaErrorsContainPaths` |
| 坐标/刻度整型换算、截断残差、溢出 | `convert.go` | `TestCoordinateAndWeightIntegerRoundTrip`、`TestParameterAndOverflowStructuredErrors` |
| 单笔栅格化、参数错误、alpha 合成 | `raster.go` | 生成器逐像素测试、参数错误测试 |
| 唯一合成次序与分块依据 | `model.go` 的 `OrderedStrokes`、`raster.go` 的 `AdjudicateTile` | `TestOrderIndependentOfDeclarationOrder` |
| 连通域、重心、外接框、孔洞、环带宽 | `topology.go` 的 `AnalyzeTopology`、`VerifyTopology` | 生成器测试、`TestTopologyErrorListsMeasuredDomain` |
| 边界法向与笔画方向分类 | `topology.go` 的 `ExtractDirectionSegments`、`ClassifyNormal`、`VerifyDirections` | 三版本方向核验 |
| 依赖图与精确失效传播 | `graph.go`、`engine.go` 的 `RecomputeAffected` | `TestEditInvalidatesOnlyIntersectingTiles`、`TestPreciseRecomputePreservesUnaffectedTiles` |
| 缓存键 | `cache.go` 的 `ComponentCacheKey` | 断点恢复时重算并校验缓存键链 |
| SQLite 单事务笔画检查点 | `checkpoint.go` | `TestIncrementalResumeFullAreIdentical` |
| 有界并行、缓冲/队列预算、异常隔离 | `scheduler.go`、`engine.go` 的 `RunScheduled` | `TestSchedulerIsolatesFailureAndRecordsWait`、`TestHTTPWorkbenchEndpoints` |
| 本地诊断界面 | `server.go`、`web/` | `TestHTTPWorkbenchEndpoints` |
| 一键验收报告 | `acceptance.go`、`cmd/maskqc/main.go` | `artifacts/verification-report.json` |

## 三版本编码

- v1：`qc.strokes.v1`；`from_px/to_px` 绝对坐标；`coverage_permille` 千分比；`hardness_percent` 百分比；方向为数字码 `0..4`。
- v2：`qc.strokes.v2`；`start_off/end_off` 为相对 `anchor(seq)=(3*seq,2*seq)` 的序号偏移；`coverage_bp` 万分比；方向为 `FWD/REV/LFT/RGT/UNK`。
- v3：`qc.project.v3`；嵌套 `geometry.start/end` 绝对像素；`coverage={basis:10000,value}`、`hardness={basis:1000,value}`；方向使用内部语义字符串。

未知 schema 返回 `E_SCHEMA`，例如 `$.schema: unknown schema version "qc.nope.v9"`；缺失字段返回带 JSON 路径的 `E_SCHEMA`，例如 `$.icon_id: missing required field`。

## 整型换算与错误

所有坐标加减、锚点推导、千分比/万分比换算均使用 `int64` 检查运算，不使用浮点累积。`ConversionReport.Entries` 记录：

- 字段与 JSON 位置；
- 输入、输出、余数；
- 舍入方向（`floor`/`ceil`/`nearest`）；
- 是否发生截断；
- 参与运算的操作数。

溢出返回 `E_OVF`，其 `field` 指明字段，`operands` 给出两个导致溢出的操作数。硬度合法区间为 `[1,1000]`（千分比）；权重万分比允许集合为 `{1000,2500,5000,7500,10000}`，错误为 `E_PARA` 并带 `legalRange` 与 `details.allowed`。

## 样本与逐像素真值

生成器用固定种子产生背景渐变、前景色块、嵌入式噪声和半透明边界描边。每个工程写出：

- `image.png`：诊断原图；
- `candidate.png`：同时包含内缩、额外空洞和外溢块的候选掩码；
- `candidate-inset.png`、`candidate-holes.png`、`candidate-spill.png`、`candidate-defective.png`：分项缺陷样本；
- `reference.png`：唯一精度基准；
- `project-v1.json`、`project-v2.json`、`project-v3.json`。

因为普通 `source-over` 只能增加 alpha、不能移除候选外溢，生成器为每个确定性外溢块配对一条同几何、全权重、全硬度的 `erase` 修正笔（`destination-out` 语义）。这使候选掩码可以真实包含外溢，同时完整施工记录合成后仍逐像素回到真值。普通绘制笔均为 `over`。

## 合成次序与分块

唯一次序由 `OrderVersion = "priority-asc+seq-asc-v1"` 定义：

1. `priority` 升序，数值小的工序先执行；
2. 同优先级按 `seq` 升序。

`AdjudicateTile` 对每个分块输出 `orderKey`、参与笔 ID、序号、优先级和包围盒相交依据。排序只作用于显式切片，不遍历 Go map 来决定笔序。

缓存键由以下内容拼接后 SHA-256：

- `tileInputHash`：候选掩码分块输入哈希；
- `strokeID`；
- `OrderVersion`；
- `ParamVersion`；
- `upstreamKey`：该分块前一笔的缓存键链；
- 算法与缓存格式版本。

依赖图包含 `stroke:*`、`component:*`、`order:*`、`tile:*`、`topology`、`direction`、`report` 节点。编辑坐标、增删笔画时，`PlanEdit` 计算旧/新包围盒覆盖分块的并集；`RecomputeAffected` 保持未受影响分块像素不变，只重建失效分块。

## SQLite 检查点

数据库包含：

- `stroke_commits(project_id, seq, stroke_id, order_index, residual_digest, order_version, param_version, algorithm_version, committed_at)`；
- `tile_components(project_id, commit_seq, tile_id, cache_key, content_hash, width, height, op, alpha)`；
- `meta(key,value)`。

`CommitStroke` 使用互斥锁、`SetMaxOpenConns(1)` 和单个 SQLite 事务写入该笔在所有受影响分块上的分量、缓存键、内容哈希、换算残差摘要和版本信息，因此写入串行且不会出现跨笔交错。恢复时按 `project_id + seq` 读取，校验笔序、分量哈希和缓存键链，然后只提交断点后的笔。

## 调度预算与隔离

`Scheduler` 支持：

- `MaxParallel`：最大 worker 数；
- `MaxBufferPixels`：每 worker 像素缓冲上限，超过上限的任务返回 `E_BUDGET`；
- `QueueBudget`：待处理任务 channel 容量；
- 队列满时阻塞入队，并记录 `queue_budget_exhausted`、job/tile 与等待毫秒；
- `panic` 恢复为失败 `JobResult`，其他分块继续执行，失败分块不会拼装进结果，也不会写入新的检查点。

## Web API

- `GET /api/state`：工程列表与最近核验状态。
- `GET /api/project?id=...`：笔画、真值、分块和依赖图。
- `POST /api/verify`：`mode=incremental|resume|full|scheduled`，可传 `failTileId`/`panicTileId`。
- `POST /api/edit`：`type=update|add|delete`，返回失效计划和精确重算结果。
- `POST /api/reset`：从磁盘恢复样本。
- `GET /api/png?project=...&kind=image|candidate|reference|accumulated`。
- `GET /api/component?project=...&stroke=...&tile=...`。
- `GET /api/checkpoints?project=...`。

界面展示笔画序列、分块覆盖分量、积算掩码、法向箭头、拓扑表、方向判定表、逐分块裁决、失效分块高亮、调度等待和 SQLite 提交序列。

## 说明与边界

- 连通域采用前景 4-连通；背景孔洞采用 4-连通泛洪。样本笔画宽度足够，结论不受对角连通歧义影响。
- 环带宽度由到背景的 Manhattan 距离变换估算为 `2*maxDistance+1`；真值由同一生成构造流程产生，容差字段随 JSON 持久化。
- 当前 Web 为本地单用户诊断台，默认监听 `127.0.0.1`，没有设计公网鉴权。
- 未使用 Docker 或外部网络服务；`go mod` 依赖已缓存在本机模块缓存。

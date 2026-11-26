# 曲面轮廓采样内核

基于固定种子解析样本的 Go 领域组件与本地诊断应用。同一曲面场景可装载为
schema v1/v2/v3，统一为内部定点模型；轮廓曲线段使用确定性自适应采样，经
预算降级后装配到分块，并由 `database/sql` + `modernc.org/sqlite` 持久化。

当前本地核验结果：`PASS`（三版本装载、高→低→高往返、预算降级、精确失效、
SQLite 原子检查点、三路径逐顶点一致、`go test -race`）。

## 快速开始

```bash
go mod download
go test ./...
go test -race ./...
go build ./...

# 一键验收，摘要打印到终端，完整 JSON 写入 artifacts/verification.json
go run ./cmd/verify -out artifacts/verification.json

# 本地诊断页（不访问外网）
go run ./cmd/silhouette -addr 127.0.0.1:8080
# 打开 http://127.0.0.1:8080
```

页面支持 schema 选择、容差/预算调整、采样与分块、三路径比对，以及编辑曲线控制点
后的失效/复用清单。服务仅使用 `net/http` 与 `encoding/json`。

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `model.go`, `errors.go` | 统一内部模型、错误码、转换与降级痕迹 |
| `fixed.go` | 绝对/相对、逻辑/像素、容差的整型换算 |
| `generator.go` | 固定种子解析样本与真值清单 |
| `schema.go` | v1/v2/v3 加载、默认补齐、导出、E_LOSSY |
| `curve.go`, `sampling.go` | 有理数 de Casteljau、自适应细分、预算降级 |
| `tiles.go` | 边界唯一归属、覆盖率、内容哈希、缓存键 |
| `deps.go` | 依赖图、编辑失效计划、持久化复用 |
| `checkpoint.go` | SQLite 单事务分块检查点与提交日志 |
| `scheduler.go` | 有界并行、每线程顶点缓冲、排队等待记录 |
| `pipeline.go` | 实时/恢复/全量重算三条路径与自动首差比对 |
| `httpapp.go` | 本地 JSON API 与单页诊断台 |
| `verification.go` | 可复用验收流程 |

## Schema 约定

内部坐标、容差和几何统计统一为微逻辑单位；声明 `1 logical = 1 pixel`，因此
内部单位同时可视作微像素。`FixedScale = 1_000_000`。

| | v1 legacy | v2 relative | v3 current |
| --- | --- | --- | --- |
| 版本标识 | `format: surface-schema/1` | `schema_version: 2` | `schema_version: 3` |
| 曲面字段 | `mesh.control_points` | `surface_grid.control_deltas` | `surface.controls` |
| 控制点表达 | 绝对逻辑坐标 | 相对 `base_point` 的微逻辑增量 | 绝对微像素 |
| 容差字段/单位 | `tolerance_micro_logical` | `tolerance_milli_px` | `sampling.tolerance_micro_px` |
| 轮廓划分 | 无显式字段，每 4 个三次控制点成段 | 同 v1，扁平控制点数组 | `segments[]` 显式 ID/权重/引用 |

装载结果为 `Scene`，保留 `SourceVersion` 和逐字段 `Traces`。未知版本、缺失必填
字段返回 `E_SCHEMA`，例如：

```text
E_SCHEMA at $.surface.controls: required field is missing
```

低版本装载补齐的默认字段记录 `path`、`old_value="<missing>"`、`new_value`、
`default_source`。高版本导出低版本时，不可表达的曲线段 ID/权重/引用与相对基点
表达会进入 `E_LOSSY` 清单；导出结果仍可被对应低版本加载器读回。由生成器构造的
场景均可在 v3 → v1/v2 → v3 后精确复原曲面控制点、曲线控制点、分块、预算和容差。

### 定点换算

```text
v1 coordinate fixed = logical * 1_000_000
v2 coordinate fixed = base + micro_logical_delta
v3 coordinate fixed = micro_pixel

v1 tolerance fixed = micro_logical
v2 tolerance fixed = milli_pixel * 1_000
v3 tolerance fixed = micro_pixel
```

所有乘加都检查 `int64` 溢出；溢出返回 `E_OVF`，含字段路径与两个操作数。不可整除
换算记录商、余数和是否截断；负坐标可指定 toward-zero/floor/ceil/nearest。采样几何
使用 `math/big.Rat`，没有浮点累积，只在输出顶点时做一次最近整数取舍。

## 确定性与预算

- 每个三次曲线段独立细分；停止条件基于控制点到弦的精确有理距离，最大深度 12。
- 顶点按参数递增输出，段结果按 `segment_id` 排序；并行完成顺序不影响顶点、哈希或提交。
- 容差收紧时顶点数单调不减，实测最大弦高单调不增。
- 容差小于精度下限，或最大深度仍达不到精度，返回 `E_TOL`，带段 ID、实测值和下限。
- 超出采样预算时，按 `curve turn * visual weight` 从低到高逐段放宽容差；每条降级
  记录依据、优先级、前后容差、前后顶点数、前后最大弦高、预算占用。
- 所有段降到两个端点仍超预算时返回 `E_BUDGET`，带超支顶点数与优先级末位段 ID。

默认样本：基础容差 `50_000 µ`，无约束采样 321 顶点；预算 90 时确定性降级到
85 顶点，并产生 12 条可解释降级记录。

## 分块、哈希与缓存键

- 分块边界归属规则为 `min-tile-id-half-open`：优先最低顺序的半开区间分块，只有
  场景外边界最大值才回退闭区间。一个采样顶点只属于一个分块。
- 每分块记录各曲线段覆盖顶点数、覆盖率分数、实测最大弦高、依赖集合。
- `ProjectionDigest` 由分块边界与各段装配顶点决定。
- `ContentHash` 由投影摘要与曲线元数据版本共同决定，不含源 schema 版本、线程数、
  文件路径或完成顺序。

```text
CacheKey = SHA256(
  tile input content hash,
  sorted segment ids,
  tolerance parameter version,
  budget parameter version,
  upstream segment cache key
)
```

## 依赖图与精确失效

依赖链：

```text
control → curve segment → sampled polyline → tile assembly
             ↑
      tolerance / budget parameters
```

编辑曲线控制点后重新采样受影响段，并与旧装配比较顶点覆盖；只在受影响段顶点进入
旧/新分块时失效。未受影响分块直接加载 SQLite 持久化结果并列入 `reused_tiles`，
失效分块列入 `invalidated_tiles`。容差或预算参数变化会使全部下游失效。

## SQLite、并行与三路径

- `tile_checkpoints` 保存顶点摘要、最大弦高、覆盖率、依赖集合、内容哈希/缓存键、
  参数版本、算法版本、完整分块 JSON 与提交序号。
- 每个分块在单一事务中更新检查点并插入 `tile_commits`；Store 单写者互斥，流水线
  按分块声明顺序提交，提交序号稳定递增。
- 工作线程池限制最大并行度和每线程顶点缓冲。无空闲 worker 时提交方阻塞排队，
  `WorkerEvent.wait_nanos` 记录等待时长，`reason` 记录并行度/缓冲耗尽原因。
- 实时路径：新库逐分块提交。
- 恢复路径：模拟前 2 个分块后中断，恢复时校验内容哈希/缓存键/参数/算法版本并复用。
- 全量重算：清空场景检查点后重新采样提交。
- `CompareRuns` 比较分块顶点、弦高、覆盖率、内容/缓存哈希、降级统计和逻辑提交序列；
  首个差异报告 `first_mismatch_tile` 与 `first_mismatch_vertex`。

## 本地 API

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/` | 单页诊断台 |
| `GET` | `/api/health` | 健康检查 |
| `GET` | `/api/scene?seed=&version=` | 生成 schema 与解析真值 |
| `POST` | `/api/run` | 装载、采样、预算降级、装配、持久化 |
| `POST` | `/api/compare` | 实时/恢复/全量重算逐顶点比对 |
| `POST` | `/api/invalidation` | 控制点编辑后的失效与复用 |

示例：

```bash
curl -s -X POST http://127.0.0.1:8080/api/run \
  -H 'content-type: application/json' \
  -d '{"seed":20261127,"version":3,"tolerance":50000,"budget":90,"workers":3,"vertices_per_worker":8192}'
```

## 实际验证结果

```text
$ go test ./...
ok  	example.com/silhouette

$ go test -race ./...
ok  	example.com/silhouette

$ go run ./cmd/verify -out artifacts/verification.json
verification PASS seed=20261127 algorithm=silhouette-sample/1.0.0
  schema-v1: canonical=true truth=true traces=104 defaults=12
  schema-v2: canonical=true truth=true traces=94 defaults=12
  schema-v3: canonical=true truth=true traces=77 defaults=0
  roundtrip-v1: controls=true tolerance=true tiles=true curves=true E_LOSSY=5
  roundtrip-v2: controls=true tolerance=true tiles=true curves=true E_LOSSY=4
  budget: 321 -> 85 <= 90, degradation-records=12, low-budget=E_BUDGET
  invalidation: invalidated=[tile-0 tile-1] reused=[tile-2 tile-3] commits=2/6
  paths: resume=true full=true sqlite=4/4
  workers: events=4 blocked=4 max-wait-ns=7094
  errors: schema=E_SCHEMA missing=E_SCHEMA($.surface) overflow=E_OVF tol=E_TOL
```

完整机器可读报告位于 `artifacts/verification.json`。测试不访问网络、外部数据库或
容器运行时；SQLite 使用纯 Go 驱动，数据库文件仅在本地临时目录或内存中创建。

# TerrainBake

开放世界远景地形的分块着色网格烘焙内核。项目只使用 Go 标准库，包含：

- 可集成的 Go 领域包。
- 三套 schema 的统一加载、编码与显式升降级。
- 确定性自适应剖分、共享边界对齐和断裂归因。
- 精确失效传播、并行预算、原子文件检查点。
- 草稿增量 / 断点恢复 / 全量重算三路径逐项比对。
- `net/http` 本地诊断页面和故障注入。

所有输入、样本和真值均由固定种子生成器产出，不依赖外部服务或数据集。

## 快速运行

环境：Linux/WSL Ubuntu，Go 1.22+。

```bash
# 全量测试
go test ./...

# 竞态检查
go test -race ./...

# 重新生成三版样本并执行三路径验证
go run ./cmd/terrain-demo -verify-only -samples samples

# 启动诊断页面
go run ./cmd/terrain-demo \
  -addr 127.0.0.1:8080 \
  -cache .terrain-cache \
  -samples samples
```

浏览器打开 `http://127.0.0.1:8080`。页面提供逻辑时刻相机播放、着色预览、线框、
边界折角/归因、失效传播、预算排队、编辑增量、三路径验证和四类故障注入。

## 目录

| 文件 | 内容 |
|---|---|
| `generator.go` | 固定种子、解析高程、分块/接缝/相机/真值生成 |
| `schema_types.go` | 三套互不相同的原始 JSON 形态 |
| `schema.go` | 统一加载、字段路径校验、转换痕迹 |
| `schema_encode.go` | 三版确定性编码与 `ConvertSchemaVersion` |
| `conversions.go` | 坐标、容差刻度、覆盖边界的整型换算 |
| `mesh.go` | 自适应四叉树、共形扇三角化、逐三角形误差 |
| `seam.go` | 边界序列对齐、折角、欠采样/错位归因、真值核验 |
| `engine.go` | 失效传播、缓存键、并行调度、预算排队、路径比对 |
| `cache.go` | 单写者原子检查点、记录哈希与版本校验 |
| `verification.go` | 草稿增量、断点恢复、全量重算 |
| `demo.go` / `server.go` | 页面状态、编辑、验证、故障注入、HTTP API |
| `web/index.html` | 无外部资源依赖的诊断页面 |
| `samples/` | 同一地形内容生成的 v1/v2/v3 JSON 与 manifest |

## 内部约定

- 世界坐标：`int64`，单位是规则网格单位。
- 高程：`int64`，单位是毫米。
- 弦高容差：`int64`，内部统一为微米。
- 覆盖范围：内部统一为半开区间 `[min,max)`。
- 三角形预算与顶点预算：每块硬上限。
- 接缝采样间距：声明为 2 的幂，且整除块尺寸；当前确定性样本为 8。

`Generator.BaseHeightMM` 是共享边界的唯一高程来源；块内编辑使用在边界处衰减为零的
确定性 bump，因此编辑不会破坏已声明的共享边界真值。

## 三版 schema

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 版本字段 | `schema_version: 1` | `version: "terrain/v2"` | `schema: "terrain/v3"` |
| 块字段 | `chunks` | `chunks` | `blocks` |
| 覆盖表达 | 绝对 `bounds`，闭区间 `[]` | `origin + extent`，半开 `[)` | 世界相对的 `bounds_rel`，开闭 `(]` |
| 邻接 | 块内 `neighbors` | 省略，按覆盖自动推断 | 块内 `adjacent` |
| 容差 | 千分比 `chord_tol_permille` | 万分比 `chord_tolerance_bp` | 千分比 `chord_tol_permille` |
| 舍入 | `half_up` | `floor` | `ceil` |
| 真值坐标 | 绝对 `x/y` | 块相对 `u/v` | 块相对 `du/dv` |
| 接缝约束 | 块内 `seams` | 顶层 `seam_constraints` | 块内 `seams` |

v1 的闭区间在内部规范为 `[min,max+1)`，残差为 1；v3 的 `(min,max]` 规范为
`[min+1,max+1)`，逐处残差为 1。所有调整都写入 `conversion_traces`。

`LoadProject` 对未知版本或缺失必填字段返回 `E_SCHEMA` 并给出字段路径；JSON 语法或
类型损坏返回 `E_DECODE`；边界规范化后的块重叠返回 `E_OVER`；悬空邻接引用返回
`E_ADJ`。

显式升降级：

```go
targetBytes, normalized, err := terrainbake.ConvertSchemaVersion(sourceBytes, terrainbake.SchemaV1)
```

返回的 `normalized` 保留源版本与逐字段转换痕迹。回归测试覆盖
`v3 -> v1 -> v3` 后世界矩形、块矩形和真值采样坐标完全一致。

## 网格剖分

1. 以块覆盖为根节点构建四叉树。
2. 邻近块边界单元强制细到声明的接缝采样间距。
3. 内部单元在采样点弦高误差超过容差时继续细分，直到容差满足、达到最小单元或预算上限。
4. 按稳定排序遍历叶单元，收集共享边上的悬挂点，构造矩形多边形。
5. 每个多边形从稳定首顶点做共形扇三角化；相邻叶单元共享的边包含相同点集。
6. 逐三角形以质心和边中点估计真实高度与平面插值的差，输出 `height_error_um`。

输出不依赖工作线程数、运行路径或并发完成顺序，顶点 ID 和三角形顺序由稳定叶排序
决定。测试用 12 个并发构建、1/4 工作线程配置分别比对逐项结果。

`BuildMesh` 对非法预算、容差、接缝间距返回 `E_PARA`，携带字段名和合法区间。

## 接缝与归因

两侧分别提取共享边上的 `(位置, 高度)` 序列并逐点比较：

- 位置数量、顺序或高度不一致：`alignment_mismatch`。
- 完全一致但折角超限，且任一侧边界三角形高度误差超过容差：`undersampling`。
- 完全一致、折角超限，但两侧都没有超差：返回 `E_ATTR`，携带边界 ID 与两侧顶点数。
- 未超限且无边界误差：`ok`。

生成器真值包含采样点高度、每块三角形上限、共享边对齐序列，以及
`undersampling` / `alignment_mismatch` 断裂归因样本。测试会按这些真值构造诊断并
校验归因结果。

折角诊断对高度使用 1000 倍垂直夸张，便于页面观察；位置和高度本身仍使用整数真值。

## 失效与缓存键

每个块的直接邻居由共享边界双向推导。块高程、预算或容差发生编辑时：

1. 标记编辑块自身失效。
2. 标记所有直接邻接块失效，原因是 `seam_realignment`。
3. 其余块继续复用检查点结果。

缓存键：

```text
baseKey(block) =
  H(contentHash, adjacencyVersion, parameterVersion, algorithmVersion)

cacheKey(block) =
  H(baseKey(block), sorted(baseKey(directNeighbor)...))
```

使用直接邻居的 base key，而不是递归组合后的 key，避免一次编辑沿完整邻接图形成
传递性级联失效。直接邻居内容或参数变化仍会改变当前块键；非邻接块保持稳定。

编辑后、从空缓存全量构建后，顶点、三角形、边界统计、归因和规范提交序列逐项相等。

## 并行预算与检查点

`EngineConfig` 控制：

- `MaxParallelism`：工作线程数。
- `MaxVerticesPerThread`：单块输出顶点硬上限。
- `MeshMemoryBudget`：全局网格内存预算。

每个作业按预算申请内存额度。额度不足时阻塞轮询并记录
`QueueEvent{reason, requested_bytes, wait_ms, acquired}`，不会静默丢块。单块超过
全局预算、超过线程顶点上限或注入失败时，仅该块返回错误。

检查点 `checkpoint.json` 由单一写者串行提交。每次写入：

1. 写临时文件。
2. `fsync` 文件。
3. 原子 `rename` 覆盖正式文件。
4. `fsync` 目录。

每个块记录包含：

- 缓存键、内容哈希、邻接版本、参数版本、算法版本。
- 依赖集合与直接上游键。
- 顶点摘要、三角形摘要、折角统计摘要、归因摘要、整记录摘要。
- 完整网格、相关接缝报告与归因标签。
- 单调提交序号。

重启时逐块校验摘要和版本。任一哈希或版本不匹配会删除整块记录并重算，不使用部分
字段。测试覆盖顶点摘要篡改与算法版本篡改，均只重算目标块。

## 三路径验证

`VerifyThreePaths` 对同一高程场和编辑序列运行：

1. 草稿增量：逐次编辑后增量烘焙。
2. 断点恢复：每次编辑前从检查点重新打开引擎。
3. 全量重算：应用全部编辑后从空缓存一次性构建。

比对字段包括规范块顺序、顶点集合/顺序、三角形集合/顺序、边界折角统计、归因和提交
序列。首个差异返回 `E_DIFF`，包含块 ID、字段、左右值。

实际运行结果：

```text
go test ./...                          PASS (26 tests)
go test -race ./...                    PASS
three-path fingerprint  all paths      f64200dcb308e4a476d4a36c61cc7d326e878c884e915239bf2f9b711397dc03
draft incremental                      reused=3 written=3
checkpoint resume                      reused=3 written=3
full rebuild                           reused=0 written=6
commit sequence                        B00 B01 B02 B03 B04 B05
```

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/` | 诊断页面 |
| `GET` | `/api/state` | 分块、网格、接缝、预算、失效、验证状态 |
| `POST` | `/api/edit` | `{"block_id":"B02","height_delta_mm":120}` 等 |
| `POST` | `/api/verify` | 运行三路径逐项比对 |
| `POST` | `/api/fault` | 注入 `decode/overlap/adjacency/block` |
| `GET` | `/api/frame?t_ms=1500` | 最近逻辑时刻、相机与可见块 |
| `GET` | `/api/schema?v=1` | 导出指定 schema 版本 |

故障注入结果：

- 损坏高程 JSON：`E_DECODE`。
- 修改块覆盖造成重叠：`E_OVER`。
- 注入不存在邻接：`E_ADJ`。
- 单块内部失败：`E_BLOCK`，其他已提交块和网格仍可用。

## 当前边界

- 自适应四叉树要求块覆盖为正方形、边长为 2 的幂；接缝间距也必须整除块尺寸。
- 世界/块坐标和高度使用有符号 `int64`；极端溢出会返回 `E_OVF`。
- “规范提交序列”包含最终可用的全部块；`written_blocks` 单独记录本次真正写检查点的块。
- 页面和 API 仅面向本地单进程诊断，未提供鉴权或多租户隔离。

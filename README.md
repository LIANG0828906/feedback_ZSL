# 地形切线空间法线烘焙与接缝诊断

这是一个完全本地的 Go/Wails 技术验证工程。它使用固定种子生成地形块、量化材质权重、共享边约束和解析真值，在受限线程/像素缓冲/内存预算下并行烘焙切线空间法线，支持接缝连续处理、失效传播、SQLite 版本化检查点、断点恢复和可视化诊断。

不依赖外部服务、账号、网络素材、GPU 或容器。

## 快速验证

要求 Go 1.22+。核心验证使用纯 Go SQLite 驱动；`google/uuid` 仅保留 modernc SQLite/Wails 所需 API 的本地最小实现，避免 `net` 触发宿主 CGO 工具链。

```bash
go test ./... -count=1
go run ./cmd/terrainctl generate -out samples/terrain_project.json
go run ./cmd/terrainctl validate -project samples/terrain_project.json
go run ./cmd/terrainctl verify \
  -project samples/terrain_project.json \
  -dir artifacts/verify \
  -out artifacts/verification.json
```

也可直接使用 `make test`、`make generate`、`make verify`、`make bake`。

最近一次实际验证结果：

```text
[PASS] analytic truth: 24 pixels, max normal error 0.000583, max slope error 0.030820 deg, max seam angle error 0.218231 deg
[PASS] seam attribution distinguishes geometry and bake artifacts: difference_operator=12, geometric_fold=2, material_mix=4
[PASS] full bake: 6 blocks committed, 2 queue events
[PASS] incremental rebake scope: affected counts 1,5,4,6; isolated blocks remained cache hits
[PASS] incremental equals full: normals, slopes, bounding boxes and seam reports match exactly
[PASS] stable snapshot submission order: full snapshot=[1 2 3 4 5 6] incremental snapshot=[1 2 3 4 5 6] sqlite=[1 2 3 4 5 6]
[PASS] checkpoint resume equals full: rebaked=0 cacheHits=6 complete=true
[PASS] partial checkpoint recovery equals full: restored 2 committed blocks, rebaked 4 missing blocks
[PASS] parallel equals serial: pixel data, seam statistics and per-revision commit order are identical
[PASS] failed block does not pollute committed snapshot: error=E_BLOCK block=2 field=bake: injected block failure unchanged=true
[PASS] retry failed block: rebaked=4 cacheHits=2
[PASS] three-path commit order: map[crashResume:[1 2 3 4 5 6] full:[1 2 3 4 5 6] incremental:[1 2 3 4 5 6] resume:[1 2 3 4 5 6] serial:[1 2 3 4 5 6]]
```

完整结果保存在 `artifacts/verification.json`，环境与命令记录在 `artifacts/VALIDATION.md`，法线/坡度预览在 `artifacts/verify/preview/` 和 `artifacts/verify/full-export/`。

## 目录

```text
cmd/terrainctl/       本地生成、校验、烘焙和验收命令
internal/model/       数据契约、向量、哈希、错误码
internal/sample/      固定种子样本与解析真值生成器
internal/projectio/   工程 JSON 读写与结构校验
internal/bake/        单块差分、材质混合、法线/坡度/颜色/包围盒
internal/seam/        接缝连续性、违反坐标与突变归因
internal/dependency/  双向依赖、材质影响范围、SCC 拓扑排序
internal/engine/      预算调度、增量失效、重试、快照比对入口
internal/checkpoint/  纯 Go SQLite 原子检查点与完整性校验
internal/compare/     逐像素/逐接缝首个差异定位（E_DIFF）
internal/truthcheck/  解析真值验收
internal/verify/      全量、增量、恢复、串并行、失败隔离验收
third_party/googleuuid/ 无 net/CGO 依赖的最小 UUID 兼容实现
app.go                Wails 绑定 API
main_wails.go         Wails 桌面入口（-tags wailsui）
frontend/dist/        无 npm 构建依赖的静态诊断前端
```

## 样本

`samples/terrain_project.json` 由固定种子 `20261125` 生成：

- 6 个高程块，分辨率 `20×18`、`18×18`、`20×16`、`18×16`、`12×10`、`10×8`。
- 块 1–4 构成 2×2 接缝连通主区；块 5、6 是独立区域，用于验证缓存复用。
- 4 条共享边约束，覆盖垂直/水平接缝、真实陡坡、材质权重差异和差分边界差异。
- 材质层 `rock`、`sand`、`grass`，每像素权重为量化整数且和严格等于 `1000`。
- 每块 4 个解析真值像素，共 24 个；真值包含单位法线、坡度角和容差；每条接缝另存解析相邻法线夹角与角度容差。
- 样本包含 2 个几何折角、4 个材质混合伪突变、12 个差分伪突变归因样本。

## 关键行为

**结构校验**

`E_SCHEMA` 表示字段缺失、重复 ID、接缝引用不存在、版本不合法等；`E_SIZE` 表示高程/权重/共享边采样长度不符；`E_WEIGHT` 表示量化权重越界、非整数或全零行；`E_PARA` 表示差分算子、缩放、混合模式或接缝松弛参数越界。错误 JSON 均包含 `blockId` 与 `field`。

**确定性烘焙**

单块烘焙只依赖高程、权重、材质参数和烘焙参数。中央差分、前向差分和 Sobel 均使用固定循环顺序与 `float64`；预览/精确路径复用同一核心。线程号、完成顺序和墙钟时间不进入任何像素结果。32 路并发测试对法线位模式、坡度位模式、颜色和包围盒做严格相等比较。

**接缝连续处理**

每条接缝按 ID 稳定排序，对两侧边界像素交替向对方法线松弛。原始夹角超过阈值时记录两侧局部坐标、法线、夹角、归因类别与依据：

- `geometric_fold`：接缝两侧高程跳变，或表面坡度超过该接缝声明阈值。
- `material_mix`：高程一致且坡度正常，但两侧量化材质权重差显著。
- `difference_operator`：高程和权重一致或接近，剩余差异来自独立边界采样/差分。

松弛后仍超过阈值则返回 `E_SEAM`，携带接缝 ID、一对块编号、坐标和实测角度。

**失效传播**

接缝在依赖图中是双向边。块高程编辑从该块沿接缝闭包传播；材质层参数编辑从所有使用该层的块出发，再沿接缝传播；差分算子/缩放编辑使全部块失效。依赖图使用 Tarjan SCC + 确定性拓扑排序，循环依赖块按块号排序。未失效块直接读取检查点，烘焙次数保持为零。

**预算与提交**

`MaxParallelism` 限制 worker 数；`PerThreadPixelBuffer` 限制每线程二维瓦片像素数；`NormalMapMemoryBytes` 限制并发在途结果。无法立即获得预算的任务阻塞并通过 `QueueRecord` 记录 `max_parallelism`、`normal_map_memory_budget` 或 `per_thread_buffer_requires_tiling`。单块自身超过总内存预算时返回 `E_BUDGET`，不会静默丢块。

SQLite 写入由单锁和短事务串行化。每个版本批次按稳定拓扑序提交全部块快照，但只重算失效块；因此全量、实时增量、恢复和串行路径的“每版本每块最后提交顺序”一致。

**版本化检查点**

每个块检查点包含：

- 输入内容哈希、法线摘要、坡度摘要
- 接缝统计、依赖集合
- 烘焙参数版本、算法版本、工程 revision
- 最终/原始法线、最终/原始坡度、包围盒

提交前先发布 `complete=false`，全部块提交后原子写入 `complete=true` 与快照拓扑序。启动时逐块校验版本、内容哈希、法线/坡度摘要和 blob 维度；任一不匹配会整体清空快照并全量重算，`RunReport.SnapshotInvalidated=true`。

**失败隔离**

新结果先进入 pending 区。单块失败、`E_SEAM` 或 SQLite 提交失败时不会替换已提交缓存。`RetryFailed()` 从失败块、缓存缺失块和内容哈希不一致块出发，只重试依赖闭包；独立块继续命中缓存。

## Wails 诊断界面

界面使用静态 `frontend/dist`，提供：

- 分块切线空间法线 RGB 预览与坡度热力图
- 接缝违反坐标、阈值、原始/最终最大夹角与归因类别
- 失效传播列表、缓存命中/重烘块与耗时
- 预算占用条、排队块和排队原因
- 高程 delta/set、材质 bias/strength、差分算子、缩放编辑
- 失败块重试、全量重建、法线/坡度 PNG 导出

构建：

```bash
wails doctor
CGO_ENABLED=0 wails build -platform windows/amd64 -nosyncgomod -s -skipbindings -nopackage
```

Windows 目标也可直接执行 `wails build -nosyncgomod -s -skipbindings`，入口文件会自动启用。`-s` 跳过 npm 构建，因为前端是不需要打包的静态资源。工程固定 Wails Go 库 `v2.9.2` 以兼容 Go 1.22；若本机 Go 已为 1.23+，也可升级到与 CLI 一致的库版本。

当前执行环境是 WSL Linux，`wails doctor` 显示 Wails `v2.10.2`、Go `1.22.12`，但无 Node/npm、无 WebKitGTK 开发包，且工具链缺少可执行 gcc。Windows 交叉构建已实际成功：`build/bin/terrain-normal-diagnostics.exe`，PE32+ GUI，13,301,248 bytes，SHA-256 `a3afc75cb832a3fc73be3065cd1b2e3b2a57cfcf3b13e7f18359ec92f0d41b1e`。默认 Linux 目标仍会因宿主 gcc/WebKitGTK 缺失停在 `runtime/cgo`；这是宿主桌面工具链限制，不是应用代码错误。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_SCHEMA` | 字段缺失、版本/引用/枚举不合法 |
| `E_SIZE` | 高程、权重或共享边采样尺寸不符 |
| `E_WEIGHT` | 量化权重越界、非整数或全零行 |
| `E_PARA` | 差分算子、缩放、混合或松弛参数越界 |
| `E_SEAM` | 迭代上限内接缝夹角仍超标 |
| `E_DIFF` | 增量/全量或恢复快照首个像素/接缝不一致 |
| `E_BUDGET` | 单块结果或像素缓冲超出硬预算 |
| `E_BLOCK` | 单块内部异常或注入故障 |
| `E_SNAPSHOT` | 检查点版本/哈希/完整性失效 |
| `E_IO` | 本地文件或 SQLite I/O 失败 |

更详细的数据流、SQLite 表结构和一致性语义见 `docs/TECHNICAL.md`。

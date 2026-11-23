# terrainbake — 地形法线贴图烘焙调度器

纯 Go、仅标准库、无 GPU、无外部服务的地形法线贴图烘焙与验证工具。项目从固定种子现场生成地形高程、量化材质权重、块间接缝约束与解析真值，完成：

- 高程场 → tangent-space 法线 PNG / 16-bit 坡度 PNG；
- 材质分层法线混合、量化权重、可配置差分算子与缩放；
- 双侧共享边接缝约束、逐块违反计数与最大夹角；
- 双向接缝依赖 + 材质依赖的精确失效传播；
- 受限工作线程 / 线程像素缓冲 / 法线图内存预算的并行调度；
- 原子持久化检查点、暂停恢复、版本与全局内容哈希整体失效；
- 实时增量、断点恢复、空缓存全量三条路径逐像素与元数据一致性验证；
- 离线 HTML 诊断页：着色块预览、法线图集、接缝热力图、失效传播图、差异热力图。

## 快速验证

环境要求 Go 1.22+。Windows `cmd` 或 PowerShell：

```bat
go version
go build -o terrainbake.exe ./cmd/terrainbake
go test ./...
go test -race ./...
go run ./cmd/terrainbake demo --out demo-out --workers 3
```

Linux/macOS 相同，只是可执行文件去掉 `.exe`。

`demo` 会生成：

```text
demo-out/
  project.json               固定种子基线工程
  project-edited.json        应用编辑序列后的工程
  verification.json          三路径一致性报告
  baseline-store/            基线检查点
  edited-store/              编辑后全量检查点
  three-path/{live,resume,full}/
  report/
    diagnostics.html
    normals-atlas.png
    blocks-colored.png
    seam-heatmap.png
    diff-heatmap.png
```

离线打开 `demo-out/report/diagnostics.html` 即可查看诊断界面。

## CLI

```text
terrainbake gen      --seed 42 --out sample
terrainbake validate --project sample/project.json

terrainbake bake --project sample/project.json --store cache --mode full \
  --workers 4 --thread-pixels 8388608 --memory-budget 268435456

terrainbake bake --project sample/project.json --store cache --mode incremental \
  --edits examples/edits.json --project-out sample/edited.json --report diag

terrainbake bake --project sample/edited.json --store cache --mode resume \
  --project-out sample/edited.json

terrainbake verify --project sample/project.json --edits examples/edits.json \
  --root verify-run --workers 4

terrainbake truth --project sample/project.json
terrainbake demo  --out demo-out
```

所有命令向 stdout 输出 JSON；业务错误向 stderr 输出带 `E_` 前缀的 JSON，并返回非零整数退出码。

常用参数：

| 参数 | 含义 |
|---|---|
| `--workers` | 最大并行工作线程数 |
| `--thread-pixels` | 单线程像素缓冲上限，块像素数超过时返回 `E_RESOURCE` |
| `--memory-budget` | 在途法线图 / 原始法线缓冲的内存预算（字节） |
| `--pause-after` | 成功提交 N 个块后暂停，写完整 pending 检查点 |
| `--operator` | `sobel`、`central` 或 `forward` |
| `--scale` | 高程梯度到法线斜率的缩放 |
| `--project-out` | 保存编辑后工程；暂停/重启续跑时必须使用 |

## 工程 Schema

`Project` 是自包含 JSON，字段名与 Go 结构体一致。以下为字段骨架（数组内容省略，实际数组长度必须满足校验规则）：

```json
{
  "schema_version": 1,
  "name": "deterministic-terrain-42",
  "seed": 42,
  "blocks": [
    {
      "id": "tl",
      "width": 16,
      "height": 12,
      "origin_x": 0,
      "origin_y": 0,
      "heights": [1.0, 1.1],
      "material_ids": ["rock", "grass", "snow"],
      "weights": [160, 620, 220]
    }
  ],
  "materials": [
    {"id": "rock", "normal_scale": 1.15, "rotation_deg": 12}
  ],
  "seams": [
    {
      "id": "tl-tr",
      "block_a": "tl",
      "block_b": "tr",
      "edge_a": "right",
      "edge_b": "left",
      "reverse_b": false,
      "max_angle_deg": 5,
      "max_iterations": 8
    }
  ],
  "truth": [
    {"block_id": "tl", "x": 8, "y": 6, "normal": {"x": 0, "y": 0, "z": 1}, "slope_deg": 0}
  ],
  "seam_truth": [
    {"seam_id": "tl-tr", "index": 6, "angle_deg": 0}
  ]
}
```

校验规则：

- `heights` 数量必须严格等于 `width*height`；
- 每像素每层权重必须是 `[0,1000]` 的整型，且每像素总和严格为 `1000`；
- 材质 ID 必须存在且块内不重复；
- 接缝必须引用两个存在的块、相对边、等长边，并在世界坐标中实际相邻；
- 真值像素必须在块内，法线必须是有限非零向量，坡度在 `[0,90]`；
- 字段缺失、尺寸不符、权重越界均返回 `E_SCHEMA`，并带 `object` / `field`，例如：
  `{"code":"E_SCHEMA","object":"block:tl","field":"heights"}`。

## 烘焙公式

对块内像素 `(x,y)`，由高程 `h` 计算：

- `sobel`：3×3 Sobel 梯度，除以 8；
- `central`：中心差分；
- `forward`：前向差分；
- 边界采用 clamp。

每层材质 `m` 的切线空间法线：

```text
n_m = normalize(-dx * scale * m.normal_scale,
                -dy * scale * m.normal_scale,
                 1)
```

再绕 Z 轴旋转 `rotation_deg`。最终法线为量化权重加权和并归一化。坡度：

```text
slope = atan2(hypot(nx, ny), abs(nz)) * 180 / pi
```

法线 RGB 使用标准 `[-1,1] → [0,255]` 编码；坡度 PNG 为 16-bit，`0..90°` 映射到 `0..65535`。所有逐像素运算只用固定顺序的 `float64` 和标准库 `math`，无随机调度、无 GPU 路径。

## 接缝求解

每条接缝按 `seam_id`、边索引固定顺序处理。对一对边法线：

1. 计算夹角；若已不大于 `max_angle_deg`，保持不变；
2. 否则两个法线各向对方旋转夹角的 `1/4`，下一轮夹角约减半；
3. 最多执行 `max_iterations` 轮；
4. 若仍有违反，返回 `E_SEAM`，包含 `block_a`、`block_b`、`seam_id`、`angle_deg`、`limit`；
5. 成功时每块记录 `constrained`、`violations`、`max_angle_deg`，绝不静默放宽阈值。

两侧块使用完全相同的输入和迭代规则，因此无论哪个工作线程烘焙、哪条路径提交，边法线逐位一致。

## 依赖图与失效传播

`DependencyGraph` 显式包含：

- 每条接缝双向的 `SeamEdges[A] → B` 与 `SeamEdges[B] → A`；
- `MaterialUsers[material_id]` 记录使用该材质的所有块。

高程编辑从被编辑块出发；材质参数编辑从所有使用该材质的块出发。传播沿双向接缝边 BFS，记录：

```json
{
  "block_id": "tr",
  "reason": "downstream seam dependency via tl",
  "chain": ["tl", "tr"]
}
```

未进入失效集合的块，其检查点文件不会被重写；测试会比较 `normal_digest`、`data_digest` 前后值。调度顺序在失效子图上做 SCC 缩点后拓扑排序；双向接缝天然形成环，因此 SCC 是“双向依赖图”的正确拓扑单位。

## 并行调度与预算

调度分三阶段：

1. **并行 raw bake**：按稳定拓扑顺序派发；每个任务先预留内存，再计算并原子写入唯一临时 raw 文件，随后释放预算。
2. **确定性接缝求解**：只加载共享边法线，按固定顺序松弛。
3. **串行提交**：按完整工程的稳定拓扑顺序遍历；失效块解码 raw、生成 PNG、原子写检查点；未失效块直接复用。因此并行/单线程的最终输出与可观察块顺序一致。

预算行为：

- 单块像素数大于 `max_thread_pixels`：返回 `E_RESOURCE`，字段 `max_thread_pixels`；
- 在途内存不足：阻塞排队，并在结果 `queue` 中记录 `normal_map_budget_wait` 或 `normal_map_finalize_wait`；
- 单块需求本身超过总预算：返回 `E_RESOURCE`，不丢块、不静默跳过。

## 检查点与恢复

每个块提交时，`Store` 在同一互斥区内原子写入：

- `normal.png`、`slope.png`、`data.bin`（最终 float 法线与坡度）；
- 清单项：法线摘要、坡度摘要、数据摘要、内容哈希、`params_version`、`algorithm_version`、接缝统计；
- 更新后的 `manifest.json`（临时文件 + fsync + rename）。

清单包含 `target_content_hash`、`epoch_dirty`、`pending`、稳定 `commit_order` 与块级 `failures`。暂停时 pending 持久化；中途崩溃最多留下未引用的孤儿文件，不会出现清单指向半个块。

重启打开检查点时，若以下任一项不匹配：

- 全局内容哈希；
- 顶层算法版本；
- 顶层参数版本；
- 任一非 dirty 块的内容哈希；
- 任一块参数版本或算法版本；
- 缓存文件摘要；

则整体删除旧快照并全量重烘焙。`resume` 不会混用旧块。恢复期间已提交的 dirty 块可以复用，pending 块继续提交；恢复会重建当前 epoch 的 raw 边值，保证接缝结果不依赖断点位置。

## 三条路径一致性

`VerifyThreePaths` 使用三个独立 store：

1. **live**：先全量基线，再实时增量编辑；
2. **resume**：基线后应用同一编辑序列，`pause-after=1` 暂停，关闭后从 `NewEngine + Resume` 重启续跑；
3. **full**：编辑后工程从空缓存一次性全量烘焙。

逐块按排序 ID、行优先像素比较 `float64` 法线三分量和坡度；随后比较接缝统计与提交序列。发现差异时返回 `E_DIFF`，并带首个不一致的 `block_id`、`x`、`y`。`CompareSnapshots` 只比较实际像素和语义元数据，不依赖临时文件位置或 goroutine 完成顺序。

## 错误码

| Code | 场景 |
|---|---|
| `E_SCHEMA` | 字段缺失、尺寸不符、权重越界、接缝引用/相邻性错误 |
| `E_PARA` | 差分算子、scale、材质参数、CLI 参数越界 |
| `E_SEAM` | 迭代上限后接缝仍超阈值；带块对与实测夹角 |
| `E_DIFF` | 三路径或真值比较失败；带首个不一致坐标与块 |
| `E_RESOURCE` | 线程像素缓冲或单块内存需求无法满足 |
| `E_STATE` | 未完成烘焙要求恢复、检查点状态不合法、上下文取消 |
| `E_IO` | 本地文件读写/摘要校验错误 |
| `E_BAKE` | 块内部异常；失败块保持 pending，可 `resume` 重试 |

块级异常不会阻止其他块提交。失败块不会进入完成快照；诊断页列出失败原因，后续 `terrainbake bake --mode resume` 只重试 pending 块。

## 可视化诊断

`WriteDiagnostics` 生成纯静态、无网络依赖的 HTML：

- `normals-atlas.png`：最终法线图集；
- `blocks-colored.png`：稳定块 ID 着色预览；
- `seam-heatmap.png`：接缝违反热力图（绿色无违反、红色有违反）；
- `diff-heatmap.png`：差异像素红色热力图；
- 依赖图 SVG：失效块红色、接缝边高亮；
- 表格：每块接缝统计、摘要、失效原因/链、提交序列、预算排队事件和失败重试说明。

## 代码布局

```text
model.go         数据模型、哈希、向量工具
errors.go        E_* 结构化错误与 JSON
validate.go      加载、字段存在性、尺寸/权重/接缝/真值校验
generator.go     固定种子样本生成器与真值清单
normal.go        差分、材质混合、PNG、raw/final 二进制
seam.go          双侧边配对、确定性迭代、接缝统计
deps.go          双向依赖图、失效传播、SCC 拓扑序、编辑
scheduler.go     内存限流、线程池 raw bake、预算排队记录
store.go         原子块文件/清单、摘要校验、整体失效
engine.go        全量/增量/恢复、稳定串行提交、失败重试
compare.go       逐像素 E_DIFF 定位、三路径验证
diagnostics.go   HTML/PNG 诊断界面
cmd/terrainbake  CLI
```

## 实际验证记录

在本次环境（Go 1.22.12）实际执行：

```text
go test ./...                     PASS
go test -race -timeout 120s ./... PASS
go vet ./...                      PASS
GOOS=windows GOARCH=amd64 go build ./... PASS
```

覆盖的通过项：

- `go test` 共 22 个顶层测试，包括 schema 缺失字段/尺寸/非整型权重/权重越界、确定性生成、全量真值、并行/串行一致（含同一编辑序列）、稳定提交顺序、未受影响块复用、内存预算排队、线程像素预算错误、暂停/恢复、全局哈希与块版本整体失效、单块失败隔离与重试、`E_SEAM`、`E_DIFF` 坐标定位、诊断文件生成。
- `terrainbake demo --seed 42 --workers 3` 实际结果：4 块、3 材质、4 接缝、20 个真值样本；`three_paths_equal=true`、`truth_checked=20`。
- 资源预算测试将总法线图预算压到 10000 字节，仍完成全部块，并记录 `normal_map_budget_wait`。
- 接缝失败测试将阈值压到 `1e-9°`、迭代 1 轮，返回结构化 `E_SEAM`，包含块对、`angle_deg` 与 `limit`。
- 三路径测试同时比较法线、坡度、逐块接缝统计和块提交序列；全部通过。

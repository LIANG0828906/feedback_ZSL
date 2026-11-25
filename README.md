# textureconv

`textureconv` 是一个纯 Go、可中断续跑的纹理通道转换组件。它把多条目转换计划切分为固定大小的图像分块，为每个“条目 × 分块”维护状态与内容哈希缓存，并用单个 SQLite 快照保存已提交结果。组件同时提供：

- 严格计划/源图/真值校验，稳定错误码 `E_SCHEMA`、`E_PARA`、`E_STATE`、`E_DIFF`；
- 8/16 位通道重排、填充、丢弃、alpha 处理和确定性量化；
- 依赖图、局部失效清单、并行分块调度；
- 实时增量、断点恢复、一次性全量三条路径；
- 不调用转换主路径的独立持久化结果验证器；
- 确定性样本生成器、CLI 和端到端一致性演示。

运行时不访问网络。SQLite 由 `modernc.org/sqlite` 提供，不需要 C 编译器。

## 快速开始

要求 Go 1.22 或更高版本，且 `go` 在 `PATH` 中。

```bash
go test ./...
go vet ./...

# 生成默认样本：8 位 RGBA、16 位 BGRA、2048x1536 16 位 RGBA
go run ./cmd/texconv generate --out sample

# 转换、原子检查点、真值验证并导出最终 stage
go run ./cmd/texconv convert \
  --sample sample \
  --image atlas_16_large \
  --store out/large.db \
  --out out \
  --workers 4 \
  --tile 64 \
  --export-final out/final.tstage

# 独立验证器只读取 SQLite 分块、计划与真值指纹
go run ./cmd/texconv verify \
  --sample sample \
  --image atlas_16_large \
  --store out/large.db \
  --params-version v1

# 中断/续跑：第一次只提交 3 个分块，再次执行会从版本匹配的检查点继续
go run ./cmd/texconv convert --sample sample --image atlas_8_rgba \
  --store out/resume.db --tiles 3
go run ./cmd/texconv resume --sample sample --image atlas_8_rgba \
  --store out/resume.db

# 显式丢弃全部检查点后全量重算
go run ./cmd/texconv full --sample sample --image atlas_8_rgba \
  --store out/resume.db

# 自动执行实时增量、重启续跑、并行全量、串行全量并逐像素比较
go run ./cmd/texconv demo --out demo-out --workers 4 --tile 32
```

Windows 上使用本地可写目录即可，例如：

```powershell
go run .\cmd\texconv generate --out .\sample
go run .\cmd\texconv convert --sample .\sample --image atlas_8_rgba --store .\data\checkpoints.db --out .\out
```

## 样本格式

`generate` 写出 `manifest.json`、`plan.json`、`truth.json` 和若干 `.ximg`：

- `.ximg` 是自描述确定性容器：8 字节 magic、宽高、位深、通道数、`RGBA`/`BGRA` 布局、payload 长度，随后是交错的小端 `uint16` 通道值。
- `manifest.json` 记录种子、源图路径、几何、位深、布局和源文件 SHA-256。
- `truth.json` 记录计划指纹、每个采样点的条目序号、坐标、目标位深、期望通道值和量化容差，并带自校验指纹。
- `plan.json` 是有序转换计划。

计划示例：

```json
{
  "schemaVersion": 1,
  "entries": [
    {
      "seq": 1,
      "targetDepth": 16,
      "targetChannels": 4,
      "permutation": [2, 1, 0, 3],
      "fill": [],
      "drop": [],
      "quantization": "round",
      "alpha": "preserve"
    },
    {
      "seq": 2,
      "targetDepth": 16,
      "targetChannels": 4,
      "permutation": [0, 1, 2, 3],
      "fill": [],
      "drop": [],
      "quantization": "round",
      "alpha": "opaque",
      "region": { "x": 17, "y": 13, "w": 97, "h": 71 }
    },
    {
      "seq": 3,
      "targetDepth": 16,
      "targetChannels": 4,
      "permutation": [0, 1, 2, -1],
      "fill": [{ "channel": 3, "value": 65535 }],
      "drop": [],
      "quantization": "round",
      "alpha": "preserve"
    }
  ]
}
```

### 条目语义

- `seq` 必须从 1 开始连续。
- `targetDepth` 只能是 `8` 或 `16`。
- `targetChannels` 为 1 到 4。
- `permutation` 长度必须等于 `targetChannels`。每个非负值是当前条目的源通道索引；同一值不可重复。
- `-1` 表示该目标槽由 `fill` 提供。`fill[].channel` 必须恰好覆盖所有 `-1` 槽，`fill[].value` 使用目标位深原始值。
- `drop` 必须恰好等于当前源通道中未出现在 `permutation` 里的通道集合。这样通道增减策略与目标通道数严格相容。
- `quantization`：`round`、`floor`、`ceil`。同输入/输出位深时始终精确恒等。
- `alpha`：`preserve`、`opaque`、`premultiply`、`straighten`。alpha 操作在源位深域执行，再映射到目标通道；被丢弃的通道会累计记录到 `Dropped`。
- alpha 槽位跟随源 alpha 经 `permutation` 映射；仅由 `fill` 写入的槽不会自动成为新的 alpha 语义。
- `region` 可选。带区域的条目只改变区域内像素；为保持全图位深/通道元数据一致，区域条目不得改变位深、通道数，且必须保留源 alpha 槽。区域外像素逐值不变。
- 8→16 使用精确 `v * 65535 / 255`；16→8 按声明方向量化。

## 错误码

| Code | 含义 |
|---|---|
| `E_SCHEMA` | JSON 字段缺失、未知字段、结构不完整、序号不连续、图像/真值结构不匹配 |
| `E_PARA` | 位深、通道置换、填充/丢弃、区域、量化或 alpha 参数非法；错误带条目序号和字段名 |
| `E_STATE` | 非法状态迁移、处理中编辑、未显式解冻已提交分块 |
| `E_DIFF` | 增量/全量不一致、持久化内容哈希不一致、独立验证采样不一致；带像素、条目、分块和两侧值 |
| `E_PROCESS` | 分块处理失败或被取消；报告列出失败分块，已提交分块仍保留 |
| `E_IO` | 文件或 SQLite I/O 失败 |

## 组件 API

主要入口位于根包 `textureconv`：

```go
import "textureconv"

bundle, err := textureconv.LoadSample("sample")
im := bundle.Images["atlas_8_rgba"]

store, err := textureconv.OpenStore("out/checkpoints.db")
engine, err := textureconv.NewEngine(im, bundle.Plan, textureconv.EngineOptions{
    ImageID:       "atlas_8_rgba",
    SourceHash:    bundle.Hashes["atlas_8_rgba"],
    ParamsVersion: "project-v3",
    TileSize:      64,
    Workers:       4,
    Store:         store,
})

report, err := engine.Process(ctx)        // 增量/续跑
// report, err = engine.FullRecompute(ctx) // 显式清空后全量

snapshot, err := engine.Snapshot()        // 所有阶段、最终图和逻辑提交序列
verify, err := textureconv.VerifyPersisted(bundle, "atlas_8_rgba", store, textureconv.VerifyOptions{
    ExpectedParamsVersion:    "project-v3",
    ExpectedAlgorithmVersion: textureconv.AlgorithmVersion,
})
```

单条目转换可直接使用：

```go
out, err := textureconv.ConvertEntry(stage, entry)
```

### 缓存键与局部失效

每个 `(条目, 分块)` 缓存键为：

```text
SHA-256(algorithm | entry | tile | effective-entry-params | input-content-hash | upstream-cache-key)
```

第一阶段输入哈希来自实际源图分块，后续阶段使用上游阶段该分块的内容哈希与缓存键。区域外分块使用稳定的 `pass-through` 参数指纹，并包含区域与分块的相对掩码，因此编辑区域参数只使相交分块失效。

`ComputeInvalidation(old, new)` 返回首个变化位置和逐条目失效清单，每条包含条目序号、受影响的真实分块编号及原因。修改某一条目时只从该条目开始向后游传播；区域修改不会使区域外分块失效。

### 状态机

每个“条目 × 分块”只有：

```text
pending -> processing -> committed
                    \-> failed -> processing -> ...
```

`committed` 是终态。非法迁移返回 `E_STATE` 且原状态不变。进度同时给出已完成分块数/总分块数和逐条目进度。

已提交分块不可静默改写。`ApplyEdit` 默认在受影响区域包含已提交分块时返回 `E_STATE`；调用方必须显式传入 `EditOptions{AllowUnfreeze: true}` 才会以事务删除受影响分块的旧提交并重算。由于 SQLite 以整块事务提交，解冻按整个分块进行。

### SQLite 快照

数据库包含：

- `snapshot`：单一整体版本（图像 ID、源哈希、计划指纹、参数版本、算法版本、几何）；
- `chunk_commits`：每分块提交哈希与单调提交序号；
- `stage_results`：该分块在所有条目上的输出、缓存键、内容哈希、计划指纹、参数/算法版本、像素和丢弃通道元数据。

每个分块的全部阶段在一个事务内写入；写入由互斥锁和单 SQLite 连接串行化。重启时任一行内容哈希、提交哈希、计划指纹或版本不匹配，整个快照被删除并从头计算，不允许混用部分旧条目。

### 并行与缓冲

引擎按批并行处理分块，批大小不超过 `Workers`。每线程瞬态像素预算由 `BufferBytesPerThread` 限制；预算不足时 `BufferLimiter.Acquire` 阻塞排队并把原因写入 `RunReport.BufferWaits`，不会丢弃任务。并行和串行路径输出相同。

### 独立验证器

`VerifyPersisted` 只读取：

- SQLite 中已完成的分块阶段行；
- 计划清单；
- 真值文件及其指纹；
- manifest 中稳定的图像/真值指纹元数据。

它不调用 `ConvertEntry`、转换缓存或调度器。验证器会重新拼装每个条目中间图、重算摘要、检查目标位深和通道元数据，并逐采样点比对容差。首个违规项以 `E_DIFF` 返回，其中包含条目序号、分块编号、像素坐标和字段名（如 `channels[1]`）。

## 一致性定义

最终逻辑提交序列按 `(分块编号, 条目序号)` 升序导出，便于实时增量、断点恢复、全量并行和全量串行直接逐项比较。SQLite 的 `submit_order` 另记录实际物理提交顺序，保证同一运行内稳定、单调且不交错。

`CompareSnapshots` 依次比较所有条目中间图和提交序列，遇到首个差异返回 `E_DIFF`，包含：

- 条目序号；
- 分块编号；
- 像素坐标；
- 字段（如 `channels[2]`）；
- 两侧完整像素取样值。

## 验证结果

在 `Ubuntu 26.04 / Go 1.22.12 / linux-amd64` 上实际执行：

```text
$ go test ./...
ok      textureconv

$ go vet ./...
（无输出）

$ CGO_ENABLED=1 go test -race -count=1 ./...
ok      textureconv

$ go test -cover ./...
ok      textureconv   coverage: 70.8% of statements
```

默认样本大图 `atlas_16_large`（2048×1536，16 位，64 像素分块）实际转换结果：

```text
progress=768/768
cacheHits=0
cacheMisses=3840
verified samples=40 maxDeviation=0
```

`go run ./cmd/texconv demo --workers 4 --tile 32` 实际结果：301×217 样本，70 个分块，5 个条目；实时增量、关闭并重开 SQLite 后续跑、并行全量、串行全量四条路径的最终图哈希、逐条目摘要和提交序列全部相等，真值采样 40 个，最大偏差 0。

竞态检测需显式启用 cgo（仅用于 race 检测工具链；生产构建和 SQLite 驱动仍是纯 Go）。

测试覆盖：

- 缺字段 `E_SCHEMA`，非法位深/置换/填充/丢弃 `E_PARA` 及条目/字段定位；
- 8/16 位恒等、8→16 精确扩展、16→8 三个量化方向；
- 通道交换、增减、填充、丢弃记录、四种 alpha 模式、区域外不变；
- 依赖图、区域局部失效、序号调整后仅下游失效；
- 非法状态迁移保持原状态；
- 局部编辑后上游缓存命中；
- 已提交分块冻结与显式解冻；
- 单分块一次失败隔离、重试后与干净全量逐像素一致；
- 关闭/重开 SQLite 续跑与全量一致；
- 内容哈希/计划指纹不匹配时整体作废；
- 并行/串行一致、提交序号稠密稳定；
- 独立验证器发现首个真值违规、自动比对定位像素/条目/分块；
- 溢出缓冲阻塞并记录原因。

## 目录

```text
errors.go       稳定错误码
types.go        图像、阶段、计划、区段类型
plan.go         严格 JSON 解码与计划校验
bundle.go       manifest/truth/plan 交叉校验
imageio.go      XIMG/stage 容器、内容哈希
convert.go      像素转换与量化
generate.go     确定性样本与独立参考真值
tiles.go        分块与拼装
graph.go        依赖图、缓存键、失效清单
state.go        双层状态机与进度
engine.go       并行调度、编辑、快照、全量/增量
store.go        SQLite 原子检查点
verify.go       独立验证器
compare.go      自动逐像素比对
limiter.go      有界像素缓冲与等待记录
demo.go         三路径一致性演示
cmd/texconv     CLI
```

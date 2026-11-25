# texturekernel

`texturekernel` 是一个可集成的 Go 贴图位深与通道转换内核。它提供三版本计划统一加载、确定性通道重排/量化、跨版本导出、区域级缓存失效、条目×分块状态机、SQLite 原子检查点、三路径一致性比较，以及不调用转换主路径的独立核对工具。

项目不依赖 CGO、Docker、外部服务或网络资源。样本由本地确定性生成器按种子产出。

## 环境与快速开始

要求 Go 1.22 或更高版本。SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.29.10`。

```bash
go test ./...
go test -race ./...
go build ./...

# 生成样本、跑全量/增量/断点恢复三路径、比对并独立核对
go run ./cmd/texture-demo -out artifacts/demo

# 独立核对已持久化结果（不调用转换主路径）
go run ./cmd/texture-verify \
  -db artifacts/demo/full.db \
  -run demo-full \
  -plan artifacts/demo/plan-v3.json \
  -truth artifacts/demo/truth.json
```

Windows 上同样使用上述命令；模块路径、数据库路径可为普通本地路径。

最小集成：

```go
planData, _ := os.ReadFile("plan-v3.json")
plan, _ := texture.LoadPlan(planData)
source, _ := texture.ReadImageFile("source.json")

st, _ := store.Open("checkpoint.db")
defer st.Close()
runner, _ := pipeline.NewRunner(st, pipeline.Config{
    ChunkWidth: 128, ChunkHeight: 96, Workers: 4,
    PixelBufferPerWorker: 16 << 20,
})
result, err := runner.Run(ctx, source, plan, pipeline.Options{
    Mode: pipeline.ModeFull, RunID: "job-0001",
})
```

后续对编辑后的计划使用 `ModeIncremental` 与 `PreviousRunID/PreviousPlan`；中断后使用同一 `RunID` 的 `ModeResume`。

## 目录

| 路径 | 作用 |
|---|---|
| `model.go`, `image.go` | 内部计划/图像模型、结构化错误、哈希 |
| `plan_load.go`, `plan_export.go` | v1/v2/v3 加载、归一化、逐字段痕迹、导出与损失报告 |
| `convert.go` | 通道映射、填充/丢弃、位深量化和 alpha 处理 |
| `graph.go` | 分块、阶段依赖、内容哈希缓存键、区域失效清单 |
| `state.go` | 条目×分块双层状态机与冻结快照 |
| `store/sqlite.go` | SQLite 原子提交、稳定提交序、检查点读取与校验 |
| `pipeline/` | 全量/增量/恢复执行器、并发调度、三路径比较 |
| `sample/` | 固定种子源图像、三版本计划、解析真值生成器 |
| `verify/` | 仅基于持久化分块、计划与真值的独立核对器 |
| `cmd/texture-demo` | 本地可运行端到端示例 |
| `cmd/texture-verify` | 独立核对命令行 |

## 三版本计划

版本只从 JSON 内容判定，不依赖文件名：

- v1：`version` + 扁平 `source` 字段；`entries[].channel_order` 表示目标通道顺序，`source_order` 表示并行源通道映射；单个 `fill` 组；alpha 仅 `copy/drop`。
- v2：`schema_version` + `image.w/h/depth.bits/channels`；`operations[].region` 使用 `left/top/right/bottom`；`output.layout`、`channel_map`、逐通道 `fill_sources`；alpha 为 `{ "mode": ... }`。
- v3：`format: "texture-plan/3"`；`source.pixel_format` 组合位深/通道布局；`entries[].area`、`source_order`、分组 `padding`、`alpha_policy` 和 `quantization`。

所有版本归一化为：

```go
type Plan struct {
    Version string
    Source  Source
    Entries []Entry
    Traces  []FieldTrace // 源级字段痕迹；条目级痕迹在 Entry.Traces
    Fingerprint string
}
```

加载失败返回稳定错误码：

- `E_VERSION`：未知/缺失版本。
- `E_SCHEMA`：必填字段缺失、字段类型错误、结构非法；`Path` 给出准确 JSON 路径。
- `E_PARA`：数值、覆盖区域、映射、缓冲上限等参数越界。
- `E_LOSSY`：跨版本导出存在不可表达字段；返回值同时包含可加载的降级产物和逐项 `Loss` 清单。
- `E_STATE`：非法状态迁移，原状态保持不变。
- `E_DIFF`：三路径比较或独立核对发现首处差异。
- `E_STORE`：SQLite/检查点错误。

导出示例：

```go
data, losses, err := texture.ExportPlan(plan, "1")
// err 可能为 E_LOSSY；data 仍可被 v1 加载器读回。
loaded, loadErr := texture.LoadPlan(data)
```

对于可无损表达的字段，`EquivalentForExport` 验证导出读回后的通道置换、目标位深、填充约定和舍入方向逐项一致。
本地测试还会对已加载的 v1/v2/v3 计划执行 3×3 全目标版本导出矩阵，逐一读回比对。

## 转换语义

- 目标通道布局由 `Entry.TargetChannels` 定义，`Mappings` 与之一一对应。
- 映射可为源通道、常量、零、满量程或另一个源通道填充。
- 常量填充值按目标位深取值域解释；源通道填充先量化到目标位深。
- 输出未使用的输入通道记录在 `DroppedChannels`。
- 量化使用整数精确运算，支持 `nearest/floor/ceil`；输出始终落在目标位深范围，`0` 与源最大端点分别精确映射到 `0` 与目标最大端点。
- alpha 支持 `copy/drop/straight/premultiply/unpremultiply`。
- 部分覆盖条目只允许保持当前格式；改变位深或通道布局的条目必须覆盖整图。部分覆盖条目在覆盖区外产生逐像素不变的直通结果。

核心 API：

```go
result, err := texture.ApplyEntry(input, plan, plan.Entries[i])
final, infos, err := texture.ConvertPlan(source, plan)
q, err := texture.Quantize(value, srcBits, dstBits, texture.RoundNearest)
```

## 缓存、依赖与失效

每个分块、每个条目的缓存键为：

```text
SHA256(input_region_content_hash, entry_index,
       effective_entry_param_hash, upstream_cache_key)
```

其中 `effective_entry_param_hash` 对未覆盖该分块的局部条目使用稳定 no-op 参数身份。这样编辑局部条目的映射时，只有实际覆盖分块及其下游分块失效。

`BuildStageGraph` 显式给出：

- 条目间依赖边。
- 条目实际覆盖矩形。
- 与覆盖矩形相交的分块坐标。

`Invalidate(oldPlan, newPlan, chunkW, chunkH)` 输出 `entry_index/chunk_x/chunk_y/reason` 清单。覆盖区域编辑会同时考虑旧区域和新区域。

## 状态机与检查点

状态迁移：

```text
pending -> processing -> committed
                      \-> failed -> pending (retry)
committed 为终态
```

非法迁移返回 `E_STATE` 且状态不变。每个条目/分块提交时冻结输出摘要、缓存键、完整规范化参数 JSON、参数哈希、计划指纹和计划版本。计划编辑通过新计划/新运行表达，不修改已冻结快照。

SQLite 使用 WAL、单连接和提交互斥锁；一个分块的所有条目输出在单个事务中写入。分块按行优先顺序稳定提交，因此重启后的提交序列可重现。

恢复规则：

- 全局源哈希、计划指纹、参数版本、算法版本、分块几何任一不匹配，整批快照作废。
- 任一阶段内容哈希、缓存键、输入区域哈希或逐阶段版本不匹配，整批快照作废。
- 只有已持久化且逐块校验通过的稳定前缀可恢复；其余分块全量重算。
- 用户取消与模拟强制终止（关闭并重开数据库）均只依赖已提交检查点。

## 并发与缓冲

`pipeline.Config` 控制：

```go
ChunkWidth, ChunkHeight      // 分块尺寸
Workers                      // 最大并行度，1..64
PixelBufferPerWorker         // 每工作线程最大像素工作集
ParamVersion, AlgorithmVersion
QueueWaitThreshold           // 记录排队事件阈值
```

调度前检查单分块工作集是否超过每线程上限，超限返回 `E_PARA`。工作线程受 worker pool 限制；等待 worker/像素缓冲时记录 `QueueEvent`，包含分块坐标、原因和 `wait_us` 等待时长，不静默丢弃任务。计算可乱序完成，但提交由协调器按行优先顺序串行推进。

## 三路径与独立核对

`pipeline.Run` 支持：

- `ModeFull`：忽略旧结果，全量重算。
- `ModeIncremental`：校验旧运行，完整复用未被失效清单命中的分块；被影响分块内部仍可复用未变化的上游阶段。
- `ModeResume`：校验并恢复稳定前缀，继续未提交分块；校验失败自动作废并全量重算。

`ComparePaths` 要求至少三条路径逐像素一致，并比较：

- 最终像素值。
- 每个条目的中间结果摘要。
- 分块提交序列。

比较不把任何单条路径当作基准；任一差异返回 `E_DIFF`，附首个不一致坐标、条目序号和字段名。

独立核对器 `verify.VerifyCheckpoint` 只读取：

- SQLite 中持久化的分块输出。
- 计划清单。
- 真值指纹与解析构造的期望最终图像/采样点。

它不调用 `ApplyEntry`、`ConvertPlan` 或执行器，从末阶段分块重组最终图像，校验逐块哈希、逐阶段版本、完整提交序列，然后报告首处像素/字段偏差。

## 本次本地验证结果

### 8-bit RGBA，512×384，16 分块

```text
sample: 512x384 8-bit RGBA, 3 entries, 16 tiles
plan v1: traces=25 round_trip=true losses=0
plan v2: traces=28 round_trip=true losses=0
plan v3: traces=27 round_trip=true losses=0
invalidation manifest: 8 entry/tile pairs
full:        commits=16 final=200f81f2647610a75e07a8e6e1d3ad8d2eb4daedc5f69fdaf0e2696fbaa0204c
incremental: commits=16 computed=8 reused=8 final=200f81f2647610a75e07a8e6e1d3ad8d2eb4daedc5f69fdaf0e2696fbaa0204c
resume:      commits=16 restored=3 wiped=false final=200f81f2647610a75e07a8e6e1d3ad8d2eb4daedc5f69fdaf0e2696fbaa0204c
three-path compare: OK (200f81f2647610a75e07a8e6e1d3ad8d2eb4daedc5f69fdaf0e2696fbaa0204c)
independent verify: 16 chunks, 18 samples, final=200f81f2647610a75e07a8e6e1d3ad8d2eb4daedc5f69fdaf0e2696fbaa0204c
```

### 16-bit BGRA，512×384，16 分块

```text
sample: 512x384 16-bit BGRA, 3 entries, 16 tiles
plan v1/v2/v3: round_trip=true losses=0
incremental: commits=16 computed=8 reused=8
resume: restored=3 wiped=false
three-path compare: OK (11b2f7c6c34d7b6a6366b9d1619fff01c15710770d65a1d4f928dff1bb1e1131)
independent verify: 16 chunks, 18 samples
```

### 1536×1024 大图

```text
sample: 1536x1024 8-bit RGBA, 3 entries, 36 tiles
incremental: commits=36 computed=12 reused=24
resume: restored=5 wiped=false
three-path compare: OK (ad88aee850c8a87b96796eeb998b9a551cf440559327c30e2dac552d688fb312)
independent verify: 36 chunks, 18 samples
```

### 自动化测试

`go test ./...` 覆盖：

- 三版本加载/导出/二次读回与字段痕迹。
- `E_VERSION`、`E_SCHEMA`、`E_PARA`、`E_LOSSY`、`E_STATE`、`E_DIFF`。
- 通道增减、填充、丢弃、端点无损量化。
- 缓存失效仅命中被编辑条目的实际覆盖分块。
- 取消后关闭/重开数据库恢复。
- 篡改检查点内容哈希后整批作废并全量重算。
- 三路径最终像素、逐条目摘要、提交序列比较。
- 非整除尺寸的边界裁剪分块与末块重组。
- 独立核对器与真值采样点。
- 像素缓冲上限与并发竞态检查（`go test -race ./...`）。

## 设计边界

- 源图像布局为任务要求的 RGBA/BGRA，源位深为 8 或 16。
- 目标通道数为 1..4，目标位深为 1..16；示例样本使用 8/16。
- 部分覆盖阶段必须保持当前位深与布局。需要改变格式的阶段必须整图覆盖，避免混合格式像素。
- 图像在内存与检查点中按 `uint16` 原生样本存储；8-bit 数据保持 0..255，不做隐式归一化。

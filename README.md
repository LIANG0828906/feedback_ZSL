# vectorcore

`vectorcore` 是面向游戏 UI 动效与互动媒体装饰图形的 Go 矢量路径描边光栅化内核。首版包含可集成的领域包、三版本场景加载、确定性样本生成器、SQLite 断点检查点和本地示例渲染器。

## 交付内容

- 三版本 JSON schema 的统一映射，内部模型保留 `SourceVersion`。
- 全整数时间/坐标换算，支持帧、毫秒、绝对点与基点增量。
- 线性、smoothstep、hold 关键帧插值，闭合贝塞尔轮廓扁平化。
- 整数描边带、圆/斜/尖连接、扫描线子采样覆盖与多种合成模式。
- 每路径、每时刻、三阶段状态机及提交冻结。
- 内容哈希阶段缓存、依赖图、精确失效清单。
- 增量重绘与空缓存全量重绘的逐层、逐像素、提交序列比对。
- 版本化 SQLite 检查点，实时预览/断点恢复/全量重算一致性。
- 固定种子本地样本生成器，输出三 schema、采样时刻、解析面积/曲线点/包围盒/参考像素。

算法细节见 [docs/DESIGN.md](docs/DESIGN.md)，实际验证记录见 [docs/VERIFICATION.md](docs/VERIFICATION.md)。

## 环境

- Go 1.22 或以上。
- 纯 Go SQLite 驱动 `modernc.org/sqlite v1.34.5`。
- 不需要 Docker、网络服务、账号或外部图形运行时。
- 已验证 Linux 运行与 Windows amd64 交叉构建。

## 快速开始

```bash
go mod tidy
go test ./...
go run ./cmd/samplegen -out samples -seed 20261125 -batches 3
go run ./cmd/vectorrender \
  -scene samples/batch-01/scene.v3.json \
  -truth samples/batch-01/truth.v3.json \
  -out out/rendered/b01-v3 \
  -db out/checkpoints/b01-v3.db \
  -fresh
```

Windows 命令行：

```bat
go build ./...
go test ./...
go run .\cmd\samplegen -out samples -seed 20261125 -batches 3
go run .\cmd\vectorrender -scene samples\batch-01\scene.v3.json -truth samples\batch-01\truth.v3.json -out out\rendered\b01-v3 -db out\checkpoints\b01-v3.db -fresh
```

`vectorrender` 输出：

- `out/rendered/.../time-XX.png`：逐采样时刻渲染结果。
- `out/rendered/.../diff-report.json`：像素偏差、包围盒、面积、曲线采样和首个差异定位。
- `out/checkpoints/...db`：SQLite 提交检查点。

不传 `-fresh` 再次运行同一命令会从检查点恢复；当前示例已验证恢复报告与首次渲染逐字节一致。

## 样本目录

```text
samples/
  manifest.json
  batch-01/
    scene.v1.json
    scene.v2.json
    scene.v3.json
    truth.v1.json
    truth.v2.json
    truth.v3.json
    ref-00.rgba ... ref-12.rgba
```

固定种子 `20261125` 生成 3 批、每批 3 条路径、7 个关键帧、13 个采样时刻。三个版本共享同一解析构造，但分别经过自身 schema 加载后再生成真值记录。

真值记录包含：

- 每段二次/三次贝塞尔在 `t=0, 1/4, 1/2, 3/4, 1` 的整数坐标。
- 闭合轮廓解析面积。
- 路径与整帧包围盒。
- 位图宽高、RGBA8 参考像素文件及 SHA-256。
- 扁平化参数、子采样网格与像素格式。

生成器真值装配路径不调用 `Engine.RenderAll`、缓存或状态机；它直接由解析几何和确定性覆盖构造生成。

## Schema 版本

| 版本 | 时间单位 | 控制点 | 字段风格 | 可选样式 |
|---|---|---|---|---|
| v1 | 帧 | 绝对坐标 | 扁平 camelCase | 描边宽度/连接、混合模式可省略 |
| v2 | 毫秒 | 基点 + `dx/dy` 增量 | snake_case、`timeline` | `stroke` 与 `blend` 可省略 |
| v3 | 帧 | 绝对坐标 | 嵌套 `timeline/render/keys` | 宽度、连接、混合模式可省略 |

未知版本返回 `E_VER`；必填字段缺失返回 `E_SCHEMA`，并携带字段路径和源版本。JSON 结构损坏返回 `E_DECODE`。

## 集成示例

```go
scene, err := vectorcore.LoadSceneFile("samples/batch-01/scene.v2.json")
if err != nil {
    return err
}
cfg := vectorcore.DefaultConfig()
engine, err := vectorcore.NewEngine(scene, cfg)
if err != nil {
    return err
}

store, err := vectorcore.OpenCheckpoint("out/demo.db")
if err != nil {
    return err
}
defer store.Close()
if err := engine.LoadCheckpoint(context.Background(), store); err != nil {
    return err
}
timeline, err := engine.RenderAll(context.Background())
if err != nil {
    return err
}
for _, tr := range timeline.Times {
    // tr.Bitmap、tr.Paths[i].Coverage、tr.Paths[i].CompositeAfter 均可消费。
}
```

实时预览后编辑并增量重绘：

```go
preview, _ := vectorcore.NewEngine(scene, cfg)
_ = preview.PrimeCache(ctx)

invalidations, _ := preview.UpdatePaint("pulse-fill", newPaint)
_ = invalidations // 含 pathId、timeIndex、stage、reason

incremental, _ := preview.RenderAll(ctx)

fullEngine, _ := vectorcore.NewEngine(preview.Scene(), cfg)
full, _ := fullEngine.RenderAll(ctx)
diff, err := vectorcore.CompareTimelines(incremental, full)
if err != nil {
    // E_DIFF，diff 已定位 timeIndex、x、y、pathId、stage。
}
```

## 核心约束

- 内部坐标：`4096` 单位/逻辑像素，`CoordScale`。
- 内部时间：`36,000,000` tick/秒，可整除 24/25/30/50/60/120 FPS。
- 换算只使用整数或有理数，禁止浮点累积。
- 默认扁平化容差 `256`（1/16 像素），子采样网格 `4×4`。
- 默认并行度不超过 4，硬上限 8。
- 单个像素缓冲默认上限 64 MiB，硬上限 256 MiB。
- 覆盖值为 `uint16`，合成像素为 RGBA8。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_VER` | 未知 schema、检查点或管线版本 |
| `E_SCHEMA` | 必填字段缺失或路径/时刻结构非法 |
| `E_OVF` | 时间、坐标或面积整数换算溢出 |
| `E_PARA` | 插值、缩放、容差、并行度、网格等参数越界 |
| `E_MODE` | 透明度、混合模式或连接模式非法，含路径标识 |
| `E_STATE` | 非法状态迁移或冻结边界冲突 |
| `E_DIFF` | 增量与全量结果不一致，含首个差异位置 |
| `E_DECODE` | JSON、检查点 payload 或哈希校验失败 |
| `E_LIMIT` | 位图尺寸或像素缓冲超过硬上限 |
| `E_IO` | 本地文件或 SQLite I/O 失败 |

## 工程结构

```text
errors.go          稳定错误码与上下文字段
model.go           内部动画模型、配置和边界
arithmetic.go      帧/毫秒/坐标整数换算
schema*.go         v1/v2/v3 加载、映射与导出
interpolate.go     关键帧插值与曲线采样
flatten.go         闭合扁平化、多边形面积、解析贝塞尔面积
stroke.go          描边带与连接环
raster.go          包围盒视图、扫描线覆盖
composite.go       透明度与合成模式
state.go           三阶段状态机
cache.go           内容哈希缓存键与依赖
engine.go          并行调度、提交与阶段装配
edit.go            精确失效与参数编辑
checkpoint.go      SQLite 版本化检查点
diff.go            增量/全量逐项比对
cmd/samplegen      确定性三版本样本生成器
cmd/vectorrender   本地渲染、PNG、SQLite 与差异报告示例
internal/samplegen 样本构造与独立参考装配
```

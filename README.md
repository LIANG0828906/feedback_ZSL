# GlyphBrush

面向游戏 UI 装饰字形的本地确定性笔触填充管线与验证工具。输入是量化为整数的二次/三次贝塞尔闭合轮廓；输出不是抗锯齿轮廓填充，而是：

1. 扁平化闭合轮廓骨架（轮廓序号 + 累计弧长）；
2. 按笔触宽度、连接、笔尖与曲率联动间距生成的简单笔触带；
3. 扫描线中心采样得到的灰度笔画掩膜；
4. 每个骨架采样点的密度（PPM）与笔触带左右边界点；
5. 笔画段级依赖图、精确失效清单、增量/全量逐像素比对；
6. 四类可重试异常和版本一致的本地检查点；
7. 静态 PNG/JSON 导出与 Fyne 可视化面板。

运行期只使用本地文件和回环资源。样本生成器不访问网络。

## 快速开始

```bash
go version                 # 需要 Go 1.22+
go test ./...

go run ./cmd/glyphbrush generate -out ./samples -seed 20261124
go run ./cmd/glyphbrush validate -samples ./samples
go run ./cmd/glyphbrush render   -samples ./samples -out ./out -checkpoints ./out/checkpoints
go run ./cmd/glyphbrush incremental -samples ./samples -out ./out -new-width 34
go run ./cmd/glyphbrush verify   -samples ./samples -out ./artifacts/verification -checkpoints ./artifacts/verification/checkpoints
go run ./cmd/glyphbrush inject   -samples ./samples -glyph ui-a -type dens -segment 2
go run ./cmd/glyphbrush render   -samples ./samples -restore -out ./out -checkpoints ./out/checkpoints
```

`inject` 会返回对应的结构化错误并以非零状态退出（例如上面的 `E_DENS`）。`render -restore` 在版本匹配时直接复用检查点；版本不匹配时打印原因并从空缓存全量重绘。

完整验收命令会输出 `artifacts/verification/verification.json`、`latest.json`，以及每个字形的：

```text
outline.png    闭合轮廓与扁平化骨架
bands.png      半透明笔触带叠加
mask.png       灰度笔画掩膜
density.png    密度热力图与边界点
summary.json   版本、哈希、网格、段状态、失效清单
densities.json 逐骨架采样点密度
segments.json  笔画段状态矩阵
invalidation.json
```

### Fyne 面板

```bash
go install fyne.io/fyne/v2/cmd/fyne@latest
go run -tags fyne ./cmd/glyphbrush-ui -samples ./samples
```

Windows 上按 Fyne 官方文档准备 CGO/GCC 与图形环境，然后在 Windows 本机执行同一命令。也可用：

```powershell
fyne package -os windows -name GlyphBrush
```

面板提供字形选择、宽度/间距/连接/笔尖调整、检查点恢复、逐段重试、异常注入、PNG/JSON 导出、四类图像、段状态矩阵和失效拓扑文本示意。

本仓库默认构建不引入 Fyne，因此核心测试不依赖图形库；Fyne 面板通过 `fyne` 构建标签隔离。没有 X11 的环境可用 Fyne 软件驱动检查编译：

```bash
go build -tags 'fyne ci' ./cmd/glyphbrush-ui
```

## 数据格式

`samples/glyphs.json`：

```json
{
  "schemaVersion": "glyphbrush.samples/v1",
  "generator": "glyphbrush/samplegen-v1",
  "seed": 20261124,
  "quantization": 1,
  "unitsPerEm": 1024,
  "coordinateRange": {"minX": -1024, "minY": -256, "maxX": 1024, "maxY": 1024},
  "densityRange": {"minPpm": 0, "maxPpm": 1000},
  "glyphs": [
    {
      "id": "ui-a",
      "baseline": 0,
      "advanceWidth": 700,
      "bounds": {"minX": -313, "minY": -2, "maxX": 314, "maxY": 720},
      "contours": [
        {
          "index": 0,
          "closed": true,
          "segments": [
            {
              "type": "cubic",
              "start": {"x": 0, "y": 720},
              "control": [{"x": 110, "y": 520}, {"x": 285, "y": 105}],
              "end": {"x": 315, "y": 0}
            }
          ]
        }
      ]
    }
  ]
}
```

`truth.json` 包含每字形的闭合面积、各轮廓面积、内容哈希、真值包围盒与每段 `t=0.25/0.5/0.75` 的已知曲线点。

加载校验（`internal/loader`）会检查：

- 必需字段存在，类型仅为 `quadratic`/`cubic`；
- 控制点数量、首尾连续、闭合标记、闭合首尾点；
- 所有坐标落在声明的 `coordinateRange`；
- 内容哈希、面积、包围盒和已知采样点与独立真值一致；
- 扁平化后的轮廓确实闭合且有正弧长。

失败返回 `E_SCHEMA`，并尽可能附上 `glyphId`、`contourIndex`。

## 错误码

| 代码 | 含义 | 关键上下文 |
|---|---|---|
| `E_SCHEMA` | 输入字段、闭合、连续性、量程或真值错误 | glyph、contour |
| `E_PARA` | width/spacing/join/tip/scale 等参数非法 | `param` |
| `E_GRID` | 栅格尺寸或世界坐标越界 | glyph、grid |
| `E_SELF` | 骨架或笔触带自交 | glyph、segment、param |
| `E_ZLEN` | 零长度笔画段 | glyph、segment、param |
| `E_DENS` | 密度超出声明 PPM 区间 | glyph、segment、param |
| `E_STAGE` | 单段内部暂态失败或落盘失败 | glyph、segment、param |
| `E_DIFF` | 增量与全量不一致 | 首个像素 `(x,y)`、segment |

结构化错误定义见 `internal/errs`。单段失败会保留在段状态中，其他段继续渲染；`Retry` 只重绘目标段。

## 几何与栅格算法

### 骨架

- 二次/三次贝塞尔使用确定性自适应细分，默认容差 `0.2` 字形单位。
- 每条扁平化骨架点记录 `glyphId`、`contourIndex`、`curveIndex`、累计 `arcLength`。
- 每个曲线段成为一个稳定编号的笔画段，`SegmentID` 从 1 递增，顺序按轮廓、曲线索引固定。

### 笔触带

```text
curveSpacing = clamp(1 / (1 + curvatureFactor * curvature * width), 0.18, 1)
step         = clamp(spacing * width * curveSpacing, 0.35, 18)
```

`spacing` 表示相对宽度的采样比例，因此同时受宽度和曲率影响。连接方式支持 `round/bevel/miter`，笔尖支持 `round/flat/square`。外侧连接做圆弧、斜切或斜接；内侧取相邻法线平均，避免内侧回折自交。生成后检查：

- 骨架折线非相邻边不得相交；
- 笔触带非相邻边不得相交（含端点接触和共线重叠）；
- 不得超出真值包围盒按笔触半径外扩的范围；
- 多边形缓冲点数量不得超过配置上限。

面积测试覆盖宽度 `12 → 56`：同一骨骼上，笔触带宽增加时面积单调不减。

### 掩膜与密度

`internal/raster` 先从真值包围盒、笔触外扩、缩放和网格量子推导世界栅格：

```text
allowance  = width / 2 * 1.35 + 3
world      = snapOutward(truthBounds.expand(allowance), gridSize / scale)
maskWidth  = round(world.width * scale)
maskHeight = round(world.height * scale)
```

`Grid` 同时保存 `truthBounds` 与 `worldBounds`，所以尺寸可回溯且与真值包围盒一致。多边形在像素中心做偶数-奇数扫描线填充，不使用子像素抗锯齿覆盖率。多段合成灰度：

```text
gray = 255 * overlapCount / (overlapCount + 1)
```

每个骨架采样点使用其段分量的积分图查询局部窗口覆盖，量化成 `0..1000` PPM；同时输出笔触带左右边界点。越界返回 `E_GRID`，区间非法或注入溢出返回 `E_DENS`。

## 依赖失效

`internal/pipeline/graph.go` 显式记录：

- 参数 → 笔画段；
- 控制点 → 曲线及同轮廓后续段（累计弧长参数会随该控制点变化）；
- 轮廓弧长区间 → 笔画段；
- 笔画段 → 轮廓合成层 → 字形合成层。

调整 `width/spacing/join/tip/curvatureFactor` 时，只有依赖该参数的段失效。编辑控制点时，当前曲线及其后同轮廓段失效；其他轮廓的段直接复用。失效清单包含：

```json
{
  "segmentId": 7,
  "contourIndex": 0,
  "curveIndex": 6,
  "arcStart": 2169.95,
  "arcEnd": 2455.19,
  "param": "control:0:0",
  "reason": "control point edited; curve and later arc-length parameters depend on it"
}
```

`topoOrder` 先列笔画段，再列轮廓层和最终合成层。

## 增量一致性与并发

- 失效段按稳定拓扑序进入工作线程池，最大并行度由 `maxParallel` 限制。
- 每段独立分配多边形/掩膜缓冲，单段点数上限由 `polygonBufferLimit` 限制。
- 并行结果收集完后，按 `SegmentID` 稳定顺序原子替换内存缓存。
- 未失效段复用原缓存。
- `Compare` 先逐像素行优先比较，再逐密度采样比较。像素不一致时用两个结果的分段掩膜定位所属段，返回 `E_DIFF` 和首个坐标。
- `FullRender` 始终从空缓存渲染，用于验收对照。

`go test -race` 已覆盖管线、检查点和 CLI。

## 检查点

目录：

```text
<checkpointRoot>/<glyphId>/manifest.json
<checkpointRoot>/<glyphId>/segments/000000.json
...
```

每个段记录笔触带、二进制掩膜分量、密度统计、记录哈希、字形内容哈希、参数版本、算法版本。`CommitAll` 在一个互斥区内重置快照、按 ordinal 顺序逐段写临时文件、`fsync`、`rename`，最后推进 manifest；不会出现交错写。

加载时任一以下条件不匹配即删除整个字形快照并返回版本失配，调用方必须全量重绘：

- 字形内容哈希；
- 笔触参数版本；
- 算法版本；
- 段数量、提交完整性、任一段记录哈希或几何尺寸。

不会混用旧段结果。

## 目录

```text
cmd/glyphbrush          CLI、验收套件、异常注入
cmd/glyphbrush-ui       Fyne 面板入口（fyne 标签）
internal/samplegen      固定种子样本与真值
internal/loader         schema/连续性/量程/真值校验
internal/skeleton       扁平化与弧长
internal/stroke         笔触带、连接、笔尖、自交检查
internal/raster         扫描线掩膜、密度、E_DIFF 像素定位
internal/pipeline       依赖图、增量调度、异常、恢复
internal/checkpoint     原子、版本化持久化
internal/viz/export     PNG/JSON 诊断导出
internal/ui             Fyne 面板
samples                 已提交的固定种子样本与真值
artifacts/verification  实际验收输出
```

## 实际验证记录

当前执行环境为 Linux WSL2（Go 1.22.12），未使用 Docker/容器。详细命令、结果和限制见 [VALIDATION.md](VALIDATION.md)。

- `go test ./...`：全部通过。
- `go test -race ./internal/pipeline ./internal/checkpoint ./cmd/glyphbrush`：通过。
- `glyphbrush verify`：31/31 检查通过。
- 三个字形：6/8/12 个笔画段；增量与全量掩膜、密度逐项一致。
- 参数变更后全部依赖段重绘；控制点编辑仅重绘同轮廓当前及后续段（3/6、4/8、8/12），其余段复用。
- 四类注入分别返回 `E_SELF`、`E_ZLEN`、`E_DENS`、`E_STAGE`；`E_STAGE` 可重试成功，其他类清除注入后可重试成功。
- 人工扰动结果可定位 `E_DIFF` 首个像素及所属段。
- 检查点同版本可恢复；参数版本变化会整体删除快照并要求全量重绘。
- Windows CLI 交叉编译产出 `artifacts/bin/glyphbrush.exe`。
- Fyne UI 已使用 `fyne ci` 软件驱动完成 Linux 编译及 Windows 交叉编译；当前环境没有 Windows 图形会话，因此未声称完成 Windows 原生 GUI 运行截图。

![Stroke bands](artifacts/verification/ui-a/bands.png)

![Density heatmap](artifacts/verification/ui-a/density.png)

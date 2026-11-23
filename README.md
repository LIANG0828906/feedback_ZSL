# PathMorph：可形变矢量路径合成内核

PathMorph 是一个仅使用 Go 标准库实现的游戏 UI 动效/互动媒体装饰图形内核。它按采样时刻对二次/三次贝塞尔闭合轮廓做关键帧插值、自适应中点扁平化、扫描线抗锯齿覆盖、灰度合成，并输出逐帧位图、逐路径覆盖统计、可恢复检查点与一致性诊断。

项目包含：

- 本地确定性场景与解析真值生成器（固定种子，不依赖网络）。
- 可配置插值、扁平化、缩放、子采样网格和合成模式。
- 基于路径数、关键帧数、采样时刻数和内存预算的精细/分块近似取舍。
- 四状态、三阶段的逐路径时间轴状态机。
- 原子 gzip 检查点，以及“全量重算 / 断点恢复 / 增量重算”三条执行路径。
- 逐像素一致性、质量容差、错误码链路实验与本地 Web 诊断界面。
- 可复现实验记录：`artifacts/experiment/`、`artifacts/stress/`。

## 环境

- Go 1.22 或以上。
- Windows 10/11 x64、Linux 或 macOS 均可；代码不使用平台相关 API。
- 无第三方依赖，`go.mod` 没有额外 require。
- 不依赖 Docker、容器、网络数据或外部图形库。

```sh
go version
go test ./...
```

Windows PowerShell 示例：

```powershell
go test ./...
go build -o pathmorph.exe .\cmd\pathmorph
.\pathmorph.exe generate -out data
```

## 快速复现

```sh
# 1. 确定性场景 + 真值
go run ./cmd/pathmorph generate \
  -out data -id demo -seed 42 \
  -paths 8 -keyframes 3 -segments 7 -samples 9 -scale 12

# 2. 结构校验
go run ./cmd/pathmorph validate \
  -scene data/demo.scene.json \
  -truth data/demo.truth.json

# 3. 完整实验：真值、精细/近似质量、三条执行路径、错误码链
go run ./cmd/pathmorph experiment \
  -out artifacts/experiment -seed 42 -paths 8 -keyframes 3 -samples 9 -scale 12

# 4. 压力场景（32/128/512/2048 条路径）
go run ./cmd/pathmorph stress -out artifacts/stress -seed 4242
# 如需真正渲染所有压力场景，增加 -run

# 5. 本地诊断界面
go run ./cmd/pathmorph ui \
  -scene data/demo.scene.json \
  -truth data/demo.truth.json \
  -work artifacts/ui -addr 127.0.0.1:8080
```

## 目录

```text
cmd/pathmorph/main.go   CLI：generate/validate/run/experiment/stress/ui
morph/types.go          场景、真值、样式、配置、质量与像素类型
morph/errors.go         稳定错误码与上下文字段
morph/load.go           JSON/gzip 严格加载与原子 JSON 写入
morph/validate.go       场景/真值结构校验
morph/generator.go      确定性场景与真值生成器
morph/geometry.go       Bezier 求值、解析包围盒、Green 定理面积
morph/eval.go           关键帧插值、自适应中点扁平化
morph/frame.go          包围盒/缩放/网格对齐的帧尺寸推导
morph/raster.go         扫描线子采样覆盖、描边组件并集、灰度合成
morph/adaptive.go       精细/近似策略与参数版本
morph/state.go          每路径每阶段四状态机
morph/checkpoint.go     原子 gzip 检查点、版本和内容哈希
morph/engine.go         工作线程池、内存限制、有序提交、失败重试
morph/quality.go        精细/近似三项质量指标与 E_SCALE
morph/compare.go        三条执行路径逐像素比对与 E_DIVERGE
morph/truthcheck.go     曲线点/面积/包围盒/闭合性对照
morph/export.go         PGM/PNG、CSV 覆盖统计、差异热力图
morph/ui.go             标准库 HTTP + Canvas 诊断界面
morph/experiment.go     可复现实验报告
morph/stress.go         分级压力场景生成
```

## 数据格式

场景文件是严格 JSON（也支持 gzip 自动识别）：

```json
{
  "format": "pathmorph.scene.v1",
  "version": 1,
  "id": "demo",
  "seed": 42,
  "quantum": 0.001,
  "coord_min": -48,
  "coord_max": 48,
  "render_defaults": { "scale": 12, "padding_pixels": 4, "max_frame_pixels": 2048 },
  "sample_times": [0, 0.125, 0.25, 0.5, 1],
  "paths": [
    {
      "id": "p0000",
      "opacity": 0.8,
      "blend_mode": "over",
      "fill_rule": "nonzero",
      "fill": { "color": [0.7, 0.8, 0.9] },
      "stroke": {
        "color": [1, 1, 1], "width": 0.8, "join": "round", "miter_limit": 4
      },
      "keyframes": [
        {
          "time": 0,
          "segments": [
            { "kind": "Q", "p0": {"x":0,"y":0}, "p1": {"x":1,"y":2}, "p2": {"x":3,"y":0} },
            { "kind": "C", "p0": {"x":3,"y":0}, "p1": {"x":4,"y":-1}, "p2": {"x":5,"y":1}, "p3": {"x":0,"y":0} }
          ]
        }
      ]
    }
  ]
}
```

约束：

- 每个关键帧是完整闭合轮廓；相邻段 `P0` 必须与前段终点一致，最后一段终点必须回到首点。
- `Q` 使用 `p0/p1/p2`，`C` 使用 `p0/p1/p2/p3`。
- 所有关键帧保持相同段数和段类型。
- 控制点必须有限、位于 `[coord_min, coord_max]` 且落在 `quantum` 网格上。
- 透明度 `[0,1]`；混合模式支持 `over/add/multiply/screen/replace`；连接方式支持 `round/bevel/miter`。

真值清单包含每个采样时刻的：

- 若干解析曲线采样点 `(segment,t,point)`。
- 轮廓面积、填充面积、描边长度面积估计、参考覆盖面积。
- 解析包围盒与闭合检查点。
- 帧宽高与渲染包围盒。
- `midpoint_of_keyframes` 标记，保证质量实验至少覆盖一个关键帧中点。

## 核心算法

### 1. 插值与扁平化

- 关键帧之间支持 `linear`、`smoothstep`、`smootherstep` 时间归一化。
- 对段内全部控制点做逐坐标插值，保留段类型与拓扑。
- 自适应中点细分使用控制点到弦线的最大偏差作为平坦度。
- 细分树固定：容差只能细化叶子，不能减少叶子，因此同一路径同一帧的折线段数随容差收紧单调不减。
- Bezier 包围盒通过导数求根得到精确极值，不使用控制点包围盒近似。
- 轮廓面积通过 Green 定理 `0.5 ∮(x dy - y dx)` 的多项式解析积分计算。

### 2. 覆盖与合成

- 填充路径在像素网格上做扫描线求交，每个像素使用 `grid × grid` 子采样。
- 支持 `nonzero` 与 `evenodd`。
- 描边由每段胶囊矩形与连接件多边形组成；每个连接件先独立累积子行覆盖，再与其他组件做并集，避免“逐子行 max”导致覆盖被低估。
- 混合模式在灰度空间实现；颜色使用 Rec.709 亮度转换为灰阶。
- 同一帧所有路径共享同一宽高和坐标映射；不同帧可有不同尺寸。
- 帧尺寸由当帧精确包围盒、缩放、像素 padding 与网格对齐共同推导；`grid <= 4` 统一按 4 像素对齐，使精细/近似模式可直接逐像素比较。

### 3. 自适应取舍

策略输入：

```text
work_units = paths × total_keyframes × samples
estimated_peak_bytes = 估算帧缓冲 × 并发数 + 折线/组件开销
```

规则：

1. `-mode fine` 强制精细模式，但不会改变质量一致性阈值。
2. 工作量和内存估算都在预算内时选 `fine`。
3. 否则选 `approx`：
   - 中点容差放大 4 倍；
   - 声明帧容差放大 2.5 倍；
   - 子采样网格减半，最低为 1；
   - 分块尺寸减半。
4. 所有选择写入 `Quality.Reasons`，并记录 `mode / mid_tolerance / frame_tolerance / grid / chunk_size / memory_budget_bytes / estimated_bytes / work_units`。
5. 实际运行时有全局像素缓冲池，容量取 `min(worker 上限 × 并发数, 内存预算)`；阻塞等待会记录 `pixel_buffer_wait` 事件，不静默丢弃任务。

### 4. 状态机与提交边界

每路径每时刻三个阶段：

```text
flatten, coverage, composite
pending -> processing -> committed
                    \-> failed -> processing -> committed
```

- 非法迁移返回 `E_STATE`，原状态不变。
- 已提交时刻的所有阶段永久冻结；修改容差、精度或网格只影响后续未提交时刻。
- 单路径单阶段失败只标记该阶段为 failed，其他路径和其他已提交时刻不受影响。
- `RetrySample` 复用已提交阶段缓存，仅重新执行失败阶段，然后提交该时刻。

### 5. 检查点与三条路径

- 每提交一个采样时刻，在互斥串行区域内写入一个 gzip JSON 文件。
- 写入流程：临时文件 → `fsync` → 原子 `rename` → 目录 `fsync`。
- 检查点包含：逐路径冻结折线、覆盖统计、完整灰度位图、内容哈希、像素哈希、算法版本、插值/容差参数版本、样式/混合版本和场景哈希。
- manifest 只记录连续已提交前缀；发现版本/场景不一致返回 `E_DIVERGE`，内容或像素哈希损坏返回 `E_DECODE`。
- 并发 worker 可乱序完成，但主调度器按采样时刻升序串行提交，保证写入顺序稳定。
- 实验验证以下三条路径逐帧尺寸、逐像素值完全一致：
  1. 一次性全量重算；
  2. 中途停止后检查点恢复；
  3. 从已有前缀继续的增量重算。

## 错误码

| 错误码 | 含义 | 必需上下文 |
|---|---|---|
| `E_DECODE` | 文件缺失、JSON 损坏、真值/检查点结构或哈希损坏 | `field` |
| `E_PATH` | 轮廓未闭合、段不连续、重复路径 ID、非法段类型 | `path_id`, `field` |
| `E_KEY` | 关键帧时间倒置/重复、拓扑不一致、采样时间序列非法 | `path_id`, `field` |
| `E_STYLE` | 坐标量化越界、透明度/颜色/宽度/混合/连接方式非法 | `path_id`, `field` |
| `E_PARA` | 容差为零、缩放非正、采样时刻越界、网格/预算非法 | `field`, `sample`, `time` |
| `E_SCALE` | 相对面积、BBox IoU、平均覆盖差超差 | `sample`, `path_id`, `metric`, 实测值、双方值 |
| `E_STATE` | 非法状态迁移、提交后重写、阶段失败 | `sample`, `path_id`, `field` |
| `E_DIVERGE` | 三条执行路径不一致或检查点版本冲突 | `sample`, 像素坐标, `path_id` |

所有错误都实现为 `*morph.Error`，可通过 `morph.AsError`/`morph.CodeOf` 获取稳定错误码和字段。

## 质量一致性

精细与近似模式逐帧比较三项指标：

1. 每路径覆盖面积相对误差。
2. 每路径覆盖包围盒 IoU。
3. 每路径平均覆盖差，以及整帧像素 MAE/RMSE/Max。

默认声明阈值：

```text
relative area <= 0.04
bbox IoU      >= 0.94
mean coverage <= 0.035
```

阈值独立于模式选择，`-mode fine` 不会放宽阈值。质量报告同时记录两种模式的折线段数、插值次数和像素误差统计，并强制检查至少一个关键帧中点采样。

## 实际验证结果

环境：Go `go1.22.12`，Linux amd64（代码同时面向 Windows）。

主实验命令：

```sh
go run ./cmd/pathmorph experiment \
  -out artifacts/experiment -seed 42 -paths 8 -keyframes 3 -samples 9 -scale 12
```

结果摘要（详见 `artifacts/experiment/EXPERIMENT.md` 和 `experiment.json`）：

- 场景：8 条路径、24 个路径关键帧、56 个闭合段、9 个采样时刻、576 个量化控制点。
- 精细模式：grid 4、中点容差 0.02、3773 个折线段、3072 次控制点插值。
- 近似模式：grid 2、中点容差 0.08、1896 个折线段、3072 次控制点插值。
- 真值检查：通过；曲线点最大误差 0，轮廓面积最大相对误差 0，栅格覆盖面积最大相对误差 0.0303。
- 精细/近似检查：通过；覆盖关键帧中点；首帧像素 MAE `0.000497`、RMSE `0.008263`、最大差 `0.501961`、变化像素 `5177`。
- 三条执行路径：9/9 帧尺寸与像素哈希完全一致。
- 错误码链：`E_DECODE / E_PATH / E_KEY / E_STYLE / E_PARA / E_SCALE / E_STATE / E_DIVERGE` 全部实测触发。
- 内存等待：近似运行记录 1 次 `pixel_buffer_wait`，请求约 32.1 MB，预算 96 MB，任务排队而非丢弃。
- 本次参考耗时：精细 136 ms、近似 117 ms（耗时随机器变化，像素结果不随并发变化）。

运行：

```sh
go test ./...
go test -race ./...
go vet ./...
```

上述三条命令均通过。

## 压力场景

`artifacts/stress/stress_report.json` 记录四级场景：

| 场景 | 路径数 | 关键帧 | 采样数 | 自动模式 | 网格 | 工作量 | 实测渲染 |
|---|---:|---:|---:|---|---:|---:|---:|
| stress-32 | 32 | 2 | 5 | fine | 4 | 10,240 | 63 ms |
| stress-128 | 128 | 3 | 5 | approx | 2 | 245,760 | 77 ms |
| stress-512 | 512 | 4 | 7 | approx | 2 | 7,340,032 | 236 ms |
| stress-2048 | 2048 | 5 | 7 | approx | 2 | 146,800,640 | 887 ms |

场景与真值以 `.json.gz` 保存，加载器自动识别 gzip。`artifacts/stress/` 包含四级场景、检查点和导出帧，约 14 MB；本次运行 `stress -run` 全流程约 5.3 秒。

## 诊断界面

```sh
go run ./cmd/pathmorph ui \
  -scene data/demo.scene.json \
  -truth data/demo.truth.json \
  -work artifacts/ui -addr 127.0.0.1:8080
```

界面功能：

- 关键帧时间轴和路径列表。
- 每个采样时刻、每条路径、每个阶段的状态。
- 当帧合成位图与当前路径折线叠加。
- 当前路径覆盖位图。
- 相对精细参考的差异热力图。
- 当前取舍模式、容差、网格、分块和预算依据。
- 运行/续跑、暂停、恢复、取消、失败阶段重试、导出帧序列。
- 所有图像与 JSON 由本地 Go HTTP 服务和 Canvas 直接提供，无外部前端依赖。

HTTP 端点：`/api/state`、`/api/frame`、`/api/control`。

## 测试覆盖

- 场景加载、gzip 往返、结构校验、真值校验。
- 关键帧插值与解析曲线点/面积/包围盒对照。
- 容差收紧时折线段数单调不减。
- 解析帧尺寸与真值一致，扫描线方形覆盖面积准确。
- 精细/近似三项指标、关键帧中点覆盖和 `E_SCALE`。
- 全量/断点/增量三条路径逐像素一致和 `E_DIVERGE` 定位。
- 状态机非法迁移保持原状态。
- 单路径单阶段注入失败、状态置 failed、重试后继续提交。
- 已提交帧在参数改变后保持冻结。
- 像素缓冲阻塞等待事件记录。
- Web 诊断状态、帧、暂停/恢复处理。
- 全量测试和 `-race` 竞态检查。

## 设计说明与边界

- 输出是灰度位图；RGB 仅用于亮度转换，不导出彩色 buffer。
- 路径轮廓要求闭合，段类型限定为二次/三次 Bezier。
- 描边针对游戏 UI/装饰图形常见的中等宽度、闭合轮廓设计；极宽描边或强自交轮廓的解析描边面积不设为验收基准，覆盖以子采样栅格为准。
- 解析真值中的轮廓面积与包围盒是精确数学结果；覆盖面积使用独立的高密度确定性采样作为参考，因此会与有限子采样栅格存在小幅误差，并受声明容差约束。
- 相同场景在同一参数、任意 worker 数量和任意三种执行路径下，帧尺寸与像素值均保持确定。

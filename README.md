# 屏幕空间体积光渲染诊断工作台

这是一个不依赖历史代码、不使用 Docker 的 Go 1.22 项目。它从固定种子生成确定性场景与独立真值，执行 CPU 光线步进体积光渲染，用 SQLite 持久化分块缓存，并通过 Wails 提供诊断界面。

核心验收不是“画面看起来正确”，而是以下可复现不变量：

- 真值来自生成器的场景构造与独立参考积分，不读取渲染输出。
- 同一场景、同一有效渲染参数下，线程数、分块调度顺序、缓存是否命中都不改变结果。
- 编辑光源/介质/遮挡几何后，只重算依赖范围内的分块；增量结果与空缓存全量重算逐块、逐像素、逐统计项相同。
- 每批结果经 SQLite 事务和单写锁提交；暂停或强制退出后只恢复版本匹配的已提交批次。
- 预览路径记录实际步长、采样数和可推导误差上界；降质预览的误差必须落在此上界内。

## 快速验证

```bash
# 1. 纯 Go 测试（无需网络、GUI 或数据库服务）
go test -count=1 ./...
go test -count=1 -race ./internal/...

# 2. 生成固定种子样本
go run ./cmd/samplegen -out artifacts/sample

# 3. 完整验收：真值、编辑失效、增量/全量、预览、断点恢复
go run ./cmd/volumediag verify \
  -scene artifacts/sample \
  -db artifacts/verification.db \
  -report artifacts/verification.json
```

本次实际运行记录见 `artifacts/run.log`，明细见 `artifacts/verification.json`。

最近一次结果：

| 检查 | 结果 |
| --- | --- |
| 真值 | 3072 像素 + 12 探针通过；最大像素误差 `1.88578e-05`，最大散射探针误差 `1.37893e-07` |
| 点光强度编辑 | 22/48 块失效，26 块复用，增量=全量 |
| 聚光方向编辑 | 4/48 块失效，44 块复用，增量=全量 |
| 介质参数编辑 | 48/48 块失效，增量=全量 |
| 遮挡深度网格编辑 | 23/48 块失效，25 块复用，增量=全量 |
| 预览 / 全量同参数 | 逐像素一致 |
| 降质预览 | 实测最大差 `1.16188e-04`，推导上界 `1.32514e-01` |
| 暂停/重启/续跑 | 已提交 24 块恢复，补齐 24 块，最终=全量 |

## 环境

本机实测：

```text
Go      go1.22.12 linux/amd64
Wails   v2.10.2
SQLite  modernc.org/sqlite v1.34.5（纯 Go，无 CGO）
GO111MODULE 未设置；Go 1.22 默认模块模式
CGO_ENABLED=1
```

`go.mod` 固定 Go 1.22 与 Wails v2.10.2，不依赖最新版 Wails 所需的 Go 1.26。

模块缓存填充后可离线构建：

```bash
GOPROXY=off go test ./...
```

### Wails 前置条件

Linux：

```bash
sudo apt install pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev
wails build -tags desktop
```

Windows：先安装 WebView2 Runtime，再安装与 Go 版本匹配的 Wails CLI。

当前容器确实缺少 `pkg-config` 与 WebKitGTK，因此桌面链接被环境阻塞：

```text
Generating bindings: Done.
Compiling application: .../assetserver/webview:
exec: "pkg-config": executable file not found in $PATH
```

这不是代码/绑定错误：`go test -tags desktop .` 通过，Wails 绑定也已成功生成到 `frontend/wailsjs/`。安装上述系统包后可直接构建。

## Wails 工作台

```bash
wails dev -tags desktop
# 或
wails build -tags desktop
```

界面启动后自动在可写位置加载/生成 `gui-sample` 与 `gui-checkpoint.db`。主要面板：

- 场景俯视图：深度网格、三类光源、相机轨迹、光源支持半径与方向。
- 深度网格热图：逐单元显示沿法线的深度偏移。
- 体积光颜色缓冲：RGB 热力图，叠加分块状态、光源影响范围、本次失效块。
- 差异热力图：当前体积光与生成器真值的绝对差异。
- 光源编辑器：强度、方向、各向异性；应用后自动触发增量重算。
- 介质编辑器：逐通道消光/散射与相函数基准参数。
- 失效传播：按 `volume(depth 0) → blend(depth 1)` 展示分块坐标、原因、来源光源/遮挡体。
- 分块缓存图：computed / reused / invalidated / failed 状态可点击查看摘要、依赖、耗时、统计与像素缓冲。
- 质量/资源：步长、抖动策略、抖动样本、工作线程、缓冲槽、批大小与检查点批大小。

按钮 `Compare vs full` 会从空缓存重算并定位首个差异的分块、数据项、像素和左右值；`Verify truth` 检查生成器真值；`Pause` 在批次边界停止。

## 目录

```text
cmd/samplegen/             确定性样本生成器
cmd/volumediag/            headless 验收/渲染/检查 CLI
internal/model/            场景、真值、结构校验、内容哈希
internal/scene/            固定种子样本与独立真值生成
internal/volume/           光线步进、阴影、相函数、参考积分、误差上界
internal/engine/           失效传播、批次调度、并发、精确比对
internal/store/            SQLite 清单、批次、分块、历史与串行写入
internal/workbench/        Wails 后端 API
frontend/src/              Wails 诊断界面（无 npm 依赖）
main.go                    Wails 桌面入口（desktop/bindings 构建标签）
docs/DESIGN.md             模型、算法与持久化不变量说明
```

## 场景与真值模型

`scene.json` 包含：

- 深度网格遮挡体：矩形基面、`axis_u/axis_v/normal`、行列深度网格。
- 点光、聚光、面积光：位置、方向、逐通道强度、支持半径、各向异性、遮挡体引用；聚光额外有内外锥角。
- 均匀参与介质：逐通道消光系数、散射系数、HG 相函数基准参数。
- 相机轨迹、活动相机、视锥参数、屏幕分块尺寸。

渲染模型：

- 均匀介质透射率采用闭式 `exp(-sigma_t * t)`。
- 单次散射为 `sigma_s * sum_lights(L_i(P) * HG(cosθ, g_eff) * V_l(P))`，再乘相机到样本的介质透射率。
- 光源使用确定性有界高斯空间核；聚光叠加平滑内外锥；面积光叠加方向余弦项。
- 阴影为样本到光源线段与深度网格平面片的解析求交。
- 每像素抖动使用像素坐标与样本索引的确定性哈希/Halton，不使用全局随机状态。

真值路径 `ReferenceRay` 与渲染路径分离：它使用独立的高精度 Simpson 参考积分和闭式透射率，输入只来自生成器构造的场景，不读取任何渲染缓冲。容差声明在 `truth.json` 中，加载器强制检查活动相机全部屏幕坐标的覆盖和探针射线完整性。

## 失效与缓存语义

每个分块记录：

- 实际参与光源及其版本；
- 介质版本；
- 影响该块的遮挡几何依赖（光源引用 + 基础几何命中）的逐块哈希；
- 步长/抖动参数版本、算法版本、结果摘要；
- 逐像素透射率、单次散射、最终加性混合像素与块统计。

失效范围：

- 光源编辑：旧/新光源影响范围的并集，再与缓存分块依赖求并集。
- 介质编辑：全部活动相机分块。
- 遮挡几何编辑：旧/新逐块几何依赖中包含该网格的分块。
- 每个体积块同时产生一个 downstream `blend` 失效记录，影响深度为 1。

影响范围由集合运算和排序得到，不依赖光源/分块遍历顺序。无法构造覆盖时返回 `E_DEP` 并定位首个无法判定的分块。

调度按 `impact_depth → camera → block_y → block_x` 稳定排序，固定大小分批。批次内的块可并行计算，但 SQLite 提交在单写锁和事务内按稳定顺序执行。缓存替换在批次成功后一次性完成。

## 检查点

SQLite 表：

- `manifest`：源哈希、场景哈希、参数/算法/介质/几何/光源版本。
- `batches`：单调递增批次号、条目数、批次摘要。
- `blocks`：当前分块缓冲、摘要、依赖版本、revision、批次号。
- `block_history`：被显式替换前的旧值。
- `meta`：快照完成标志。

恢复规则：

1. 全局清单任一版本不匹配：整体作废。
2. 分块依赖版本不匹配或摘要损坏：整体作废，禁止部分混用。
3. 仅已提交批次可恢复；未提交批次不会出现。
4. 续跑只计算缺失分块；已提交块直接复用。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_SCHEMA` | 必需字段缺失或结构不合法 |
| `E_REF` | 光源遮挡体/相机等引用不存在 |
| `E_PARA` | 数值非法、步长/抖动/资源参数越界，附参数快照 |
| `E_DECODE` | JSON、深度网格或持久化分块损坏 |
| `E_DEP` | 无法确定分块依赖范围，附首个分块坐标 |
| `E_DIFF` | 增量/全量或真值比较失败，附分块、像素、数据项与左右值 |
| `E_BLOCK` | 单块异常，块失败且不提交该批次 |
| `E_CANCELED` | 批次边界暂停/取消 |

## CLI

```bash
# 渲染
go run ./cmd/volumediag render -scene artifacts/sample -db artifacts/render.db -mode full
go run ./cmd/volumediag render -scene artifacts/sample -db artifacts/render.db -mode preview
go run ./cmd/volumediag render -scene artifacts/sample -db artifacts/render.db -mode resume

# 查看检查点、参数、逐块状态
go run ./cmd/volumediag inspect -scene artifacts/sample -db artifacts/render.db
```

## 已知边界

- 桌面二进制未在当前 Linux 容器完成链接，原因是宿主缺少系统 WebView/GTK 开发包；核心、Wails 绑定与 `desktop` 构建标签测试均已执行。
- 区域光采用有界高斯诊断模型，不是完整面光源积分。
- 参考积分容差针对当前 64×48 样本显式声明；更大视口/更小特征场景需要重新评估容差与步长。
- 本工作台以 CPU 确定性验证为目标，不实现生产级 GPU 阴影贴图或去噪。

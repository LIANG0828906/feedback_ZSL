# Transparent Medium Volume Fog

一个仅使用 Go 标准库实现的透明介质体积雾分块积分内核，以及可本地运行的
HTTP 诊断工作台。场景、体素密度、相机轨迹、积分配方和解析真值都由固定种子
确定性生成；没有第三方依赖、外部服务或容器依赖。

## 快速开始

环境：Linux / WSL Ubuntu，Go 1.22。

```bash
go version
go test ./...
go test -race ./...
go build ./...
```

本地工作台：

```bash
go run ./cmd/workbench -addr 127.0.0.1:18080
# 浏览器打开 http://127.0.0.1:18080/
```

不启动 HTTP、直接输出一份可重复验收摘要：

```bash
go run ./cmd/workbench -once \
  -seed 42 -width 16 -height 12 \
  -data /tmp/vfog-diagnostics \
  -json-out /tmp/vfog-diagnostics.json
```

常用参数：`-seed`、`-width`、`-height`、`-data`、`-addr`、`-once`、
`-json-out`。

## 本地接口

| 路径 | 用途 |
|---|---|
| `/` | 诊断工作台：双路径画面、预算、排队、失效、缓存、三路径与注入结果 |
| `/api/diagnostics` | 完整诊断报告 JSON |
| `/api/render?mode=preview` | 当前场景的预览帧 JSON |
| `/api/render?mode=exact` | 当前场景的精确帧 JSON |
| `/inject?kind=decode`（另有 `order`、`density`、`block`） | 参数化异常注入页面 |
| `/healthz` | 存活检查 |

`/inject` 还接受 `tile=tile-r00-c00`、`media=haze-west`，用于指定单块失败位置。

## 代码结构

```text
fog/
  scene.go, generate.go       场景模型、固定种子生成、装载校验
  math.go, ray.go             固定点量化、相机射线
  intersect.go                AABB 求交、重叠边界切分、体素依赖遍历
  phase.go, integrate.go      相位函数、透射率/散射积分、阴影采样
  budget.go, render.go        分块调度、并行/射线缓冲/阴影预算、双路径
  compare.go                  预览/精确一致判定与 E_DIVERGE
  deps.go                     依赖图、下游遍历、精确失效清单
  cachekey.go                 介质内容/射线/参数/上游缓存键组合
  checkpoint.go               纯 Go 原子事务检查点
  threepath.go                实时、断点恢复、全量重算三路径
  inject.go, diagnostics.go   异常注入与工作台诊断聚合
cmd/workbench/                net/http 本地诊断工作台
```

## 固定口径

### 固定点与区间

- 几何区间端点使用 `Recipe.FixedScale`（默认 `1_000_000`）量化；积分只使用
  量化后的 `int64` 端点。
- 每个分段同时保存原始端点 `RawT0/RawT1` 与量化残差
  `ResidualT0/ResidualT1`，便于审计。
- 吸收/散射系数以 `Recipe.CoefficientScale` 为刻度的 `int64` 存储，源场景
  不保存有损浮点系数。

### 重叠分段规则

射线与每个介质体求得的有效区间先排序，再收集所有区间端点并量化。相邻端点
之间形成一个基础分段，其 `ActiveMedia` 是覆盖该分段全部区间的介质集合。
因此在两介质重叠处会得到 `A`、`A+B`、`B` 三个分段，而不是把 A、B 两个完整
区间的贡献直接相加。

积分时每个分段使用活跃介质的合并消光系数和各自散射/相位源项：

```text
T <- T * exp(-sum(sigma_t(medium, sample)) * h)
L <- L + T * sum(sigma_s * phase(g) * light * visibility) * h
```

主步数按分段长度确定性分配；若配方步数少于必须保留的分段边界数，则每段至少
一步，`Decision.StepsActual` 会如实报告该强制下界。阴影采样在每条射线上受
`ShadowSamples` 上限约束，并在全局阴影采样令牌预算中阻塞排队。

### 收敛与真值

积分采用左端点透射率与左端点源项。均匀介质下，步数增加时估计误差单调下降；
`fog.AnalyticHomogeneous` 提供独立的单介质均匀解析值。生成器内置 4 条真值
射线，测试会将积分结果与真值容差比较。

## 调度与双路径

- 视口按固定屏幕块划分，块内自己生成射线、求交、积分，块间不共享可写渲染态。
- `MaxParallelTiles` 限制并行块，`RayBufferPerWorker` 限制每 worker 的射线
  批大小，`ShadowBudget` 是全局并发阴影采样令牌数。
- 预算耗尽时通过有界 channel 阻塞，记录 `tile_id`、原因、等待毫秒数，而不是
  丢弃射线。
- 预览使用 `PreviewSteps`、`PreviewShadow`；精确路径使用 `Steps`、
  `ShadowSamples`。两条路径记录同一 `MediaSnapshotVersion` 与
  `CameraVersion`，`CompareFrames` 校验覆盖、版本并按像素找出首个超差位置。

## 依赖与精确失效

渲染后保留显式依赖图：

```text
density:<medium>:<voxel> ─┐
coeff:<medium> ───────────┼─> segment:<ray>:<n> ─> pixel:<ray> ─> tile:<id>
camera:<version> ─────────┤
recipe:<version> ─────────┘
```

密度编辑只从具体体素源出发，系数、相机、配方编辑从对应版本源出发。遍历下游
即可得到受影响的射线分段、像素、块与依赖来源；未受影响块沿用已持久化结果。

缓存键链：

```text
mediaDepHash = H(排序后的实际依赖系数源哈希 + 采样体素源哈希)
segmentKey_n = H(mediaDepHash, rayID#n, paramVersion, segmentKey_(n-1))
rayKey       = H(mediaDepHash, rayID, paramVersion, lastSegmentKey)
tileKey      = H(tile 几何, 场景结构版本, 相机/配方/算法/质量版本,
                 介质子集哈希, rayKeys)
```

`mediaDepHash` 使用实际依赖的系数和体素子集，而不是整块介质的全量哈希；这样
编辑一个射线未经过的体素不会误伤其它块。地形/分辨率/优先级变化会进入系数源
哈希。

## 持久化与原子性

任务允许“SQLite 驱动或纯 Go 本地持久化方案”。本项目选择后者，以避免引入
第三方依赖。检查点是一个 append-only 事务日志：

- 每提交一个块写入一行 JSON，包含块像素、完整射线分段、透射率/散射累积、
  射线段摘要、缓存键、依赖源哈希、内容哈希、配方/算法/相机版本。
- 行内包含 magic、事务号、前序校验和与整行校验和；写入在互斥锁内完成并在
  更新内存索引前 `fsync`。
- 未带换行的尾部事务视为未提交；内容哈希、依赖哈希、版本或缓存键不一致时
  整块作废。
- 调用方按块编号稳定提交；重启恢复和一次性全量重算会得到相同提交序列。

## 错误码

| 错误码 | 触发点 |
|---|---|
| `E_DECODE` | JSON 解码失败、密度样本损坏 |
| `E_PARA` | 包围盒、系数范围、配方参数等非法 |
| `E_ORDER` | 相机逻辑时间非严格递增 |
| `E_DENS` | 密度分辨率或体素数与包围盒不相容 |
| `E_BLOCK` | 单块内部失败，携带介质与块坐标 |
| `E_DIVERGE` | 预览/精确或三路径超差，携带首个不一致像素与差异量 |
| `E_BUDGET` / `E_CANCELED` | 预算/取消链路 |
| `E_CACHE` / `E_CHECKPOINT` | 缓存校验或检查点写入失败 |

所有 `*fog.Fault` 都带 `Code`、`Path`、`Actual`，适用时还带 `MediaID`、
`TileID`、`Pixel`。

## 异常注入

工作台和 `RunInjection` 支持：

- `decode`：密度样本越界，走 `E_DECODE`；
- `order`：相机时间倒置，走 `E_ORDER`；
- `density`：分辨率越界，走 `E_DENS`；
- `block`：指定块内部失败，走 `E_BLOCK`，携带介质与块坐标，其余块照常完成。

## 实测验收

本次在 Go `1.22.12 linux/amd64` 上执行。时延是当前 WSL 主机实测值，精度与
一致性结果不依赖具体时延。

```text
$ go test ./... -count=1
ok  volumefog/cmd/workbench
ok  volumefog/fog

$ go test -race ./... -count=1
ok  volumefog/cmd/workbench
ok  volumefog/fog

$ go run ./cmd/workbench -once -seed 42 -width 16 -height 12
preview: steps=14 shadow=1 elapsed=29.479ms limit=120ms met=true
exact: steps=48 shadow=4 tiles=16 elapsed=38.240ms
preview/exact: coverage=true max_diff=0.00470424604 tolerance=0.035 diverged=false
cache: no_edit=16/16 post_edit=12 recomputed=4
invalidation: source=density:calibration:0 tile_count=4 unchanged=12
three-path: pixels=true summaries=true commit=true cache_hits=[0 8 0]
injections: decode=E_DECODE order=E_ORDER density=E_DENS block=E_BLOCK
checkpoint: entries=20 tiles=16 corrupt_lines=0 truncated_tail=false
```

HTTP 冒烟结果：

```text
/                                            HTTP 200
/healthz                                     HTTP 200
/inject?kind=block&tile=tile-r00-c00         HTTP 200
/api/render?mode=preview                     HTTP 200
```

完整可重复命令与输出记录见 `artifacts/verification.txt`。

## 测试覆盖

- 固定种子场景哈希稳定性、JSON 往返、解析真值一致性；
- 包围盒/密度/系数/相机时序错误码、字段路径、实测值；
- 重叠区间切分次序、固定点端点与残差、重复求交摘要稳定；
- 积分精度、步数收紧单调收敛、并发度与块完成顺序无关；
- 阴影预算阻塞与排队记录、并行块/射线缓冲上限；
- 预览/精确覆盖与版本一致、首个像素超差定位；
- 体素与系数编辑的精确失效、未失效块检查点复用；
- 介质结构（包围盒/分辨率/集合）变化时缓存整体拒绝；
- 检查点损坏行隔离、截断尾事务、重启复用、版本失配整体作废；
- 实时/断点恢复/全量重算的像素、射线摘要、提交序列一致；
- 四种异常注入链路与单块失败不污染；
- 工作台模板、HTTP 页面/API/注入接口。

## 已知边界

- 输出为标量辐亮度，便于固定验收口径；不包含 RGB 光谱或多次散射。
- 阴影可见性使用有限 `ShadowDistance` 上的确定性分层采样。
- 单块失败时该块像素标记 `Missing`，其余块仍可提交；失败块不会进入检查点。
- 工作台为本地诊断用途，默认只绑定 `127.0.0.1`。

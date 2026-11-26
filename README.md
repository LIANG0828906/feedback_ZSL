# Volume Kernel

实时透明介质体积光分块积分内核，附带确定性样本、解析真值、SQLite 检查点、失效传播和本地诊断工作台。

本仓库从空 Go 模块开始实现，运行时只使用 Go 标准库、仓库内文件和一个 SQLite 驱动：`modernc.org/sqlite v1.34.5`（纯 Go，无 cgo）。样本生成、渲染、校验和诊断均在本机完成，不依赖外部服务或网络，不使用 Docker。

## Quick Start

环境：Linux / WSL Ubuntu，Go 1.21+。

```bash
go test ./...
go run ./cmd/volume-lab verify
go run ./cmd/volume-lab serve
```

验收命令会生成：

- `artifacts/scene.json`：固定种子的场景、介质、密度场、相机轨迹、配方与光照。
- `artifacts/truth.json`：生成器独立构造的解析真值清单。
- `artifacts/exact.png`：一次性全量精确渲染。
- `artifacts/preview.png`：交互预览帧。
- `artifacts/verification.json`：机器可读的完整验收结果。
- `artifacts/report.md`：人可读的验收摘要。

诊断工作台启动后访问 `http://127.0.0.1:8787`：

```bash
go run ./cmd/volume-lab serve -addr 127.0.0.1:8787 -seed 20261127 -camera 2
```

## Deterministic Sample

默认种子为 `20261127`。生成器使用 `math/rand` 固定种子，重复运行得到相同场景哈希。

- 三个 AABB 透明介质体：`media-alpha`、`media-beta`、`media-gamma`，存在重叠区域。
- 每个介质声明吸收系数、散射系数、Henyey-Greenstein 各向异性参数和整型体素密度场。
- 密度场分辨率分别为 `6×6×5`、`5×6×6`、`6×4×5`，体素值范围 `0..255`，按 `value/255` 归一化。
- 5 个按时间单调不减排列的相机采样点，包含位置、目标、上方向、FOV 和 aspect。
- 配方声明步进次数、相位函数、每步阴影采样数、阴影总预算、分块尺寸、并行度、射线缓冲上限、预览参数和像素容差。
- 默认视口 `48×30`，分块 `8×8`，共 24 个块。

真值由生成器构造，不读取任何渲染输出：

1. 生成器把每条射线与介质内的每个体素单元求交。
2. 以体素边界为原子区间，按射线参数排序。
3. 对每个原子区间，使用生成器自己的闭式透射率、单次散射公式和精确阴影参考值累加。
4. 渲染器只负责采样近似积分；测试与验收把渲染结果同该真值比较。

坐标与参数统一使用 `1e-6` 定点刻度表示区间端点。射线交集端点分别向下/向上量化并记录残差，渲染结果中的 `RaySegment` 保留：

```text
T0Ticks, T1Ticks, T0ResidualTicks, T1ResidualTicks
```

## Architecture

```text
cmd/volume-lab          CLI: verify / serve / render
internal/model          领域类型、哈希、快照、定点量化、统一错误码
internal/generator      固定种子场景、整数密度场、相机轨迹、解析真值
internal/scene          JSON 装载与参数/密度/时序校验、异常注入
internal/camera         相机射线生成
internal/geom           AABB / 体素求交、重叠边界扫描
internal/kernel         固定点区间、重叠分段、步进分配、体积积分
internal/physics        相位函数、阴影参考值与采样估计
internal/pipeline       分块调度、预算门、缓存键、依赖图、失效、双路径判定
internal/checkpoint     SQLite 原子检查点与持久化块缓存
internal/workbench      本地 HTTP 诊断工作台及嵌入式页面
internal/acceptance     端到端验收执行器与报告生成
internal/imageout       PNG 输出
```

## Kernel Semantics

### 1. Scene Validation

`scene.LoadJSON` / `scene.Validate` 检查：

- JSON 截断或格式错误：`E_DECODE`，尽量从已消费前缀恢复介质标识。
- AABB 最小角大于最大角：`E_PARA`，定位 `medium` 和 `bounds`。
- 系数为负、非有限、超出声明刻度或未落在定点网格：`E_PARA`，定位介质和 `absorption` / `scattering`。
- 各向异性不在 `[-0.999,0.999]`：`E_PARA`。
- 密度分辨率、数组长度或体素尺度不相容：`E_DENS`，定位介质和 `density.resolution` / `density.values`。
- 相机时刻倒序：`E_ORDER`，消息包含采样序号和相机 ID。

### 2. Ordered Overlapping Segments

每条射线与所有介质 AABB 求交。所有区间按 `(T0, T1, mediumID)` 排序；扫描边界时，同一原子区间内的所有活动介质按 ID 排序后复合：

```text
sigma_t = sum(density_i * (sigma_a_i + sigma_s_i))
sigma_s = sum(density_i * sigma_s_i)
g_eff   = sum(density_i * sigma_s_i * g_i) / sigma_s
```

因此重叠区不是逐介质简单相加，而是先构造有序原子段，再计算复合介质系数。排序只依赖参数和介质 ID，不依赖介质切片遍历顺序。

### 3. Bounded Integration

配方总步数按原子段长度使用确定性最大余数法分配，每段至少一步。每步在段中点采样：

- 透射率累乘：`T *= exp(-sigma_t * dt)`
- 散射累积：`L += T_before * phase(g_eff, theta) * light * shadow * sigma_s * dt`

相位函数支持：

- `isotropic`
- `henyey-greenstein`

阴影估计使用配方声明数量的分层采样。预算不足时：

- 步数分配不足：`E_BUDGET`，消息含射线 ID、实际需要步数和限制。
- 单射线阴影采样上限：`E_BUDGET`，消息含射线 ID、实际采样数和限制。
- 分块阴影需求超过全局容量：`E_BUDGET` 并定位分块。

测试约束了 `16 → 32 → 64 → 96 → 192` 步的最大误差单调下降。

### 4. Block Scheduling

- 视口按固定 `BlockWidth × BlockHeight` 划分，每块独立生成射线、求交、积分。
- 最大并行块数由 worker pool 限制。
- 每线程射线缓冲上限通过分块内 `MaxRaysPerBlock` 分片实现。
- 全局阴影预算使用条件变量门控。预算不足时 worker 阻塞，不丢弃分块，并记录：

```json
{"block_index": 7, "reason": "shadow budget queue: need=...", "wait_ms": 1.23}
```

- 最终像素按全局 `(y,x)` 排序输出；线程完成顺序不影响结果。

### 5. Preview / Exact Consistency

交互预览走同一快照和相机参数版本，只降低步数与阴影采样数。停止交互后执行全精度渲染并调用 `CompareFrames`：

- 检查 snapshot version 和 camera version。
- 逐像素比较 `Transmittance` 与 `ScatteredRadiance`。
- 超出 `PixelTolerance` 返回 `E_DIVERGE`。
- 错误携带三项实测值：`max_abs_error`、`mean_abs_error`、`over_tolerance_rate`，以及首个不一致像素坐标。

默认验收中预览耗时约 17-27 ms，声明上限为 250 ms。

### 6. Dependency Graph, Cache Keys, Invalidation

依赖图显式记录：

```text
density voxel / absorption / scattering / camera / recipe
    -> ray segment
    -> screen block
    -> pixel
```

缓存键由以下内容共同哈希：

- 块实际触及介质的本地内容哈希；
- 块内射线 ID 摘要；
- 相机版本与配方版本；
- 有效步数/阴影采样等渲染参数；
- 排序后的上游依赖键摘要。

编辑一个体素或散射系数时，从变更节点沿图传播，只失效实际依赖它的块及其像素。未受影响块使用 SQLite 持久化缓存。默认样本中编辑 `media-beta[2,2,2]` 会失效 5/24 个块，其余 19 个块复用缓存。

### 7. SQLite Checkpoints

SQLite 驱动固定为 `modernc.org/sqlite v1.34.5`。`database/sql` 连接池限制为单连接，并对提交加互斥锁，保证事务串行、不交错。

检查点表 `checkpoint_blocks` 每块一行，单一事务写入：

- 稳定递增的 `seq`；
- 分块坐标；
- 射线摘要；
- 缓存键；
- **全会话**内容哈希；
- 配方版本、算法版本；
- 包含逐像素透射率、散射累积和射线段明细的块 JSON。

恢复规则：

- `seq` 必须连续；块提交顺序由分块坐标稳定排序。
- 会话 scene/camera/recipe/algorithm 任一不匹配：删除该会话全部检查点，整体失效。
- 任一行内容哈希或版本不匹配：同样整体失效。
- 断点恢复会新开 SQLite 连接读取已提交块，仅渲染剩余块。

持久化块缓存与检查点分离：

- 检查点是单次渲染会话的一致性记录，任何全局版本变化整体失效。
- 块缓存是内容寻址的跨渲染缓存，允许编辑后复用未受影响块。

### 8. Exceptions and Workbench

`cmd/volume-lab serve` 启动本地诊断工作台，页面完全嵌入 Go 二进制，不需要前端构建或外部资源。

页面展示：

- 三个介质 AABB 的投影预览、相机位置和介质系数；
- 预览图与精确图；
- 每个分块的透射率均值、散射均值、步数、阴影采样数和缓存键；
- 阴影预算排队原因与等待时长；
- 体素编辑后的失效块网格、受影响像素数、缓存删除/命中；
- `E_DECODE`、`E_ORDER`、`E_DENS`、`E_BLOCK` 注入按钮与错误定位日志；
- 预览/精确三项一致性实测值。

HTTP API：

```text
GET  /api/state
GET  /api/render?mode=preview|exact&steps=24&shadow=4
GET  /api/edit?medium=media-beta&x=2&y=2&z=2
GET  /api/inject?kind=decode|order|dens|block
GET  /api/reset
GET  /preview.png?mode=preview|exact
GET  /api/health
```

## Error Codes

| Code | Meaning |
|---|---|
| `E_DECODE` | JSON/介质数据截断或编码损坏 |
| `E_PARA` | 参数越界、非有限、定点刻度不匹配 |
| `E_ORDER` | 相机采样时刻倒置 |
| `E_DENS` | 密度分辨率/数组/体素尺度不相容 |
| `E_BUDGET` | 步进或阴影采样预算不足 |
| `E_BLOCK` | 单块渲染失败 |
| `E_DIVERGE` | 预览与精确路径超出容差 |

## Verification

`go test ./...` 覆盖：

- 生成器固定种子确定性；
- JSON 截断、AABB、系数、密度、相机时序校验；
- 重叠段与介质遍历顺序无关；
- 步数分配总量、最小步数和确定性；
- 步数收紧误差单调下降；
- 渲染相对解析真值容差；
- 全量、实时、重启恢复三路径逐像素一致；
- 预览/精确一致性及 `E_DIVERGE`；
- 体素/散射编辑后的持久化缓存复用；
- 依赖图上游→射线→分块→像素边；
- SQLite 提交顺序、损坏行整体失效、缓存版本保护；
- 单块失败不污染已提交检查点；
- 工作台 HTTP/PNG/异常/编辑端点；
- 完整验收流程。

本次实测结果（见 `artifacts/report.md`）：

| Check | Result |
|---|---|
| Exact paths | `1440 / 1440 / 1440` 像素，逐像素相同 |
| Frame digest | `5aca697a4fbb5ccc`（三路径相同） |
| Preview | deadline met；样本运行约 `22 ms / 250 ms` |
| Preview max / mean error | `1.37774416e-3` / `2.39226249e-4` |
| Preview over-tolerance rate | `0` |
| Step 16 → 192 max error | `1.93354604e-3 → 1.82940457e-4`，单调下降 |
| Checkpoint blocks | `24`，恢复时复用前 `5` 块后继续 |
| Invalidation | 影响 `5/24` 块，`19` 块命中持久化缓存 |
| Queue events | 约 `20` 次阴影预算阻塞记录 |
| Exception chains | `E_DECODE`、`E_ORDER`、`E_DENS`、`E_PARA`、`E_BUDGET`、`E_BLOCK`、`E_DIVERGE` 全部命中 |

重新生成结果：

```bash
go test ./...
go run ./cmd/volume-lab verify -out artifacts
```

## Notes and Model Assumptions

- 物理模型是用于确定性验收的单次散射标定模型：方向光、体素化线性介质、方向相位函数、分层阴影透射估计。
- 解析真值只由生成器构造的体素区间和闭式公式产生，未反推渲染输出。
- 本机诊断台是浏览器界面，页面资源嵌入 Go 二进制，不访问外网；不使用 Docker、容器或外部渲染服务。
- 预览延迟和绝对耗时依赖机器负载；验收同时记录实测毫秒数，deadline 判定以配方声明值为准。

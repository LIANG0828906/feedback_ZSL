# 分块高度场光照烘焙内核

这是一个从空工作区实现的 Go 领域组件与本地诊断工作台，覆盖确定性样本生成、装载校验、分块直射阴影、天空可见度、定点混合辐照、跨块接缝、精确失效、增量调度、SQLite 断点恢复和预算并行。

固定验收样本为 `seed=424242`：9 个 `5x5` 高度分块、8 个天空方向、4 个太阳时刻，共 900 条独立真值点。数据生成与装载校验分离，真值只从生成器几何和参数求值，不读取烘焙输出。

## 快速运行

环境已确认：

- Go `1.22.12`
- Wails CLI `v2.10.2`
- SQLite 驱动 `modernc.org/sqlite v1.34.5`（纯 Go）

```bash
go test ./...
go vet ./...
go test -race ./...

# 生成固定种子样本
go run ./cmd/terrainbake generate -seed 424242

# 独立装载并校验
go run ./cmd/terrainbake validate -sample artifacts/sample-424242.json

# 三路径、真值、失效、接缝、SQLite 重启恢复演示
go run ./cmd/terrainbake demo -seed 424242

# 纯 Go 本地诊断界面（不含容器，默认只监听 loopback）
go run ./cmd/terrainbake serve -seed 424242 -addr 127.0.0.1:8090
```

也可使用 `make test`、`make demo`、`make serve`、`make build`。

`demo` 会重建 `artifacts/demo.sqlite`，并写入 `artifacts/sample-424242.json`。

## 实际验证结果

本机执行结果：

| 检查 | 结果 |
| --- | --- |
| `go test ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test -race ./...` | 通过 |
| `cd wailsapp && go test ./...` | 通过（源码编译） |
| `wails build -skipbindings -clean` | 被系统依赖阻断：缺少 GTK/WebKit 开发包 |

固定种子 `424242` 的演示结果：

```text
seed=424242 chunks=9 truth_points=900
truth: shadow_mismatch=0 max_sky=0 max_irr=0.000509749656 tolerance=0.0025
paths: any_order=true resume_equal=true incremental_equal=true commit_stable=true
resume: loaded=36 reused=36
edit: invalidations=20 incremental_rebaked=20
seams: edges=12 transfers=372 queue_events=3 buffer_batches=40
```

`queue_events` 与等待纳秒数取决于实际线程调度；任务数、顶点缓冲批次数和结果一致性是确定性的。

定点刻度单调性（`seed=5150`）：

```text
fixed_scale=1000    max_irradiance_error=0.00547305435572
fixed_scale=10000   max_irradiance_error=0.000540804000106
fixed_scale=100000  max_irradiance_error=5.48978834634e-05
```

容差收紧时，容差内点数单调不增：

```text
tolerance=0.1       within_tolerance=900
tolerance=0.01      within_tolerance=900
tolerance=0.001     within_tolerance=900
tolerance=0.0001    within_tolerance=36
```

## 目录与集成边界

```text
model/       稳定数据契约、错误码、确定性哈希、共享边构建
generator/   固定种子生成器与独立真值求值器
project/     分层 JSON 解码、缺字段/截断诊断、语义校验
geometry/    双线性高度查询、方向向量、纯函数可见性步进
bake/        单元烘焙、定点累积、缓存、失效、调度、比对、接缝
checkpoint/  SQLite 原子单元检查点与恢复
diagnostic/  控制器、HTTP API、嵌入式诊断前端
cmd/terrainbake/  generate / validate / demo / serve
wailsapp/    独立 Wails v2 桌面壳（复用同一 HTTP Handler）
```

根模块只依赖纯 Go 代码与 SQLite。Wails 放在独立子模块，避免缺少 WebKit 时阻断 `go test ./...`。

## 核心口径

### 样本与装载

- 高度网格坐标由全局整数索引计算，相邻分块在共享边使用完全相同的坐标。
- 分块覆盖校验检查：位于地形边界内、面积和等于地形面积、无正面积重叠。
- 高度数组长度必须等于 `res_x * res_y`。
- 遮挡体引用必须存在且不重复。
- 太阳逻辑时间严格递增、方位角单调不减。
- 共享边两侧高度序列在装载时逐点比较，失配返回 `E_SEAM`。
- 缺字段返回 `E_DECODE`，错误对象带 `chunk_id` 与字段路径；截断 JSON 也返回 `E_DECODE`。

### 光线与跨块

可见性由固定 `shadow_step_budget` 和 `ray_step` 推进。对每个采样点，直射太阳和各天空方向使用同一几何口径。

跨块时：

1. 步进点切换到相邻分块；
2. 对步进线段二分定位交接位置；
3. 分别从两侧分块读取共享边高度；
4. 两侧高度一致才继续，否则返回 `E_SEAM`；
5. 在结果中记录 `Handoff{edge_id, from, to, position, height_from, height_to, source_chunk}`。

对角穿过四块交汇角时，交接点精确吸附到共享角点，避免浮点误差伪造接缝。

直射结果记录步进预算、实际步数、首次遮挡位置和可见性翻转位置。输出是高度场、光源方向、遮挡体与追踪参数的纯函数，与提交顺序、缓存命中和并行度无关。

### 定点辐照

`fixed_scale` 是声明刻度。引擎先量化输入，再用 `int64` 完成权重、辐射、乘法与累积，最后除以固定刻度。每个发生截断的位置写入采样点的 `Truncations`：

```text
weight_fixed       = int64(sky_weight * scale)
radiance_fixed     = int64(sky_radiance * scale)
sky_weighted_fixed = Σ visible ? weight_fixed * radiance_fixed : 0
sky_fixed          = sky_weighted_fixed / Σ weight_fixed
direct_fixed       = sun_radiance_fixed * sin(elevation)_fixed / scale
mixed_fixed        = albedo_fixed * (direct_fixed + sky_fixed) / scale
irradiance         = mixed_fixed / scale
```

天空可见度是可见方向权重和与总权重之比，定点范围必须为 `[0, scale]`。非法负值或超过单通道上限返回 `E_VAL`，并带采样点坐标、世界坐标与通道名。

`IrradianceTolerance` 是默认真值验收阈值。`AnalyzeTruth` 同时提供 `OverTolerance` 与 `WithinTolerance`，容差收紧时 `WithinTolerance` 单调不增。`FixedScaleErrorBound` 给出保守上界，实际测试也验证刻度提升后最大误差下降。

### 依赖、缓存键与失效

每个烘焙单元记录：

- 所有路径实际经过的分块高度哈希；
- 分块反照率哈希与遮挡体引用；
- 全局遮挡体几何哈希；
- 太阳时刻与太阳哈希；
- 天空样本集哈希；
- 共享交接边；
- 上一逻辑时刻缓存键及相邻高度场上游键。

缓存键由分块自身高度哈希、路径邻块高度哈希、太阳时刻/哈希、参数版本、算法版本与排序后的上游缓存键共同决定。

抬升高度时：

- 修改共享边顶点会同步更新两侧重复顶点，保持几何连续；
- 仅失效“自身或多方向射线实际经过被改分块”的缓存单元；
- 同一分块后续太阳时刻因时间上游键变化继续失效；
- 未失效单元直接复用内存与 SQLite 缓存；
- 失效清单带 `chunk_id`、`time`、`reason`、`dependency`。

修改太阳某时刻时，该时刻及后续时刻失效，较早时刻保持复用。

### 增量与全量一致性

调度按太阳时刻波次推进，时刻内按分块并行。每个分块结果计算完成后先持久化，再原子替换内存缓存。规范提交序列按 `(time, chunk_id)` 排序。

引擎提供三种验收路径：

- `BakeFull`：规范顺序、空缓存；
- `BakeFullOrder`：显式任意分块顺序、空缓存；
- `BakeIncremental`：复用未失效缓存，只烘焙缺失/失效单元。

`CompareResults` 逐项比较提交序列、直射阴影、天空可见度、RGB 定点辐照。首个不一致返回 `E_DIFF`，并带分块、时刻、顶点、世界坐标与字段。

`AnalyzeSeams` 比较共享边两侧重复采样点的阴影与天空可见度，聚合来源分块、交接位置和阴影区间；漏传、多传或两侧不一致返回 `E_SEAM` 并带交接边编号。

### SQLite 检查点

`checkpoint.Store` 使用单连接、单写锁和每单元单事务。每个事务写入：

- 分块阴影摘要；
- 完整辐照度缓冲；
- 依赖 JSON；
- 内容哈希与高度哈希；
- 太阳/天空哈希；
- 参数版本与算法版本；
- 单调提交序号。

恢复行为：

- 单元内容哈希或版本不匹配：整体清空快照，返回 `E_SNAP`，不做局部拼接；
- 快照元数据版本不匹配：`Open` 清空并返回 `E_SNAP`；
- 部分提交或强制终止：下次只依据已持久化且通过校验的单元恢复；
- 编辑后正常重启：未失效单元仍在 SQLite 中，可继续复用。

### 预算并行

`ScheduleOptions` 限制：

- 最大工作线程数；
- 每线程一次处理的顶点缓冲上限；
- 待处理分块队列容量。

队列预算耗尽时提交方阻塞等待，不丢弃任务，并记录 `queue_budget_exhausted`、任务 ID 与实际等待纳秒数。调度统计另外记录顶点分批数和实际工作线程数。

## 诊断界面

`go run ./cmd/terrainbake serve` 启动本地 Web 工作台，包含：

- 分块辐照着色；
- 直射阴影点层；
- 天空可见度层；
- 12 条共享交接边标注；
- 当前时刻失效分块高亮；
- 失效原因与依赖来源列表；
- 全量 / 任意顺序 / 增量三路径比对；
- 真值最大天空/辐照误差；
- 队列事件与顶点批次数；
- 点击选择分块顶点，抬升高度后立即增量重烘焙并自动核对。

进程级实测：`/api/state?time=2` 返回 9 个分块、12 条共享边、372 条交接记录且三路径一致；对 `chunk-00-00` 顶点 `(1,1)` 抬升 `3.5` 后返回 20 条失效记录，增量与当前全量结果仍一致。

Wails 桌面壳位于 `wailsapp/`，使用 `AssetServer.Handler` 复用同一诊断 HTTP Handler：

```bash
cd wailsapp
wails build -tags webkit2_41
```

当前机器的 `wails doctor` 显示缺少 `libwebkit`，且 `pkg-config` 找不到 GTK/WebKit，因此桌面二进制链接失败；根模块与诊断 Web 入口不受影响。安装系统开发包后可构建：

```bash
sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev
```

较旧发行版若只有 WebKitGTK 4.0，可安装 `libwebkit2gtk-4.0-dev` 并直接使用 `wails build`。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_DECODE` | JSON 截断、类型错误或必填字段缺失 |
| `E_VAL` | 数值、方向、可见度、通道上限或调度参数非法 |
| `E_SEAM` | 共享边高度、跨块阴影传播或两侧结果不一致 |
| `E_DIFF` | 三路径逐项比对或真值容差失败 |
| `E_SNAP` | SQLite 内容哈希或版本不匹配，快照整体作废 |
| `E_CANCEL` | 调度期间上下文取消 |
| `E_IO` | 文件或 SQLite I/O 失败 |

错误对象包含可用的 `chunk_id`、`field`、`point`、`world`、`channel`、`edge_id`、`time`、`reason`、`dependency` 上下文。

## 测试覆盖

- 生成器同种子逐字节确定性；
- 缺字段、截断、覆盖、数量、遮挡引用、太阳单调、共享边校验；
- 直射阴影与生成器真值逐点一致；
- 天空可见度/辐照定点误差与刻度单调性；
- 任意分块顺序与全量一致；
- 共享边抬升后的增量/全量与接缝一致性；
- 高度编辑只失效实际路径依赖单元；
- 太阳编辑只失效当前及下游时刻；
- 增量、重启恢复、部分检查点恢复与全量一致；
- SQLite 内容哈希/版本不匹配整体作废；
- 最大并行度、顶点缓冲上限与队列阻塞事件；
- 诊断 HTTP 页面、状态 API、抬升触发与三路径比对；
- `E_DIFF`、`E_SEAM`、`E_VAL` 定位字段。

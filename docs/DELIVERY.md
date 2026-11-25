# 体积雾分块积分内核交付说明

## 范围

本目录是一个可独立构建、测试和运行的 Go 领域组件与本地诊断工作台：

- 固定种子确定性生成介质体、整型密度体素、相机轨迹、积分配方和解析真值。
- 射线/AABB 区间求交、定点量化、重叠段边界化与次序校验。
- 受限步进和阴影预算下的透射率、散射累积积分。
- 屏幕分块、并发槽、射线缓冲批次和阴影总预算调度。
- 交互预览与全精度渲染的一致性判定。
- 密度/吸收/散射/相机/配方到射线段和分块的依赖图、缓存键与精确失效。
- 纯 Go `modernc.org/sqlite` 原子检查点、重启作废和三路径比对。
- 异常注入、失败重试、资源/队列/失效展示和本地 HTTP 诊断页。

模块名为 `volumefog`，根包名为 `fog`；桌面诊断入口是 `cmd/workbench`。

## 目录地图

- `errors.go`：`E_SCHEMA`、`E_SEG`、`E_ITER`、`E_DIVERGE`、`E_DIFF`、`E_DECODE`、`E_ORDER`、`E_DENS`、`E_BLOCK`。
- `fixed.go`：声明定点刻度 `FixedScale = 1_000_000`、定点数、向量和 AABB。
- `generate.go`：固定种子生成器与独立解析真值构造。
- `validate.go`：工程装载校验。
- `geometry.go`：射线生成、Slab 求交、区间排序、重叠原子段和 `E_SEG`。
- `material.go`：整型体素采样、HG/各向同性相位函数、精确/预算阴影透射率。
- `integrate.go`：受限步进步进积分、透射率累乘和散射累积。
- `verify.go`：真值容差、`E_ITER` 和单调收敛曲线。
- `tile.go`：分块渲染、射线缓冲批次、段摘要和分块像素。
- `scheduler.go`：并行块上限、缓冲上限、阴影预算闸门、排队记录和块故障注入。
- `preview.go`：预览/精确双路径和 `E_DIVERGE`。
- `graph.go`：依赖图、精确失效传播和分层缓存键。
- `checkpoint.go`：SQLite 单事务检查点、重启哈希/版本校验和作废。
- `paths.go`：实时、断点恢复、全量重算和 `E_DIFF` 定位。
- `codec.go`：介质封套、校验和、损坏输入与 `E_DECODE`。
- `workbench.go`、`server.go`：有状态诊断工作台、JSON API 和单页界面。
- `selfcheck.go`：`-verify` 使用的全链路自检报告。

## 关键口径

### 定点精度

坐标、AABB、射线参数端点和系数均使用 `Fixed = int64`，刻度为 `1e6`。Slab 的浮点解析结果在进入分段结构时立即量化为 `Fixed`，同时在 `SegmentEndpoint.Residual` 记录 `raw - fixed`。后续排序、边界和长度均从定点值恢复。

### 重叠段规则

声明规则为 `atomic-span-linear-composite`：

1. 收集所有介质的进入/离开参数点。
2. 排序并去重，形成原子参数区间。
3. 每个原子区间记录当前活跃介质集合。
4. 重叠区间不按整段长度重复累计，也不把多个区间长度简单相加；在该原子区间内按活跃介质的物理系数线性组合积分。
5. `PriorityKey` 是排序后的介质 ID 组合，用于检测无法解释的重叠顺序。

### 真值独立性

`TruthBundle` 只由 `GenerateSample -> buildTruth -> BuildAnalyticRayTruth` 产出。真值构造会额外沿密度体素边界切分，使每段内密度为常数，然后使用解析指数透射和常系数散射积分；阴影使用独立的精确光学深度遍历。它不读取 `IntegrateRay`、`TileResult`、预览帧或 SQLite 像素，因此不会从渲染输出反推。

### 缓存键

射线键包含：

- 射线 ID 和像素坐标；
- 相机轨迹版本与内容哈希；
- 配方版本与内容哈希；
- 该射线实际依赖介质的 ID、版本和完整内容哈希；
- 步进/阴影预算；
- 算法版本。

分块键由该块全部射线键、分块坐标、配方版本和算法版本再次哈希得到。分块 `content_hash` 只汇总该块实际相交的介质，因此无关介质编辑不会改变未受影响块的哈希或缓存键。

### SQLite 检查点

驱动为 `modernc.org/sqlite`，不使用 CGO。连接池设置 `SetMaxOpenConns(1)`，Store 内另有互斥锁；每个分块的检查点行和提交日志在同一个事务中写入。稳定提交顺序为行优先的 `(y,x)`，并写入 `commit_order`。重启时若 `content_hash`、`recipe_version`、`camera_version`、`algo_version` 或 `cache_key` 不一致，则删除该块检查点和日志并整体重算。

## 常用命令

```bash
go mod download
CGO_ENABLED=0 go build ./...
go test -count=1 ./...
go run ./cmd/workbench -verify
go run ./cmd/workbench -addr 127.0.0.1:8088 -db artifacts/workbench.db
```

浏览器打开 `http://127.0.0.1:8088/`。

常用 API：

- `GET /api/state`：当前工作台快照。
- `GET /api/action?action=preview&steps=18&shadow=42`
- `GET /api/action?action=exact`
- `GET /api/action?action=scheduled`
- `GET /api/action?action=threeway`
- `GET /api/action?action=inject&kind=decode|order|density|block&x=1&y=1`
- `GET /api/action?action=retry&x=1&y=1`
- `GET /api/action?action=editVoxel&medium=fog-a&vx=2&vy=2&vz=3&value=9`
- `GET /api/action?action=editScattering&medium=fog-b&value=0.22`

## 异常定位

所有领域错误均为 `*fog.Error`，JSON 字段包含错误码、`field/value`、`ray_id`、`medium_id`、`medium_pair`、`tile`、`pixel`、`deviation` 和 `differences`。诊断 API 不把注入错误当成 HTTP 500，而是在快照的 `anomaly` 中返回完整 `E_*` 结构，便于页面和自动化同时断言。

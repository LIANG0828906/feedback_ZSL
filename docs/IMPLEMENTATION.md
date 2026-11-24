# 实现说明

模块名：`roomreverb`；Go 指令版本：`go 1.22`；SQLite 驱动：`modernc.org/sqlite v1.29.10`，纯 Go、无 CGO。

## 代码地图

- `cmd/roomreverb/main.go`：`generate`、`validate`、`solve`、`serve`、`verify`、`example-config` 命令。
- `internal/model/model.go`：关卡、真值、路径结果、检查点和统一错误码。
- `internal/sample/generate.go`：固定种子 `42` 的六房间、十七墙段、五门洞、三声源、三听者样本及解析真值。
- `internal/levelio/levelio.go`：JSON Schema 字段检查与结构/几何校验。
- `internal/geom/geom.go`：整型点、矩形、共边、相交、墙段包含等原语。
- `internal/topology/topology.go`：门洞节点图、Dijkstra、房间链/门链/折线重建、穿墙检查。
- `internal/fixedpoint/fixed.go`：`math/big` 十进制定点数、截断记录、定点长度和 `log10`。
- `internal/dsp/dsp.go`：解析真值公式、定点衰减曲线/声压级合成、`E_PREC` 核对。
- `internal/sched/pool.go`：固定工作线程、每线程缓冲上限、阻塞排队等待记录。
- `internal/engine/engine.go`、`run.go`、`invalidation.go`、`edit.go`：状态机、缓存、失效传播、增量合并、恢复和比对。
- `internal/diff/diff.go`：全量/增量结果逐项比较并定位房间与字段。
- `internal/store/sqlite.go`：串行事务、内容哈希、参数/算法版本和整体快照作废。
- `internal/web/server.go`、`internal/web/static/index.html`：`net/http` API 与无外部依赖的本机诊断页。
- `internal/acceptance/acceptance.go`：可重复执行的本地总验收。

## 需求对应

1. **加载与校验**：`levelio.LoadFile`/`Validate` 检查缺失字段并返回 `E_SCHEMA`；矩形内部重叠、门洞越界、坐标越界等返回 `E_GEOM`，`Ref` 为房间对、墙段或点位编号。每个听者在 `truth.listener_rooms` 中必须恰好一次。
2. **连通图与房间链**：房间是隐式节点，门洞连接两侧房间；`topology.Graph.Solve` 在整型坐标上计算定点长度并运行 Dijkstra。折线顶点只可能是声源、门中点和听者；同房间凸矩形保证线段不穿越外墙，另有 `PolylineClear` 做墙段相交复核。不可达返回 `E_UNREACH`，`Ref` 含 `声源/听者`。
3. **衰减与声压级**：曲线为
   `SPL = SourceLevel + 20log10(gain) - 20log10(distanceM) - occlusion·doors + 3log10(1+density)`，
   `Curve[k] = SPL + 2·density·(1-k/(N-1)) - 60·k·dt/RT60`。
   标准档为 6 位小数定点，所有乘除/开方/对数级数均经 `fixedpoint.Engine`，截断写入 `PathResult.truncations`。`dsp.CheckTruth` 在超差时返回 `E_PREC`，详情含 `sample_index` 与 `observed_deviation`。
4. **状态机**：状态为 `pending`、`connectivity_analyzing`、`parameter_solving`、`committed`、`failed`。非法迁移返回 `E_STATE` 且保持原状态；`failed -> connectivity_analyzing` 支持逐房间重试。普通迁移中 `committed` 是终态，冻结参数/门洞/听者结果；编辑已提交房间必须先经显式 `OpenDraft` 建立新参数/几何修订，随后该房间才作为未提交草稿进入求解流程。
5. **精确失效**：`PlanInvalidation` 比较房间拓扑签名和全部声源—听者链签名。参数变化只失效本房间及位于该房间的听者；门洞/几何变化只失效签名或链实际变化的房间与下游听者。清单逐项给出 ID、原因和 `reused`。`CheckScope`/`Engine.ScopeCheck` 对“整关误失效”请求返回 `E_SCOPE`，`Ref` 是首个应复用的房间。
6. **增量一致性**：失效听者由有界池重算，未失效路径和房间检查点直接从 `pathCache`/`roomCache` 复用。`diff.CompareFullResults` 比较提交顺序、房间链、门链、折线、长度、逐采样曲线、SPL、房间参数/门洞快照和听者结果；首个差异返回 `E_DIFF` 及 `room_id`、`field`、`sample_index`。
7. **SQLite 检查点**：`modernc.org/sqlite` 以 `SetMaxOpenConns(1)` 和 `Store.mu` 保证写入串行；每个房间一个事务，含参数快照、该房间相关门洞快照、路径摘要、完整听者结果、SHA-256、参数版本和算法版本。重启时任一哈希/版本/当前关卡快照匹配失败都会清空并整体重算，不混用半套旧结果。
8. **并发与诊断页**：`sched.Pool` 限制 `max_parallelism`，每工作线程队列容量为 `sample_buffer_limit`；全满时阻塞入队并记录 `per_worker_sample_buffer_full`、任务和等待毫秒。页面地址默认 `http://127.0.0.1:8088`，仅监听本机，展示 SVG 平面图、门洞折线、曲线、SPL 条、状态矩阵、失效表、缓存统计、排队事件，并支持参数编辑、增删门洞、移动墙线、重试和比对。

## 配置默认值

见 `configs/example.json`：种子 `42`、标准精度 6 位小数、最大并行度 4、每线程缓冲 64、稳定提交顺序 `R1..R6`、SQLite 路径 `data/workbench.sqlite`、HTTP 地址 `127.0.0.1:8088`。

## Windows 运行

```powershell
winget install GoLang.Go
go version
$env:CGO_ENABLED="0"
go mod download
go build -o .\bin\roomreverb.exe .\cmd\roomreverb
.\bin\roomreverb.exe example-config
.\bin\roomreverb.exe generate
.\bin\roomreverb.exe verify
.\bin\roomreverb.exe solve
.\bin\roomreverb.exe serve
```

仓库内也附带已交叉编译验证的 `bin/roomreverb.exe`（Windows amd64，纯 Go、无 CGO）。

浏览器打开 `http://127.0.0.1:8088`。不需要 Docker、CGO、外部服务或网络数据。

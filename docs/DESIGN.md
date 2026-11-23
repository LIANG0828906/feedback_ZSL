# 遮挡剔除与场景流式加载诊断工作台 · 技术说明

## 1. 模块结构

```
cmd/generator/   确定性样本生成器入口（固定种子落盘 world.json）
cmd/server/      net/http 诊断工作台入口
internal/
  core/          错误码（E_SCHEMA…E_DIVERGE）、Vec3/AABB 几何基础
  scene/         工程数据模型、加载与结构校验、确定性 JSON 落盘
  generator/     固定种子场景生成 + 独立射线-AABB 真值与屏幕投影真值
  culling/       视锥平面提取、包围盒量化、覆盖栅格、纯函数剔除引擎（串行/并行）
  streaming/     窗口状态机、预算校验、确定性加载/卸载调度、窗口冻结
  worker/        有界工作线程池（最大并行度 + 每线程栅格缓冲上限 + 取消）
  persist/       内容/轨迹指纹、SQLite 串行原子检查点、版本识别与整体作废
  workbench/     实时/恢复/全量三路径编排、E_DIVERGE 自动比对、工作台门面
  httpapi/       本地页面与 JSON API
```

## 2. 关键设计如何对应业务要求

### 2.1 工程加载与校验（要求 1）
`scene.World.Validate` 区分三类错误并带定位字段：
- 字段缺失/结构问题 → `E_SCHEMA`；
- 包围盒非有限或最小角大于最大角 → `E_BOX`，附 `partition` 与 `chunk`；
- 遮挡体/分块引用越界 → `E_REF`，附 `partition`、`chunk`、`ref`；
- 相机轨迹时刻必须严格单调递增，相机参数越界返回 `E_PARA`。

### 2.2 纯函数剔除核心（要求 2）
`culling.Engine` 对相同 `(World, Params)` 永远产出相同结果，不读取线程标识、
完成顺序或历史状态。单采样判定流程固定为：
相机内外参 → `ExtractFrustum` 六个几何平面 → 包围盒视锥测试 →
遮挡体量化投影到覆盖栅格（最近深度 + owner）→ 分块整体遮挡判定。
参数越界由 `Params.Validate` 统一返回 `E_PARA` 并指出字段名。

### 2.3 预览 / 精确双路径（要求 3）
两条路径只通过 `culling.Params` 区分：
- 预览：`GridResolution=16`、`SampleStride=4`，记录栅格分辨率与抽样比；
- 精确：`GridResolution=64`、`SampleStride=1`，覆盖全部采样时刻。
二者调用同一个 `ExtractFrustum` 与同一套 `QuantPrecision` 量化角点，
预览结果记录在 `PreviewMeta`，且预览可见集合是精确结果的保守子集（有测试守护）。

### 2.4 窗口状态机（要求 4）
五状态 `pending → culling → loading → committed`，任意阶段可转 `failed`，
`failed` 可重试回 `pending`。非法迁移返回 `E_STATE` 且保持原状态；
`committed` 为终态。提交时 `Window.Freeze` 冻结可见集合、加载清单、
驻留分块与栅格摘要并计算内容哈希，后续参数变化只影响未提交窗口。

### 2.5 优先级调度与预算（要求 5）
`streaming.Scheduler.Plan` 确定性地：先卸载不再可见的驻留分块，
再按 `距离升序 → 加载成本升序 → ID 升序` 加载“可见且未驻留”分块；
未可见分块绝不进入候选，不抢占槽位。槽位或内存耗尽时分块进入
`Delayed`（注明 `slot-exhausted` / `memory-exhausted`），不静默丢弃。
非法槽位/预算返回 `E_BUDGET`。

### 2.6 检查点与版本识别（要求 6）
`persist.Store` 用单连接 + 互斥锁串行化所有写入，每窗口在独立事务内原子提交，
并强制 `window_index` 严格连续，杜绝交错与乱序。身份 `persist.Identity` 覆盖
内容哈希、轨迹指纹、量化/算法版本、栅格分辨率、抽样步长、槽位、内存、窗口大小。
重启时任一字段不匹配即 `Reset` 整体作废，随后全量重算，绝不部分混用旧窗口。

### 2.7 三路径一致性与 E_DIVERGE（要求 7）
`workbench.VerifyThreePaths` 运行：
- `replay`：一次性全量重放；
- `realtime`：逐窗口推进并逐窗写 SQLite；
- `resume`：身份校验后从已提交窗口继续，失配则整体作废重算。
`Compare` 逐窗口比对可见集合、遮挡集合、加载/卸载/驻留序列与延迟记录，
首个不一致返回 `E_DIVERGE`，定位窗口编号、分块标识与差异字段。

### 2.8 工作台、并发边界与异常链路（要求 8）
- `net/http` 页面展示分块着色、遮挡网格、相机轨迹、窗口状态矩阵、
  槽位/驻留/延迟、缓存命中与三路径比对，支持调参、重试、导出。
- `worker.Pool` 限制最大并行度与每线程栅格缓冲上限，支持 `context` 取消；
  并行剔除按固定索引回填，测试证明与串行结果逐采样深度一致。
- SQLite 写入在存储层串行化；单窗口异常仅置该窗口 `failed`，
  不污染其他窗口，可经 `RetryWindow` 重试。
- 进程被强制终止后，重启只依据已持久化且身份匹配的窗口恢复（有重启测试）。

## 3. 真值与引擎为何能精确对齐

生成器真值是与剔除引擎相互独立的实现：射线-AABB slab 求交 + 视锥平面测试。
为消除两类实现的系统性偏差，做了两点约束：
1. 视锥阶段两实现使用相同的 tan 形式几何平面，差异只保留在遮挡判定；
2. 射线真值在分块盒的投影子矩形内自适应采样，避免窄缝分块被固定网格漏采样；
   覆盖栅格采用格子中心采样并裁剪屏外区域，避免屏外足迹被外扩误判。
测试 `TestPreciseCullingMatchesIndependentGroundTruth` 逐时刻断言两实现可见集合一致。

## 4. 错误码一览

| 码 | 触发场景 |
| --- | --- |
| `E_SCHEMA` | 字段缺失、结构不合法、轨迹时刻非单调 |
| `E_BOX` | 包围盒非有限或倒置（定位分区/分块） |
| `E_REF` | 分块/遮挡体/真值引用越界 |
| `E_PARA` | 剔除、量化、线程池参数越界（指出字段名） |
| `E_STATE` | 非法窗口状态迁移、乱序提交 |
| `E_BUDGET` | 槽位/内存预算非法 |
| `E_DIVERGE` | 三路径逐窗口结果不一致（附定位） |
| `E_IO` | 本地文件或 SQLite 错误 |

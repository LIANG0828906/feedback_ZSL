# 掩码质检工作台技术说明

本仓库仅使用 Go 标准库，模块名为 `maskqc`。根包是可集成的领域组件，`cmd/workbench` 是控制台 + 本地 HTTP 诊断工作台，`cmd/review` 是独立报告复核工具。

## 运行

```bash
go test ./...
go vet ./...
go run ./cmd/workbench             # 非交互环境输出报告；TTY 中可选择启动界面
go run ./cmd/workbench --selftest  # 全量确定性验收后退出
go run ./cmd/workbench --serve     # http://127.0.0.1:8765
go run ./cmd/review \
  -a artifacts/workbench/path-live.json \
  -b artifacts/workbench/path-recovery.json
```

工作台会写出：

- `artifacts/workbench/diagnostics.json`：完整诊断报告。
- `artifacts/workbench/path-live.json`
- `artifacts/workbench/path-recovery.json`
- `artifacts/workbench/path-full.json`
- `artifacts/workbench/checkpoint/`：纯 Go 事务日志与检查点。

## 领域结构

- `model.go`：工程、笔画、分组、真值、追踪记录、降级清单与 `E_*` 错误包络。
- `schema.go`：`mask-qc.project/v1|v2|v3` 探测、必填字段路径、版本映射、显式导出、转换痕迹和降级 sidecar。
- `convert.go`：序号锚点坐标、千分比/万分比/百分比整型换算、余数、溢出检查和参数范围。
- `sample.go`：固定 seed 的 splitmix64 生成器；产出多分辨率 RGBA 图标、缺陷候选掩码、笔画记录、参考掩码和真值。
- `raster.go`：整数点线段距离、硬度衰减、权重覆盖、source-over alpha 合成、分块网格和确定性次序。
- `topo.go`：8 邻域前景连通域、4 邻域背景孔洞、重心/面积/外接框/环宽、边界段和方向分类、`E_TOP` 比对。
- `graph.go`：依赖图、缓存键组成和编辑失效传播。
- `checkpoint.go`：append-only、SHA-256 校验、fsync 的事务日志，以及四态状态机。
- `scheduler.go`：固定 worker、像素缓冲上限和有界分块队列；队列满时阻塞并记录等待时间。
- `engine.go`：三路径共用管线、稳定提交顺序、持久化复用、结果比对和首个分块/字段差异定位。
- `workbench.go`：样本、异常码、三路径、编辑序列、预算演示和验收报告编排。

## 三版本 Schema 差异

- v1：`construction.lines[points=[x1,y1,x2,y2]]` 绝对坐标数组；`weight_permille` 千分比；无硬度字段，升级补 `50`；`direction_code` 整数编码；分组为 `construction.passes[line_ids]` 引用结构。
- v2：`layers[].segments[]` 嵌套笔画；`fromOffset/toOffset` 为按 `seq` 和 `CoordinateRule{StepX:8,StepY:6}` 推导的锚点偏移；`weightPercent` 百分比；有 `hardness`；方向为 `T/A/O/D/X` 短码；分组即嵌套 layer。
- v3：扁平 `strokes[].start/end` 绝对点对象；`weight_myriad` 万分比；`hardness_pct`；规范方向字符串；`stroke_groups` 显式分组。

所有 JSON 数字通过 `json.Decoder.UseNumber` 解析为整数。缺失字段返回 `E_SCHEMA`，错误包含 `path`、`object_id` 和字段名。

## 整型往返与损失报告

权重换算使用：

```text
permille = floor(myriad / 10)
remainder = myriad mod 10
myriad = permille * 10 + remainder
```

坐标使用 `anchor=(seq*8, seq*6)`，偏移为 `absolute-anchor`，恢复为 `offset+anchor`。所有乘法/加法经 `CheckedMul64`、`CheckedAdd64` 检查；溢出返回 `E_OVF`，其中含字段和两个操作数。

导出 v1 时，无法原生表达的 `mixed` 方向类别和富分组 `kind` 会形成 `E_LOSSY` 项，记录处置方式与 `x_maskqc.sidecar` 键。精确权重余数和原始值写入审计扩展 `x_maskqc.weight_remainders` / `x_maskqc.weight_myriad`，因此对应版本加载器可恢复等价内部模型；每条补齐、重命名、截断、余数和 sidecar 恢复均进入 `TraceEntry`。

## 栅格、次序与拓扑

笔画在所属分块内按整数倍坐标和平方距离判定内/外半径，覆盖值为 `0..255`，随后执行：

```text
out_a = src_a + dst_a * (255 - src_a) / 255
```

合成次序固定为 `priority` 升序、相同优先级按 `seq` 升序。每个分块的 `OrderDecision` 记录参与笔画和 `priority/seq` 依据。并行只影响计算时机；提交前按分块 ID 升序、分块内按裁决顺序串行提交。

拓扑统计包括面积、千分之一像素定点重心、外接框、孔洞数和：

```text
ring_width_1000 = 2 * area * 1000 / boundary_pixel_count
```

方向分类使用法向量与笔画向量的整数点积平方阈值，不使用浮点三角函数。超差返回 `E_TOP`，`details[]` 给域编号、核验项、期望值和实际值。

## 缓存与精确失效

分块缓存键由以下内容确定性哈希得到：

- 候选分块输入哈希；
- 笔画 ID、序号和笔画参数内容哈希；
- `order_version=priority-seq-v1`；
- `convert_version=int-convert-v1`；
- 所有笔画分量键形成的 `upstream_key`。

编辑旧笔画时取旧/新笔画几何影响分块的并集；插入时取新笔画影响集。失效项包含分块编号、触发笔画、变更类型和传播路径，例如：

```text
stroke:s1 -> chunk:2:coverage -> verification:topology -> verification:direction
```

未进入失效集的分块从 `ChunkResultRecord` 恢复，不重新栅格化。

## 检查点与三路径

Go 1.21+ 标准库没有 `sqlite3` 驱动或 `database/sql` 的内置驱动。题目环境准备明确允许“纯 Go 的文件型存储”，因此当前实现使用仓库内 `Store`：

- `checkpoint.txlog` 为 JSON 事务行；
- 每行 envelope 含单调 `seq` 与 payload SHA-256；
- 每个笔画-分块分量在一次加锁写入、一次 `write + fsync` 中提交；
- 记录包含分量字节、`cache_key`、`content_hash`、`residual_digest`、`order_version` 和 `algorithm_version`；
- 重放时校验校验和，尾部撕裂行截断到最后有效事务边界。

状态机为：

```text
pending -> processing -> committed
                      \-> failed -> processing
committed -> processing  # 仅用于新一代失效重算
```

其他迁移返回 `E_STATE` 且保持原状态。

三条验证路径：

1. `live-incremental`：调度器并行计算并持久化；
2. `checkpoint-recovery`：在固定分量提交数后模拟强杀，重新打开日志并从最后提交前缀恢复；
3. `one-shot-full`：全新存储全量重算。

`CompareResults` 先比较分块掩码 SHA，不一致时下钻到首个像素，再比较 `cache_key`、笔画序列、提交序列和域统计；`cmd/review` 输出首个不一致分块编号与字段名。

## 受限调度与注入

`SchedulerOptions` 控制：

- `Parallelism`：worker 数；
- `PixelBufferCap`：单任务像素缓冲上限，超出返回 `E_PARA`；
- `QueueBudget`：待处理任务 channel 容量。

队列满时提交方阻塞，任务不丢弃，并记录 `queue-budget-exhausted`、队列长度、预算和等待纳秒。

注入链路：

- `Injection.CandidateSizeMismatch` -> `E_SIZE`，定位分块和首笔画；
- `Injection.OutOfRangeStrokeID` -> `E_RANGE`，定位笔画和影响分块；
- `Injection.FailEnabled/FailChunkID/FailStrokeID` -> `E_BLOCK`，分块覆盖状态转 `failed`。

## 实际验证结果

最终一轮结果见 `artifacts/workbench/diagnostics.json`。当前实现已实际执行：

```bash
go test ./...
go test -race ./...
go vet ./...
go run ./cmd/workbench --selftest
go run ./cmd/review -a artifacts/workbench/path-live.json -b artifacts/workbench/path-recovery.json
go run ./cmd/review -a artifacts/workbench/path-live.json -b artifacts/workbench/path-full.json
```

验收报告中：

- 3 个固定种子样本均完成 v1/v2/v3 导出与读回；
- live、recovery、full 的分块像素、域统计和提交序列一致；
- 编辑序列的计划失效集与实际重算集一致，未受影响分块复用持久化结果；
- `E_SCHEMA/E_LOSSY/E_OVF/E_PARA/E_TOP/E_STATE/E_SIZE/E_RANGE/E_BLOCK` 均有确定性检查；
- HTTP `/`、`/api/report`、`/mask.png` 已用 `curl` 验证。

候选掩码按题意刻意带内缩、外溢和小空洞，因此样本的拓扑核验会稳定产生 `E_TOP` 诊断；这是被质检对象的预期发现，不是工作台运行失败。工作台自身的 `AcceptanceOK=true` 表示三路径、失效传播和异常码链路均符合验收。

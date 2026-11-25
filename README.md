# 分块早期反射声学能量场内核

这是一个面向平面关卡的独立 Go 领域组件，用于在旧版整关混响预计算退役前，验证“分块早期反射能量场 + 增量重建 + 可恢复检查点”的替代方案。项目从空目录初始化，输入完全由固定种子生成，不依赖样本文件、容器或第三方网络服务。

## 环境与状态

- Go：`go1.22.12 linux/amd64`
- SQLite：纯 Go 驱动 `modernc.org/sqlite v1.34.5`，已固定在 `go.mod`
- 测试：`go test ./... -count=1` 通过
- 演示：`go run ./cmd/acoustic demo` 通过三路径比对与独立复核
- 本地计划：WSL Ubuntu / 普通 Linux 均可，使用单调时钟与临时目录，不依赖 Docker

## 快速开始

```bash
go version
go test ./... -count=1

# 生成固定样本，执行全量、增量、取消恢复三路径并自动比对
go run ./cmd/acoustic demo -seed 20261126 -workers 4 -abort-after 25

# 启动诊断页（默认 http://127.0.0.1:8080）
go run ./cmd/acoustic serve -seed 20261126 -db /tmp/acoustic-demo.db

# 从 SQLite 已提交分块独立复核
go run ./cmd/acoustic verify -seed 20261126 -db /tmp/acoustic-demo.db
```

也可以使用 Makefile：

```bash
make test
make demo
make serve
```

`demo` 会在临时目录生成独立数据库，并输出：

- 三路径的 `round`、提交块数、缓存命中数、重算数、最大守恒残差
- 完整分块提交序列摘要
- 编辑失效清单（分块、传播距离、原因）
- `three-path compare: MATCH`
- 独立复核的字段指纹与提交序列指纹

## 核心行为

默认生成器使用 6×5 规则网格、3 个频带（125/500/2000 Hz）、每块 1 个声源与 1 个听者、最多 2 次反射、3 轮有限传播。所有输入与真值仅由 `seed` 决定。

### 坐标、定点与能量

- 坐标和路径长度：整数微米（`int64`），距离用整数平方根。
- 吸收系数与法线：Q16 定点（`65536 = 1.0`）。
- 能量：抽象整数能量单位。
- 数值域保护：坐标绝对值不超过 `1e9` 微米，单频带发射能量不超过 `1e12`，保证面积、距离和定点乘法不溢出 `int64`。
- 所有传播拆分、路径权重和最大余数分配均使用整数运算；每次定点乘法和拆分记录 `Quantization`/`QuantizationResidual`。
- 传播轮次上限造成的剩余能量记录为 `truncation` 逃逸，而不是静默丢失。

### 分块路径与能量

每块用镜像源法展开最多 `MaxReflections` 次轴对齐或任意法线的线段反射面。路径有效性检查包括：

- 反射点位于反射线段上；
- 折叠后的每个顶点位于分块包围盒内；
- 路径长度按整数微米逐段累加。

分块能量先由有限波次传播得到，再按路径权重分配到路径：

```text
weight(path, band) = absorption[band]^reflections * distance_gain(path)
captured_fraction   = 1 - absorption[band]
```

同一块、同一参数下的路径集合、逐频带能量和边界记录与线程数、完成顺序、缓存命中无关。

### 跨块传递与守恒

每个波次把未捕获的余量按固定顺序精确拆成四向份额。内部邻居产生 `Transfer`，关卡边界或轮次上限产生 `Escape`。每条传递记录包含：

- 来源声源、来源块、目标块；
- 边界侧、边界位置、边界长度；
- 波次、逐频带能量、量化残差和稳定版本摘要。

守恒式按声源/频带检查：

```text
emission = Σ chunk_energy + boundary_net_flow + escape
```

内部边界净流量在全局求和为零，但仍逐记录保存并纳入检查。超过声明残差返回 `E_CONS`，错误中包含频带、源标识及各能量项。

### 状态机

分块状态：

```text
pending_filtering -> path_filtering -> energy_transfer -> committed
                          |                  |
                          +-------> failed <--+
failed -> path_filtering  (显式重试)
committed -> pending_filtering (仅失效操作)
```

重建回合状态：

```text
pending_rebuild -> rebuilding -> committed
                        |
                        +------> failed -> rebuilding
```

非法迁移返回 `E_STATE`，原状态和进度保持不变。已提交分块的路径、能量和传递载荷在下次显式失效前不可变。

### 增量失效与缓存键

编辑吸收系数、反射面几何、声源发射能量或边界传递记录时，引擎沿可传播边界半径标记真实下游块。失效清单记录：

- 分块编号；
- 距编辑源的距离；
- `absorption_changed`、`reflector_geometry_changed`、`source_emission_changed`、`boundary_transfer_changed`、`downstream_of_chunk` 等原因。

块缓存键由以下内容共同决定：

- 分块内容哈希（边界、吸收表、反射面）；
- 参数版本；
- 算法版本；
- 本地声源发射摘要；
- 所有入边传递版本及其上游内容/传递缓存标识。

未失效块复用持久化结果；失效块按边界强连通分量（SCC）的拓扑序、块编号次序重算并原子替换。网格内部存在环，因此“拓扑序”是 SCC 缩略图拓扑序，环内使用稳定块号。

### SQLite 检查点与恢复

数据库启用 WAL、`synchronous=FULL`、外键和单连接串行写入。每提交一个分块，路径摘要、逐频带能量、入/出边界记录、依赖、内容哈希、参数版本、算法版本和缓存键在单一事务内写入；提交序号必须连续，禁止交错。

恢复规则：

- 只加载 `chunk_results` 中已提交的块；
- 任一块内容哈希、参数版本、算法版本或路径摘要不匹配，整体快照作废；
- 编辑失效会在同一事务中删除受影响块并更新关卡/真值身份；
- 未提交块既不恢复，也不写入失效清单；
- 强制终止或取消后，新进程从已提交块继续，后续仍走同一稳定提交顺序。

### 独立复核与三路径一致性

`VerifySnapshot` 是独立复核路径，只读取 SQLite 中的已提交分块、边界传递记录和生成器真值指纹，不调用 `SolveChunk` 或能量传播主路径。它检查：

- 每个分块的路径摘要与长度；
- 从 `SourceEnergy` 重算的分块逐频带能量；
- 与真值的逐块、逐频带、逐听者总能量；
- 入/出边界记录配对、逃逸能量与守恒残差；
- 连续提交序列。

`CompareRunReports` 比较全量、增量、恢复三条路径，按稳定块号、频带、字段定位首个差异；任何差异返回 `E_DIFF`。

## 错误码

| 码 | 场景 | 关键上下文 |
|---|---|---|
| `E_SCHEMA` | 必填字段缺失、引用缺失 | 对象 ID、字段路径 |
| `E_VAL` | 数值/几何越界 | 实测值、合法区间 |
| `E_CONS` | 逐源逐频带守恒超限 | 频带、源、发射/块能量/边界净流/逃逸 |
| `E_STATE` | 非法状态迁移或提交顺序 | 原状态、目标状态、进度 |
| `E_DIFF` | 持久化/并行路径/真值不一致 | 首个分块、频带、字段、路径 |

## 并行预算与诊断

工作线程池限制：

- `MaxParallel`：最大并行度；
- `QueueBudget`：待处理分块队列容量；
- `PathBufferPerWorker`：每次工作线程求解的路径缓冲上限。

队列预算耗尽时提交阻塞，并记录 `queue_budget_exhausted` 与等待时长；单块异常/panic 被隔离为 `failed`，其他块继续提交，下一次重建会重试失败块。

诊断页展示：

- 分块×频带能量热力；
- 边界传递流与前 80 条明细；
- 失效传播块、距离和原因；
- 守恒逐项结果；
- 缓存命中、重算数、排队等待；
- 增量/全量比对状态；
- JSON API：`/api/report`、`/api/state`。

## 代码结构

```text
config.go            参数与版本
model.go             领域模型、路径、传递、真值
fixed.go             Q16、整数距离、镜像与求交
generator.go         固定种子样本与独立解析真值
validate.go          JSON 字段存在性、工程/几何/数值校验
solver.go            波次能量传播、路径枚举、能量分配、守恒
state.go             双层状态机
order.go             边界 SCC 缩略图拓扑序
hash.go              内容/路径/缓存/快照摘要
store.go             SQLite 原子检查点与恢复校验
engine.go            失效、增量调度、并行提交、三路径比较入口
verify.go            独立复核与运行间首个差异
server.go            net/http 诊断页/API
cmd/acoustic/        本地 CLI
```

## 设计边界

- 当前首个版本针对平面关卡和早期反射；不处理三维体积、衍射、空气吸收或频域滤波。
- 能量传播是确定性有限轮次模型；轮次截断明确计入逃逸能量。
- 边界传递记录编辑用于改变下游依赖版本并触发保守失效，能量仍由守恒传播重算，不允许注入破坏守恒的孤立能量。
- 生成样本的反射面为轴对齐；加载文档和路径展开支持任意单位法线线段，并在测试中覆盖旋转反射面。

更详细的公式、事务边界和验收记录见 [docs/architecture.md](docs/architecture.md)。

本次本机运行的原始输出保存在：

- `artifacts/test-results.txt`
- `artifacts/demo-results.txt`
- `artifacts/verification-summary.txt`

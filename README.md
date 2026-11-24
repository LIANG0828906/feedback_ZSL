# Resource Economy Workbench

面向开放世界 / 模拟经营题材的确定性资源生产与经济调度内核，附带本地样本生成器、SQLite 检查点、三路一致性验证和可视化编辑工作台。

项目不依赖容器、网络账号或外部服务。样本、真值和演示数据库都在本地生成。

## 快速开始

环境要求：

- Go 1.22+
- Windows 10+、Linux 或 macOS
- SQLite CLI 可选；Go 侧使用纯 Go 驱动 `modernc.org/sqlite v1.34.5`

```bash
go mod download
go test ./...

# 生成固定种子样本（可重复执行，输出字节一致）
go run ./cmd/samplegen -out data/samples -seed 1592639710

# 启动本地工作台
go run ./cmd/workbench
# http://127.0.0.1:8787

# 三路一致性 / 检查点 / 并发端序验收，并写出报告
go run ./cmd/workbench -verify
```

工作台默认监听 `127.0.0.1:8787`，样本目录 `data/samples`，验收工作目录 `data/verification`。

## 已交付能力

### 确定性样本

生成器按固定种子提供三组场景：

| 样本 | 节点 | 意图 | 逻辑时长 | 真值采样 |
|---|---:|---:|---:|---:|
| `frontier_small` | 10 | 240 | 720 ticks | 11 |
| `river_medium` | 10 | 3,500 | 1,440 ticks | 17 |
| `industry_large` | 10 | 24,000 | 2,400 ticks | 21 |

每个节点包含类型、父子层级、初始等级/工人、基础产出、投入、容量、初始库存和升级定义。每个意图带 `seq`、逻辑时间戳、类型、目标节点及类型负载。

场景中的节点全部预声明；`build` 激活预声明但未启用的节点，`demolish` 停用节点，`upgrade` 进入下一连续等级，`recipe` 原子替换完整配方，`assign_workers` 调整在岗工人。为异常链路测试额外支持显式 `consume` 意图。

样本随机性只来自：

```text
SHA-256(game_seed || node_id || purpose) -> SplitMix64 子随机源
```

因此同一固定种子重复生成会得到字节级一致的 `manifest.json` 和样本文件，不依赖 Go `math/rand` 的版本内实现。

### 结构校验

加载样本时校验：

- 必填字段、类型、资源名
- 节点 ID 唯一、父节点存在、生产链无环
- 速率/容量/库存/意图负载为非负定点整型
- 意图时间戳单调、目标节点存在
- 升级等级连续、工人数量在合法范围
- 真值采样时刻与快照数量、时刻一致
- `scene_hash` 与内容一致

损坏 JSON 返回 `E_DECODE`。缺字段、成环、未知引用等返回 `E_SCHEMA`，并带 `path`、`node_id`、`intent_id` 等定位字段。

### 定点经济结算

- 所有速率、容量、库存和净速率使用 `int64`。
- 单位固定为 `1/1000` 资源单位；时间单位为一个逻辑 tick（生成样本中 `100ms`）。
- 每个 tick 先原子消费该时刻意图，再按父子拓扑顺序结算产出/投入。
- 投入不足或工人不足时节点 `stalled=true`，首次停滞时刻写入 `stall_since`。
- 产出超出容量时截断，累计记录 `node_id + resource` 溢出来源、总量及最后发生时刻。
- 升级意图一次替换等级、产出、投入、容量和所需工人；失败返回 `E_STATE` 并回滚整步。
- 节点自身库存不足时会沿父链取投入；任一投入资源不足则本 tick 不部分消费、不产出。

### 粗略预览与精确结算

实时编辑路径使用较大发布步长（默认 8 ticks）只发布粗粒度曲线；内部仍使用与精确结算相同的单 tick 确定性积分单元。这样：

- UI 只收到较少的曲线点；
- 预览和精确路径使用相同意图顺序和定点单位；
- 预览的最终状态和采样快照可与精确结算逐字段一致；
- 预览记录 `step`、`cutoff_tick` 和每个点所属时刻。

这是一项有意的保守设计：不在粗步长上做可能失真的容量/投入近似。业务上的“粗略”体现为稀疏预览采样，而不是牺牲最终一致性。

### 三路一致性验证

`verify` 包对每组样本比较：

1. 一次性全量结算
2. 固定窗口增量结算（SQLite 提交）
3. 中途退出、重新打开 SQLite 后从最后已提交窗口恢复

比较内容包含节点等级、工人、停滞起始、库存、容量、净速率、累计投入/产出、全局库存、全局净速率、溢出总量与来源。

首个差异返回 `E_DIVERGE`，包含：

- `tick`：逻辑时刻
- `node_id`：节点
- `field`：字段路径
- `left` / `right`：两侧取值

### SQLite 检查点

窗口默认 64 ticks。每个成功窗口在单个 SQLite 事务中写入：

- 场景 ID
- 窗口序号、起止 tick
- 窗口起止意图消费偏移
- 完整状态 JSON 与状态哈希
- 定点参数版本
- 场景内容哈希
- 算法版本

写入路径由进程内互斥锁串行化，并校验“上一窗口末尾 = 本窗口开头”，数据库写入不交错。

恢复时校验全部窗口：

- 窗口序号连续
- 前缀的 tick 和意图偏移连续
- 每个窗口状态哈希匹配
- `scene_hash`、`params_version`、`algorithm_version` 全部匹配

任何一项不匹配都会在事务中整体删除该场景检查点，并从 0 全量重算，禁止新旧窗口混用。

窗口内部异常会：

- 回滚当前窗口内存状态；
- 写入 `window_failures`；
- 保留之前已提交窗口；
- 返回原始业务错误码，允许再次从最后已提交前缀重试。

### 并发窗口流水线

工作线程池负责可并行的不可变窗口准备：时间切片、意图静态校验、窗口边界与 checkpoint 元数据。共享经济状态推进和全局总量/溢出归集按窗口顺序串行折叠，确保并行与单线程输出相同。

边界参数：

- `workers`：1..32
- `buffer_per_worker`：1..1024
- 结果通道容量为 `workers * buffer_per_worker`
- 缓冲耗尽时生产者阻塞，不静默丢窗口
- 窗口提交顺序与单线程完全一致

### 工作台

本地 Web 工作台提供：

- 样本选择与场景打开
- 生产链层级树、活跃/停滞状态
- 节点产出、投入、容量、所需工人编辑
- 意图时间轴；拖动意图标记修改逻辑时间
- 粗略预览与精确结算曲线叠加
- 节点 × 采样时刻差异热力图
- 批次提交、失败回滚、错误码/字段路径展示

编辑先进入工作副本。暂存编辑会去抖调用 `/preview`，在不修改服务端工作副本的前提下返回粗粒度曲线；`Commit batch` 将整批编辑应用到克隆副本，重新校验、重算哈希、执行窗口化结算，成功后才替换已提交场景。任一编辑或结算失败返回 `E_TXN`，带 `batch_id`，整批不落地。

主要 API：

```text
GET  /api/health
GET  /api/samples
POST /api/sessions                       {"sample_id":"frontier_small"}
GET  /api/sessions/{id}
POST /api/sessions/{id}/draft            {"batch_id":"...","edits":[...]}
POST /api/sessions/{id}/preview          {"batch_id":"...","edits":[...]}
POST /api/sessions/{id}/commit           {"batch_id":"...","edits":[...]}
POST /api/sessions/{id}/discard
```

### 异常与终止边界

| 错误码 | 场景 |
|---|---|
| `E_DECODE` | JSON/样本负载损坏 |
| `E_SCHEMA` | 缺字段、成环、非法定点值、意图引用不存在节点 |
| `E_STATE` | 升级状态非法、结算内部状态异常；失败步回滚 |
| `E_PARA` | 步长、窗口、采样时刻、并行度或缓冲参数越界 |
| `E_DIVERGE` | 快照不一致 |
| `E_TXN` | 工作台批次失败并整体回滚 |
| `E_UNIT` | 运行时意图目标/工人/类型非法 |
| `E_RES` | 消耗意图库存不足 |
| `E_CHECKPOINT` | 检查点顺序、哈希或事务异常 |
| `E_CANCEL` | 用户取消结算 |

进程被强制终止时，只能依赖已提交且版本匹配的窗口恢复。未提交窗口、未写入的最终报告都不保证存在；本项目不承诺强杀后的完整报告。

## 验收结果

本机实际运行：

```bash
go run ./cmd/workbench -verify -report artifacts/verification-report.json -workdir data/verification
```

结果：`all_ok=true`。三组样本的 truth、preview、incremental、resume、parallel 五项全部通过；并行窗口数与串行一致：

- `frontier_small`: 12 个窗口
- `river_medium`: 23 个窗口
- `industry_large`: 38 个窗口

完整报告见 `artifacts/verification-report.json`。

自动化测试还覆盖：

- 缺字段 / 成环 / 意图坏引用 / 负速率 / 非单调时间戳
- 升级同一步原子切换与失败回滚
- 停滞起始时刻、容量截断与溢出来源
- `E_PARA`、`E_DIVERGE`、`E_TXN`、`E_UNIT`、`E_RES`、`E_DECODE`
- SQLite 检查点顺序写、并发重复写拒绝、版本/哈希整体失效
- 窗口失败记录且不污染已提交前缀
- 工作台 HTTP 批次失败 409 与成功提交后的窗口序列

## 目录

```text
cmd/samplegen/            固定种子样本生成 CLI
cmd/workbench/            本地工作台与验收 CLI
internal/core/            模型、校验、定点结算、窗口、预览、比较
internal/det/             字节级稳定子随机源
internal/samples/         样本生成与清单
internal/store/           纯 Go SQLite 检查点
internal/verify/          三路 / 并发 / 恢复验收
internal/workspace/       工作副本、批次事务、服务端会话
internal/workbench/       HTTP API + 内嵌前端
data/samples/             固定样本与 manifest
data/verification/        验收临时 SQLite（运行时生成）
artifacts/                验收报告
```

## 技术说明

更详细的状态机、事务边界和一致性决策见 `docs/DESIGN.md`。

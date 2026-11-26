# MaskQC 笔画拓扑方向核验工作台

从空目录独立实现的 Go 领域组件与本地诊断应用。工程不依赖容器、外部服务或真实账号；图标、候选掩码、笔刷施工记录和解析真值均由固定种子生成。

原始需求保留在 `docs/REQUIREMENTS.md`，实现与实测结果见 `docs/IMPLEMENTATION.md`、`docs/VALIDATION.md` 和 `artifacts/validation-report.json`。

## 快速开始

环境：Linux/WSL、Go 1.22+。SQLite 驱动为纯 Go 的 `modernc.org/sqlite`。

```bash
go mod download
go test ./...

# 生成 96/128/160 三个分辨率、每个分辨率 v1/v2/v3 三份工件
go run ./cmd/maskqc generate -seed 20261127 -out samples

# 执行三版本、三路径、编辑失效、故障隔离、恢复重试验收
go run ./cmd/maskqc verify -data samples -report artifacts/validation-report.json

# 本地诊断页面
go run ./cmd/maskqc serve -data samples -db state/maskqc.db -addr 127.0.0.1:8080
```

打开 `http://127.0.0.1:8080/` 可查看候选掩码、积算掩码、逐笔覆盖、连通域统计、方向类别、依赖图、失效清单和预算/排队明细。页面提供“全量重算”和“编辑首笔”两个演示动作。

离线重复构建（依赖已进入本地模块缓存后）：

```bash
GOPROXY=off go build ./...
```

## 目录

| 路径 | 职责 |
| --- | --- |
| `model/` | 内部规范模型、错误码、方向/矩形类型、稳定哈希、统一排序 |
| `schema/` | v1/v2/v3 严格加载、升级默认痕迹、降级 `E_LOSSY`、整数换算与回读自检 |
| `sample/` | 固定种子多分辨率图标、候选掩码扰动、真值、三版本工件写盘 |
| `raster/` | 单笔整型覆盖栅格化、alpha-over 合成、块级覆盖 |
| `topology/` | 4 连通域、孔洞、环带宽度、边界段法向与方向一致性、`E_TOP`/`E_DIR` |
| `engine/` | 唯一合成次序、块缓存键、依赖图、失效传播、并行预算调度、三路径与对比 |
| `checkpoint/` | `database/sql` + SQLite 笔画级事务、块检查点、恢复与失效 |
| `web/` | `net/http` JSON API、PNG 图像、中文诊断页面 |
| `cmd/maskqc/` | `generate`、`verify`、`serve` 命令 |

## Schema 版本

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 字段命名 | snake_case | 嵌套 lower snake | camelCase |
| 坐标 | 绝对像素 | 相对前一笔终点 | 绝对像素 |
| 覆盖权重 | 千分比 `0..1000` | 万分比 `0..10000` | 万分比 `0..10000` |
| 笔刷硬度 | 缺失，默认 70 | 有 | 有 |
| 笔画方向 | 缺失，默认 `unknown` | 缺失，默认 `unknown` | 有 |
| 分组/合成次序 | 缺失 | 有合成次序、无分组 | 有分组与合成次序 |

加载器把三种表达统一为内部绝对坐标与万分比权重，并保留 `SourceVersion`。v1 升级的每次默认补齐都记录字段路径、旧值、默认值和来源；导出到低版本时，无法表达的方向、分组、硬度和合成次序给出带 `handling` 的 `E_LOSSY`。导出工件会立刻由对应版本加载器回读，避免产出不可消费文件。

整数换算规则：

- 相对坐标以排序后前一笔终点为锚点，使用 `safeAdd`/`safeSub`，溢出返回 `E_OVF` 并携带字段、对象和两个操作数。
- 万分比降至千分比采用显式 `nearest_half_up`，每处记录商、余数、结果和残差摘要；全链路不使用浮点累积。
- 对可表达权重（样本为 10 的倍数），v3→v1→v3 的起止坐标、笔序号和权重逐笔完全一致；v3→v2→v3 另保持硬度与合成次序。
- 坐标安全范围 `±1048576`、半径 `1..65536`，避免栅格整数平方溢出。

## 合成与核验口径

- 合成次序唯一由 `(priority, compositeOrder, seq, id)` 决定；输入切片乱序不改变结果。
- 笔刷覆盖只依赖绝对像素坐标、半径、硬度与权重；硬度内圈为 `radius*hardness/100`，外圈整型线性衰减。
- 积算掩码按唯一次序对候选掩码做 alpha-over：`src + base*(255-src)/255`，舍入规则固定。
- 连通域采用 4 邻接，前景阈值 128。每个域计算面积、重心、外接框、孔洞数、平均环带宽度与全部边界段。
- 边界段法向与最近参与笔的行进向量比较，分类为 `co_directional`、`counter_directional`、`perpendicular`；域方向取稳定多数类别。
- 域按重心最近配对；孔洞、环宽、面积、外接框、重心、像素 IoU 超容差返回 `E_TOP`，方向类别不符返回 `E_DIR`，错误携带域编号与实测值。

错误码：`E_SCHEMA`、`E_LOSSY`、`E_OVF`、`E_PARA`、`E_TOP`、`E_DIR`、`E_BUDGET`。

## 缓存、失效与三路径

块缓存键组合：

```text
SHA256(
  块输入内容哈希,
  参与笔标识与覆盖分量上游键,
  块内合成次序版本,
  换算参数版本,
  换算残差摘要,
  算法版本
)
```

逐笔覆盖上游键只包含覆盖相关参数，避免方向或未改变相对顺序的元数据触发像素重算。依赖图显式连接 `conversion → stroke → block → topology → direction`。

编辑失效规则：

- 坐标、半径、硬度、权重变化：失效旧包围框与新包围框覆盖的块。
- 工序/序号变化：比较编辑前后全序，只失效“相对次序翻转且两笔包围框相交”的块。
- 拓扑与方向核验作为下游节点重算；未失效块复用内存与 SQLite 持久化结果。
- 失效报告包含编辑笔标识、受影响笔集合、块编号、复用块和下游节点。

三条路径：

1. `full`：忽略缓存，一次性全量重算。
2. `incremental`：同引擎增量运行，复用有效块。
3. `resume`：新引擎从 SQLite 已提交状态恢复。

`engine.CompareRuns` 逐块、逐像素比较，并先比较分块提交序列；发现差异时返回首个不一致块号和绝对像素坐标。并行调度限制最大工作线程、每线程像素缓冲和待处理队列；队列耗尽会阻塞提交并记录原因与微秒级等待，不丢块。

## SQLite 检查点

表：`project_meta`、`stroke_checkpoint`、`stroke_component`、`block_checkpoint`、`invalidation_log`。

每个笔画在单个事务内写入项目戳、笔画状态、受影响块覆盖字节摘要/缓存键/内容哈希/残差摘要/次序版本/算法版本；引擎按稳定顺序串行调用。块结果另以事务提交。

恢复时严格校验项目内容哈希、画布尺寸、算法/换算版本、笔画版本、块覆盖摘要与块字节摘要。任一失配会清除该项目全部检查点并触发重算。失败笔画只更新自身失败状态并删除自身覆盖分量；其他成功笔画和已提交块不受影响，可清除故障注入后重试。

## HTTP API

| 方法/路径 | 说明 |
| --- | --- |
| `GET /` | 诊断页面 |
| `GET /healthz` | 健康检查 |
| `GET /api/projects` | 工程与运行摘要 |
| `GET /api/project/{id}` | 笔画、域、方向、预算、依赖图、失效与块提交 |
| `POST /api/project/{id}/rerun?mode=full` | 全量重算 |
| `POST /api/project/{id}/edit` | 编辑笔画并返回失效传播 |
| `GET /img/{id}/candidate.png` | 候选掩码 PNG |
| `GET /img/{id}/accumulated.png` | 积算掩码 PNG |
| `GET /img/{id}/icon.png` | 生成图标 PNG |
| `GET /img/{id}/stroke/{strokeId}` | 单笔覆盖分量 PNG |
| `GET /img/{id}/truth/{domainId}` | 单域参考掩码 PNG |

## 测试

```bash
go test ./...
go test -race ./...
go vet ./...
go run ./cmd/maskqc verify -data samples -report artifacts/validation-report.json
```

当前实测结果与页面烟雾结果见 `docs/VALIDATION.md`。

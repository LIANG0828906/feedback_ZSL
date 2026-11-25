# 角色换装部件合批内核

这是一个可本地复现的 Go 1.22 内核，覆盖换装工程的装配冲突诊断、确定性合批、材质/预算取舍、依赖失效、增量重划分、SQLite 检查点恢复和本地诊断页。样本与真值由固定种子生成，不依赖外部服务、账号或网络资源。

本次环境的 WebKitGTK 4.0/4.1 未安装，因此按题目允许的降级路径交付 `net/http` 本地页面入口，没有引入不可构建的 Wails 桌面壳。Wails CLI `v2.10.2` 已存在，但 `pkg-config` 找不到 `webkit2gtk-4.1`。

## 实际验证

```text
Go:       go1.22.12 linux/amd64
Seed:     20261126
样本:     4 个角色、36 个部件、16 个姿态真值、56 个真值批次
真值:     16/16 逐项一致
全量:     16/16 一致
恢复:     16/16 一致
三路径:   16/16 一致
结果:     PASS
```

冲突覆盖：

```text
mount_contention=12  explicit_exclusion=12  skin_incompatible=4
```

取舍原因覆盖：

```text
material_slot_pressure=12  vertex_budget_pressure=8  index_budget_pressure=8
material_affinity=16       skin_separation=12        budget_fit=12
initial_batch=16
```

核验产物：

- `artifacts/sample_bundle.json`：确定性输入与解析真值，SHA-256 `253885014ad60fc624fb82b4f6836184ab0d2f0907d0c0e67989416d9b3af69b`
- `artifacts/verification.json`：真值、全量、SQLite 恢复与三路径逐用例结果

## 快速开始

要求 Go 1.22+。SQLite 驱动使用 `github.com/mattn/go-sqlite3`，因此 Linux 下需要可用的 C 编译器和 `CGO_ENABLED=1`。

```bash
go mod init wardrobe     # 本次已初始化
go build ./...
go test ./...

go run ./cmd/wardrobe generate \
  -seed 20261126 \
  -out artifacts/sample_bundle.json

go run ./cmd/wardrobe verify \
  -sample artifacts/sample_bundle.json \
  -db artifacts/wardrobe.db \
  -report artifacts/verification.json

go run ./cmd/wardrobe serve \
  -addr 127.0.0.1:8787 \
  -sample artifacts/sample_bundle.json \
  -db artifacts/wardrobe.db
```

浏览器打开 `http://127.0.0.1:8787`。页面提供：

- 挂点层级与装配占用图
- 部件勾选后的即时冲突诊断
- 批次、材质槽和顶点/索引/材质预算
- 每个子网格的批内顶点及索引区间
- 材质槽争用、蒙皮隔离、顶点/索引挤压的取舍依据
- 并行队列阻塞原因与等待时长
- 材质槽或几何规模编辑后的增量复用/失效结果
- full / incremental / SQLite recovery 三路径比对
- 单批次重试

## 数据与真值

`domain.Bundle` 是样本根对象，包含多个角色工程。每个工程声明：

- `mounts`：挂点层级、`skinMode=any|skinned|rigid`
- `materials`：材质槽与版本
- `parts`：挂点引用、顶点数、三角面数、子网格、蒙皮标记、批次优先级
- `exclusionRules`：显式互斥对
- `constraints`：最大材质槽、顶点、索引预算，是否允许跨蒙皮
- `truth.cases`：解析构造的合法装配集合、冲突、批次、子网格偏移、取舍记录与提交序列

真值由 `generator/truth.go` 独立生成。该文件不导入生产的 `assembly` 或 `planner` 包，避免“用被测实现证明自己”。`planner/planner_test.go` 对每个姿态逐项比较内核输出和真值。

## 核心规则

### 装配合法性

`assembly.Evaluate` 是纯函数。输入集合先做稳定去重排序，冲突按固定键排序：

- 挂点争用：同一挂点被多个部件占用，按部件对输出
- 显式互斥：命中声明互斥规则
- 蒙皮不相容：部件蒙皮标记与挂点 `skinMode` 不一致

合法集合为零冲突时的完整输入；有冲突时，按 `batchPriority` 降序、部件 ID 升序选择确定性的极大合法子集。输入列举顺序和线程调度不改变输出。

### 确定性合批

部件按 `batchPriority` 降序、ID 升序处理。每个候选部件按批次序号尝试首个可容纳批次：

- 跨蒙皮策略：不允许时，批次内蒙皮标记必须一致
- 新批次材质槽去重后不超过预算
- 顶点累计和索引累计不超过预算

能装入已有批次时，结果为 `reuse_existing_batch`，原因为材质亲和或预算适配；否则开新批次，结果为 `split_new_batch`。优先级较高的部件先占预算，后续部件只能复用或延后到新批次，因此相同输入稳定复现。

每个批次内按实际放置顺序累计偏移：

```text
part.vertexOffset = sum(previous part vertexCount)
part.indexOffset  = sum(previous part triangleCount * 3)
submesh.vertexOffset = part.vertexOffset + submesh.vertexStart
submesh.indexOffset  = part.indexOffset  + submesh.indexStart
```

### 依赖、缓存与精确失效

部件内容哈希来自部件完整结构；批次缓存键由以下内容共同决定：

```text
部件内容哈希集合
+ 材质槽版本
+ 约束版本与参数
+ 算法版本
+ 上游批次缓存键
```

批次依赖显式记录 `part:*`、`mount:*`、`material:*@version`、两种预算、蒙皮策略与上游批次。编辑时先找真实失效前沿：

- 部件缩小后若能回填更早批次，前沿前移
- 材质槽版本变化命中旧批次时，前沿前移
- 前缀批次及取舍记录必须逐项等于旧缓存，否则回退全量
- 只重放前沿之后的批次；未受影响前缀标记为 `reused`

### SQLite 恢复边界

`store/sqlite.go` 使用两个表：

- `checkpoint_sets`：输入哈希、约束/算法版本、期望批数、`building|ready` 状态与完整结果
- `batch_snapshots`：每批部件、子网格布局、预算、依赖、内容哈希、版本与缓存键

写入时全局串行且 SQLite 连接数为 1。每批使用独立事务；最后一批事务同时把整组状态改为 `ready`。恢复时校验：

- 状态为 `ready`
- 输入哈希、约束版本、算法版本
- 期望批数、连续提交序号
- 每批内容哈希
- 每行与完整结果负载逐项一致

任一不符则整组删除并重建，不保留半套快照。`SIGKILL` 后不保证退出报告；`building` 或批数不足的组会在恢复时识别并作废。

### 并行调度

批次预算校验和偏移重算通过固定工作池执行，限制：

- `MaxWorkers`
- `CandidateBufferPerWorker`（按部件权重）
- `PendingQueueBudget`（待处理部件数）

队列预算耗尽时生产者阻塞并记录 `pending_part_budget_exhausted` 与等待毫秒数。任务异常和 panic 被隔离为单批错误，其他批次继续完成；`RetryBatch` 只校验并替换指定批次。

## HTTP API

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/` | 本地诊断页 |
| GET | `/api/health` | 版本与工程数 |
| GET | `/api/projects` | 工程、约束与姿态摘要 |
| GET | `/api/diagnostics?project=&case=` | 图、批次、冲突、取舍、调度与真值核对 |
| POST | `/api/validate-assembly` | 当前选择集的即时冲突诊断 |
| POST | `/api/edit-part` | 材质槽/几何编辑并增量重划分 |
| POST | `/api/tri-compare` | full / incremental / recovery 比对 |
| POST | `/api/retry-batch` | 单批次重试 |
| POST | `/api/recover` | 强制从 SQLite 恢复或作废重建 |

默认只监听 `127.0.0.1`。

## 测试覆盖

```bash
go test ./...
```

- `schema`：缺字段、重复 ID、悬空引用、子网格越界/重叠、未知 JSON 字段
- `assembly`：三类冲突、确定性合法子集、顺序无关、输入不被修改
- `planner`：所有姿态逐项匹配真值，全部取舍原因被样本覆盖
- `engine`：晚批次编辑复用前缀、三路径逐项相等、SQLite 重启恢复
- `scheduler`：并行度上限、部件队列阻塞、异常/ panic 隔离、单任务重试
- `store`：检查点往返、`building` 部分写入整组作废、版本/内容哈希不符整组作废
- `httpui`：页面、诊断 API、即时冲突、三路径与单批重试

更细的算法与表结构说明见 `docs/TECHNICAL.md`。

## 边界

- 不访问网络、不启动容器，页面只绑定本机。
- 样本生成器只使用固定种子，不读取时钟或主机状态。
- 不承诺强制终止后仍有退出报告；承诺下一次恢复不消费不完整快照。
- Wails 桌面壳未构建，原因是当前环境缺少 WebKitGTK；`net/http` 页面已实际启动并通过 `curl` 验证。

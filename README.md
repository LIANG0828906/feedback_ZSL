# dressbatch

角色换装部件合批渲染数据内核与装配冲突诊断组件。项目是一个可直接导入的 Go 包
`dressbatch`，同时提供 `cmd/dressbatch` 本地命令、固定种子样本生成器、SQLite
检查点、增量重划分、受限并行验证和 `net/http` 诊断页面。

实现不依赖 Docker、真实账号、线上服务或外部数据集。

## 环境与验证基线

实际环境：

- Linux / WSL2 Ubuntu，x86_64
- `go version go1.22.12 linux/amd64`
- SQLite CLI `3.46.1`
- Go SQLite 驱动：`modernc.org/sqlite v1.34.5`（纯 Go，不要求 CGO）

当前用户没有免交互 sudo 权限，因此没有执行系统级 `apt install`。`sqlite3` 与
`libsqlite3-dev` 的 Ubuntu 包已通过 `apt-get download` 解包到本地 `.tools/root`：

```bash
.tools/root/usr/bin/sqlite3 --version
# 3.46.1 2024-08-13 ...
```

Go 实现使用纯 Go SQLite 驱动，因此普通 `go test ./...` 不依赖系统 C 库。本地解包
的 `sqlite3` 已符号链接到用户级 `~/.local/bin`，当前 shell 可直接执行
`sqlite3 --version`；也可用于直接检查数据库。`.tools/` 已加入 `.gitignore`。

## 快速运行

```bash
go mod download

# 生成 4 套固定种子角色工程及其解析真值
go run ./cmd/dressbatch generate -seed 42 -count 4 -out samples

# 对照样本中的批次、子网格偏移和冲突真值
go run ./cmd/dressbatch verify -dir samples

# 完整演示：全量 -> SQLite -> 重启恢复 -> 局部编辑 -> 增量重划分 -> 再次恢复
go run ./cmd/dressbatch demo -out artifacts/demo -seed 42 -count 4

# 本地诊断页（默认 127.0.0.1:8080）
go run ./cmd/dressbatch serve -sample samples/sample_00.json -addr 127.0.0.1:8080

# 检查持久化内容
.tools/root/usr/bin/sqlite3 artifacts/demo/checkpoints.db \
  'select run_id, commit_seq, batch_id, batch_index from batch_checkpoints order by run_id, commit_seq;'

# 自动化验收
go test ./...
go test -race ./...
go vet ./...
```

诊断页端点：

- `GET /`：批次划分、预算条、冲突、失效传播、依赖图、排队事件
- `GET /api/state`：同一状态的 JSON
- `GET /healthz`：健康检查

## 实际验证结果

以下结果由当前仓库内命令实际运行得到。四套样本的 24 个场景报告保存在
`artifacts/verification.json`，单角色全量/恢复/增量演示保存在
`artifacts/demo/verification.json`。

- 固定种子 `42` 生成 4 套角色工程，每套 6 个采样场景，共 24 个场景。
- 24 个场景全部与 JSON 真值逐项一致；其中 8 个合法场景完成真实合批。
- 覆盖三类装配冲突：挂点争用、显式互斥、蒙皮分组不相容。
- `char-01` / `idle-legal` 划分为 3 批：
  - `b0001`: `body, head`，1472 顶点 / 2205 索引
  - `b0002`: `hair_a, tail`，449 顶点 / 672 索引
  - `b0003`: `torso_a, hand_l, hand_r`，1132 顶点 / 1695 索引
- 示例偏移：`body` 为顶点 0 / 索引 0，`head` 为顶点 995 / 索引 1491。
- 首次提交 3 个批次后关闭并重新打开 SQLite，恢复批次序列仍为
  `b0001, b0002, b0003`。
- 对最后一个批次的 `hand_r` 只修改子网格布局后：
  - `b0001`、`b0002` 复用持久化结果；
  - 仅 `b0003` 失效，原因为 `part_layout_changed:hand_r`；
  - 增量结果与从空缓存全量重划分逐字段相等；
  - 原子替换后再次重启恢复，3 个批次全部有效。
- `go test ./...` 覆盖真值、纯函数确定性、`E_PACK`、增量比对、SQLite
  篡改截断、SIGKILL 恢复、限流排队、异常隔离/重试和诊断页面。
- `go test -race ./...` 与 `go vet ./...` 均通过；当前包级测试覆盖率约 71.8%。

## 领域模型

`Project` 包含：

- `materials`：材质槽及版本。
- `mounts`：挂点表，挂点 ID 必须唯一。
- `parts`：部件几何、三角面/索引数量、材质槽集合、子网格区间、蒙皮标记、
  蒙皮分组和取舍优先级。
- `mutual_exclusions`：跨部件显式互斥规则。
- `constraints`：单批材质槽、顶点、索引预算，是否允许跨蒙皮，以及策略名。
- `truth.assemblies`：若干采样姿态下的候选装配、冲突真值、批次真值和提交序列。

### 装配判定

`EvaluateAssembly` 是纯函数，冲突按两两部件逐条枚举并按稳定顺序排序：

- `mount_conflict`：两个部件占用同一挂点。
- `explicit_exclusion`：命中 `mutual_exclusions` 规则。
- `skin_incompatible`：两个蒙皮部件属于不同 `skin_group`。

非法集合会输出一个确定性极大合法子集 `accepted_part_ids`：按
“优先级降序、部件 ID 升序”贪心接受无冲突部件，同时在 `conflicts` 中保留全部
原始冲突。这样调用方既能看到完整诊断，也能得到可继续处理的合法集合。

### 确定性合批

`Pack` 只接受合法集合，并按以下稳定规则工作：

1. 候选按 `priority` 降序、`id` 升序处理。
2. 当前剩余最高优先级部件成为新批次种子，记录 `split` 决策。
3. 其余候选依次尝试装入当前批；满足跨蒙皮策略、材质槽并集、顶点/索引预算
   时记录 `place`，否则记录 `defer` 及具体原因。
4. 所有延后候选在下一批重新按同一顺序尝试。
5. 每个子网格的批内顶点/索引区间由所属部件的批内基址加源区间起点得到。

因此输入部件顺序、Go map 遍历顺序、缓存命中或并发完成顺序都不会改变结果。

### 错误码

- `E_SCHEMA`：结构/字段/引用/区间校验失败，包含 `object_id` 与 `field`。
- `E_ASSEMBLY`：试图对含冲突集合合批。
- `E_PACK`：部件无法在单批约束内装入，附带剩余预算与部件规模。
- `E_DIFF`：增量与全量结果不一致，定位第一个 `batch_id` 与 `field`。
- `E_TRUTH`：解析真值不一致。
- `E_STORE` / `E_RECOVERY`：SQLite 写入或恢复边界错误。
- `E_SCHEDULER`：线程池配置、排队、批次失败或 panic 隔离错误。
- `E_HTTP`：本地诊断服务错误。

## 依赖、缓存键与失效

每个批次的 `dependencies` 显式记录：

- `part:<id>`、`mount:<id>`
- 批次成员的完整 `part_hash:<sha256>`
- 只参与本批取舍决策、但被延后的候选使用 `part_fit_hash:<sha256>`
- `material:<id>@<version>`
- `constraints:<sha256>`
- 受影响的 `exclusion:<id>`
- 非首批的 `upstream:<previous-cache-key>`

缓存键由成员内容哈希、延后候选的拟合哈希、材质槽版本、互斥规则指纹、约束参数
版本、算法版本与上游批次缓存键共同计算。

这样区分的原因是：延后候选只影响预算/材质/蒙皮等“拟合决策”；若只修改其子网格
布局，不应误废更早批次。但若修改其几何规模、材质槽、优先级或蒙皮属性，则会使
包含其决策轨迹的批次失效，并沿 `upstream` 依赖向后传播。

`IncrementalRepack` 会：

1. 对比新旧工程；
2. 找出首个失效批次；
3. 复用此前批次（可来自 SQLite 恢复结果）；
4. 从剩余候选按相同优先级算法重算后缀；
5. 与从空缓存全量重划分逐项比较；
6. 有差异时返回 `E_DIFF`，否则返回结果和失效清单。

## SQLite 检查点

表结构：

- `runs`：运行 ID、工程哈希、约束版本、算法版本、状态。
- `batch_checkpoints`：提交序号、批次 ID/序号、工程哈希、批内容哈希、缓存键、
  材质/约束/算法版本、依赖 JSON、完整批次 JSON、提交时间。

保证：

- `SetMaxOpenConns(1)` 加进程内互斥，批次提交不交错。
- `AppendBatch` / `AppendNextBatch` 每次只提交一个批次，且要求
  `commit_seq` 连续、`batch_index == commit_seq`。
- `ReplacePlan` 在一个事务中整体替换增量缓存。
- `RecoverRun` 逐批重算内容哈希、缓存键、材质版本、约束/算法版本和依赖。
- 首个不匹配处及其后所有批次会在事务中删除；此前已提交前缀保留。
- 强制终止后没有退出回调，恢复只读取事务提交过的 SQLite 状态。

测试 `TestForcedTerminationRecovery` 会启动真实子进程，提交两个批次后等待，再由
父进程 `SIGKILL`，随后重新打开数据库并验证两个已提交批次可恢复。

## 受限并行调度

`PoolConfig`：

- `MaxWorkers`：最大工作线程数。
- `CandidateBufferPerWorker`：每个 worker 的候选任务缓冲上限。
- `QueueBudget`：等待队列中候选部件总量预算。

调度器在预算耗尽时阻塞，不会静默丢弃，并记录：

- 批次 ID
- 原因（`queue_part_budget` / `candidate_buffer_full`）
- 等待毫秒数
- 排队候选部件数

单批次校验失败或 panic 只会令该批次失败，不污染其他批次。`RetryFailedBatches`
仅重试失败批次，成功批次及其尝试次数保持不变。

## Go API 入口

```go
project, err := dressbatch.LoadProjectFile("samples/sample_00.json")
assembly, err := dressbatch.EvaluateAssembly(project, candidatePartIDs)
result, err := dressbatch.Pack(project, assembly.AcceptedPartIDs)

changes, err := dressbatch.DiffProjects(oldProject, newProject)
incremental, err := dressbatch.IncrementalRepack(dressbatch.IncrementalRequest{
    OldProject:       oldProject,
    NewProject:       newProject,
    Previous:         result,
    CandidatePartIDs: assembly.AcceptedPartIDs,
    ReusedPrefix:     persistedBatches,
})

store, err := dressbatch.OpenStore("artifacts/demo/checkpoints.db")
recovery, err := store.RecoverRun(ctx, runID, project)

report, err := dressbatch.ValidateBatchesWithPool(ctx, project, result.Batches,
    dressbatch.PoolConfig{MaxWorkers: 4, CandidateBufferPerWorker: 3, QueueBudget: 64})

handler, err := dressbatch.NewDiagnosticHandler(state)
```

## 目录

- `types.go`：领域数据结构与错误码。
- `validate.go`：JSON 加载、严格字段与引用/区间校验。
- `assembly.go`：装配合法性与冲突。
- `pack.go`：确定性合批、偏移与解释性决策。
- `canonical.go`：规范化哈希和缓存键。
- `incremental.go`：工程 diff、失效传播、增量重划分。
- `compare.go`：真值与增量/全量比较。
- `store.go`：SQLite 原子检查点、恢复与原子替换。
- `scheduler.go`：受限线程池、排队事件、失败隔离与重试。
- `generator.go`：固定种子样本生成器。
- `server.go`：本地诊断页面。
- `cmd/dressbatch/`：CLI。

# 多路复用网关流调度与信用内核 — 交付说明

本交付是一个可集成 Go 组件与本地可重复演示。所有样本由固定种子的确定性生成器在工作区内生成；不连接真实网络、账号、证书或外部服务。SQLite 驱动 `modernc.org/sqlite v1.29.10` 已 vendor，默认 `GOPROXY=off go test ./...` 可离线通过。

## 一键验证

```bash
go version                 # 实测 go1.22.12 linux/amd64
go test ./...
./scripts/generate.sh      # 生成 data/project-v1.json、project-v2.json、project-v3.json、truth.json
./scripts/verify.sh        # 全量验收并写 artifacts/demo/
./scripts/demo.sh          # 生成、验收并打印前 12 条调度轨迹
./scripts/checkpoint-crash-demo.sh
```

也可以直接运行 CLI：

```bash
go run ./cmd/generate -out data
go run ./cmd/verify -data data -artifacts artifacts/demo
go run ./cmd/demo all --workers 2 --buffer 8
go run ./cmd/demo inject --list
go run ./cmd/demo inject --case dependency_cycle
```

## 代码结构

- `internal/model`：统一工程、流、事件、轨迹和编辑模型。
- `internal/bizerr`：稳定 `E_xxx` 业务码、路径与上下文。
- `internal/fixedpoint`：Q16 定点份额、31 位窗口边界、CheckedAdd/Mul 与饱和运算。
- `internal/schema`：v1/v2/v3 装载器、单位换算、依赖环检测和规范一致性比较。
- `internal/sample`：固定种子生成器、三版序列化、参考轨迹真值和临界定点用例。
- `internal/engine`：逻辑时间窗口核算、分层加权轮转、阻塞归因、RST/孤儿重挂。
- `internal/incremental`：编辑深拷贝、失效传播闭包、前缀状态缓存复用、增量/全量逐项比较。
- `internal/parallel`：固定 worker 池、每流缓冲预算、确定性线程预算背压探针。
- `internal/checkpoint`：SQLite meta/stream/scheduler/global 表与崩溃恢复。
- `internal/app`：验收用例编排和 JSON 报告。
- `cmd/generate`、`cmd/verify`、`cmd/demo`：命令行入口。

## 三版 Schema 映射

| 维度 | v1 | v2 | v3 | 统一模型 |
| --- | --- | --- | --- | --- |
| 字段风格 | snake_case | camelCase | 短名/混合域 | `model.Project` |
| 单位 | 字节 | KiB（×1024） | 字节 | 字节 int64 |
| 时间 | `frame_no` | `atMs` | `tick` + `clock.basis` | `frame_seq` + `time_ms` |
| 权重 | 千分比 | 万分比/基点 | 万分比 | `weight_permyriad`（千分比×10） |
| 窗口 | 绝对剩余 | budget−alreadySent | `creditMode` 显式选择 | `initial_window_bytes` |
| 优先级 | 扁平 `priorities` | 嵌套 `priorityTree` | `upstreamFid` | `parent_id` |
| 水位 | `frame_watermark` | `waterMarkMs` | 可缺省 `waterLevel` | 有效 `frame_watermark` |

装载后会校验重复流、未知父流、依赖环、零/越界权重、负窗口、窗口超上限和事件引用。冲突字段组合返回 `E_SCHEMA_CONFLICT`，无法映射返回 `E_FIELD_MAPPING` 或 `E_SCHEMA_UNSUPPORTED`。

## 确定性样本

默认种子 `424242`，生成：

- 连接 `C-deterministic-1`，初始连接窗口 6144 字节、上限 65536。
- 流 `1,3,5,7,9`，依赖树为 `1 -> {3,5}`、`3 -> {7,9}`。
- 42 个逻辑事件：HEADERS、五轮 DATA、连接/流 WINDOW_UPDATE、RST_STREAM、FIN DATA。
- 29 条实际发送记录。
- `data/truth.json`：每事件连接窗口、各流窗口/可发送字节、阻塞集合、发送帧、RST 移除时刻和释放信用。

关键手工真值：前六帧选中流依次为 `1,3,5,7,9,1`，每帧 1024 字节；事件 10 后连接窗口为 0；事件 11 中流 3 同时有连接窗口零和流窗口零归因；事件 37 RST 流 3 释放 4096 窗口信用并把 7/9 重挂到流 1。

## 调度与归因

调度器使用无浮点的 Smooth Weighted Round Robin，并按依赖树递归选择：

- 根/父节点先按同级有效权重累计 `current_weight += weight`。
- 选择当前权重最大且子树可运行的节点；选中后减去同级总权重。
- 若节点本身无 DATA，则递归进入其子节点。
- RST 后从候选、pending 和窗口占用中移除目标流。
- `orphan_policy=reparent` 时子流重挂到祖父；`inherit` 时最低 ID 子流继承父流权重，其余子流重挂。

原因码包括 `E_CONNECTION_WINDOW_ZERO`、`E_STREAM_WINDOW_ZERO`、`E_FRAME_TOO_LARGE`、`E_BODY_SEGMENTS_EXHAUSTED`、`E_BUFFER_FULL`、`E_STREAM_RESET`、`E_STREAM_CLOSED`。终态 reset/closed 保留在每流状态中，但不再进入活动阻塞集合或调度候选。

## 定点误差

- 报告份额使用 Q16：`fixedpoint.Scale = 65536`。
- 权重千分比到万分比是精确 ×10，不产生舍入。
- 份额采用确定性 half-up 整数舍入，声明容差为 2 Q16 ULP。
- `artifacts/demo/fixed-point-report.json` 输出每个流的累计分配字节、真值分配、Q16 份额、份额差、剩余窗口和是否在容差内。
- 连续 7 次和 1000 次 WINDOW_UPDATE、100 次权重刻度往返和饱和边界用例均为整数精确结果，不会累积浮点误差。

## 增量失效

`internal/incremental` 对工程做 JSON 深拷贝，处理过程不修改源工程。编辑类型：

- `change_weight`
- `change_parent`
- `change_initial_window`
- `append_event`
- `delete_event`
- `update_event`

管理器缓存每个事件前的完整引擎状态。事件类编辑复用编辑点之前的状态；初始权重/父依赖/窗口编辑从事件 0 重算并给出依赖祖先/后代闭包。重算轨迹会附加 `source:*`、`propagate:dependency-closure`、`propagate:connection-window`、`propagate:stream-window:<id>`、`propagate:scheduler-selection` 等来源。每次编辑都与一次性全量重算逐项 `DeepEqual`。

实测复用/重算事件数：

- 权重/父依赖/初始窗口：reused=0, recomputed=42。
- 更新事件 31：reused=31, recomputed=11。
- 删除事件 20：reused=20, recomputed=21。
- 末尾追加事件：reused=42, recomputed=1。

## 并行与背压

`parallel.RunConnections` 启动固定数量 worker，结果按连接 ID 排序。worker 数边界为 `[1,64]`，每流 pending 缓冲边界为 `[1,4096]`。

- 参数越界返回 `E_BUDGET_RANGE`。
- `Pool.TryAcquire` 在预算占满时确定性返回 `E_THREAD_BUDGET_EXHAUSTED`。
- DATA 使某流 pending 帧达到上限时返回 `E_BUFFER_FULL` 和 stream/event/budget/pending 上下文；不放行、不静默丢帧。

## SQLite 检查点

数据库表：

- `checkpoint_meta`：检查点格式版本、schema 版本、project ID。
- `stream_state`：每流事件序号、父/有效父、权重、窗口、sent、状态、原因、pending 和完整 runtime JSON。
- `scheduler_progress`：SWRR `current_weight` 和有效依赖父映射。
- `connection_checkpoint`：最后写入的全局已提交事件序号、连接窗口和逻辑时间。

每个事件在一个 SQLite 事务内按以下顺序提交：

1. upsert 全部 `stream_state`；
2. upsert `scheduler_progress`；
3. 最后 upsert `connection_checkpoint.committed_event_seq`；
4. `COMMIT`。

崩溃注入：

```bash
go build -o bin/demo ./cmd/demo
./bin/demo crash --db artifacts/demo/x.db --after 4 --mode before_global_seq
# shell 可见 137/SIGKILL；未提交事务回滚，global seq 仍为 3
./bin/demo recover --db artifacts/demo/x.db
```

`after_commit` 模式在提交后 SIGKILL，重启从已提交序号之后继续。恢复时执行版本匹配；错误格式版本或 schema 版本返回 `E_CHECKPOINT_VERSION`，截断/损坏 SQLite 文件返回 `E_CHECKPOINT_CORRUPT`。

## 稳定错误码

`E_SCHEMA_UNSUPPORTED`、`E_SCHEMA_PARSE`、`E_SCHEMA_CONFLICT`、`E_FIELD_MAPPING`、`E_WINDOW_NEGATIVE`、`E_WINDOW_OVER_CAP`、`E_WEIGHT_RANGE`、`E_WEIGHT_ZERO`、`E_DEPENDENCY_CYCLE`、`E_FRAME_TOO_LARGE`、`E_BUDGET_RANGE`、`E_BUFFER_FULL`、`E_THREAD_BUDGET_EXHAUSTED`、`E_CHECKPOINT_CORRUPT`、`E_CHECKPOINT_VERSION`、`E_INCREMENTAL_MISMATCH`。

## 主要产物

- `data/project-v1.json`
- `data/project-v2.json`
- `data/project-v3.json`
- `data/canonical-project.json`
- `data/truth.json`
- `data/fixed-point-cases.json`
- `artifacts/demo/verification-report.json`
- `artifacts/demo/incremental-report.json`
- `artifacts/demo/fixed-point-report.json`
- `artifacts/demo/demo-traces-v1.json`
- `artifacts/demo/schedule.jsonl`
- `artifacts/demo/checkpoint-recovery.db`

## 边界说明

这里实现的是 HTTP/2 风格多路复用网关的确定性流控/调度内核与帧工程 schema，不实现真实 TCP/TLS、HPACK 或线上 socket。SQLite 使用纯 Go modernc 驱动，因此不需要 CGO 或系统 `sqlite3` CLI。

# Mesh Policy Orchestration Workbench

一个零外部服务的首版可验收链路：确定性生成服务网格策略工程，经三个 schema 版本装载为统一领域模型，完成逐逻辑时刻裁决、变更传播、未完成传播门控、增量复用、准入替换、有界并行调度、纯 Go SQLite 检查点恢复，并通过 `net/http` 本地工作台展示和注入异常。

- Go：`go1.22+`
- SQLite 驱动：`modernc.org/sqlite`（纯 Go，`database/sql`，数据库为本地文件）
- 监听：默认 `http://127.0.0.1:18080`
- 固定种子：`42042`
- 不使用 Docker、Kubernetes、真实网格或外部账号

## 快速运行

```bash
go test ./... -count=1
go run ./cmd/meshbench -addr 127.0.0.1:18080 -db data/checkpoint.db -seed 42042
```

浏览器打开 <http://127.0.0.1:18080/>。

常用参数：

```bash
go run ./cmd/meshbench -addr 127.0.0.1:18081 -db /tmp/mesh-policy.db
```

## 验收入口

```bash
# 三版等价、门控锚点、增量一致性
go test . -run 'TestThreeSchema|TestIncremental' -v

# 准入真值、SQLite 崩溃/截断/版本、背压、HTTP 异常码、无基线拒绝
go test . -run 'TestAdmission|TestCheckpoint|TestScheduler|TestHTTP|TestGate' -v

# 全量与竞态
go test ./... -count=1
go test -race ./... -count=1
```

最近一次实际结果见 `docs/VERIFICATION.md`。

## 三版 Schema

| 版本 | 风格 | 时刻 | 端口 | 选择器 | 默认拒绝 |
| --- | --- | --- | --- | --- | --- |
| `mesh.policy/v1` | snake_case | `at_seq` / `start_tick` | `target_port` 或 `[from,to]` | 内联扁平标签 | 显式 `default_deny` |
| `mesh.policy/v2` | camelCase | `atMs` / `startMs`，按 `tickMs=100` 换算 | `portRange` | 按命名空间嵌套的 `namespaceTree` | 可省略，版本口径为默认拒绝；也接受显式 `defaultDeny` |
| `mesh.policy/v3` | `apiVersion` + 扁平 `selectorTable` | `atTick` / `tickWindow` | `"8080"` 或 `"7000-7010"` | `selectorRef` 引用扁平选择器表 | 显式指针字段 |

动作编码：

- V1：`action_code=0` 放行，`1` 拒绝。
- V2：编码互换，`actionCode=1` 放行，`0` 拒绝。
- V3：使用 `allow` / `deny` 语义字段，也保留 `effectCode` 扩展位。

当文本动作与整型编码矛盾、编码越界或两者都缺失时，返回 `E_ACTION_CONFLICT`，上下文带 JSON 字段路径、策略编号和实际值。

## 核心语义

- 策略归属目标工作负载命名空间；空选择器命名空间按策略所在命名空间处理，`*` 表示跨命名空间。
- 裁决候选必须同时满足命名空间、标签、服务账户和端口区间。
- 优先级数值越大越先裁决；同优先级按 `declared_order` 升序，最后以稳定 ID 兜底。
- 无命中时使用工程的默认拒绝口径，依据码为 `R-DEFAULT-DENY-MARKER` 或 `R-DEFAULT-ALLOW-NO-MATCH`。
- 变更生成 `MRK-<change>-G<generation>` 标记：G1 为旧/新策略并集可能命中的连接，G2+ 沿工作负载 `depends_on` 闭包扩散。
- 每代一个逻辑时刻到达；到达当时尚未确认，时刻结束确认。连接在标记未到达或未确认时不能应用新策略：有流程基线则回退旧判定，无基线则稳定拒绝。
- 增量编辑只把传播闭包内的连接标记为重算，其余直接复用缓存；报告闭包边界、失效来源、每个结果的 marker 和前后差异。离散判定容差为 `0`。

## 准入规则

替换请求只接受：

- `state=revoked`：规则号 `R-REPLACE-CONFIRMED-DEAD`
- `end_seq < at_seq`：规则号 `R-REPLACE-EXPIRED-WINDOW`

活跃、未过窗口条目返回 `E_REPLACE_TARGET_ACTIVE`。命名空间上限和优先级边界：

- 上限负数或超过声明最大值：`E_POLICY_LIMIT_INVALID`
- 上限为 `0`：`E_POLICY_LIMIT_ZERO`
- 当前条目数超过上限：`E_NAMESPACE_POLICY_LIMIT`
- 优先级不在 `[0,1000]`：`E_PRIORITY_RANGE`

## 并发与检查点

`internal/sched` 提供：

- 全局 worker 预算，范围 `1..64`
- 每命名空间缓冲，范围 `1..4096`
- `Pipeline.Submit` 的非阻塞满队列错误 `E_BACKPRESSURE`，上下文内含 `E_NAMESPACE_BUFFER_FULL`
- `Admission.TryAdmit` 在 worker 槽耗尽时返回 `E_WORKER_BUDGET_EXHAUSTED`

不同 worker 数、调度顺序只影响 goroutine 执行顺序；输出按逻辑时刻、声明次序和 ID 稳定排序。工作负载编辑通过逻辑时刻快照读取，避免跨命名空间共享可变状态。

SQLite 表：

- `checkpoint_meta`：工程版本与全局已提交序号
- `workload_state`：每个工作负载最新已应用判定 JSON
- `propagation_queue`：待传播 marker 和载荷

保存事务先写 `workload_state`，再写 `propagation_queue`，最后更新 `checkpoint_meta.committed_seq` 并提交。事务提交前模拟 kill 会回滚，重开仍读到上一完整序号。版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`，工作台随即从起点全量重算。

## HTTP API

| 方法/路径 | 作用 |
| --- | --- |
| `GET /` | 本地诊断工作台 |
| `GET /api/health` | 健康检查 |
| `GET /api/current` | 当前运行时工程、真值和最近报告 |
| `GET /api/equivalence` | 三版优先级、选择器、传播集合和判定等价核对 |
| `POST /api/load` | 装载 `v1` / `v2` / `v3` |
| `POST /api/run` | 按 worker/buffer 预算重跑 |
| `POST /api/edit` | 增量编辑并返回闭包、缓存复用和 diff |
| `POST /api/replace` | 准入替换 |
| `POST /api/checkpoint/save` | 保存指定逻辑序号检查点 |
| `POST /api/checkpoint/restore` | 恢复；版本不匹配时返回全量重算报告 |
| `POST /api/inject` | 注入 17 类异常/背压/检查点故障 |

统一响应：

```json
{ "ok": false, "error": { "code": "E_ACTION_CONFLICT", "message": "...", "context": { "path": "$.policies[0].action_code" } } }
```

## 稳定业务码

完整常量见 `internal/xerr/xerr.go`。常用码：

- 装载/引用：`E_SCHEMA_UNSUPPORTED`、`E_DOCUMENT_INVALID`、`E_FIELD_CONFLICT`、`E_ACTION_CONFLICT`、`E_PORT_RANGE_INVALID`、`E_SELECTOR_TREE_INVALID`、`E_SELECTOR_CYCLE`、`E_WORKLOAD_MISSING`、`E_WORKLOAD_EXITED`、`E_POLICY_NOT_FOUND`
- 选择器诊断：`E_SELECTOR_OVERLAP`（普通重叠仍按优先级裁决；严格注入时作为异常码）
- 准入：`E_REPLACE_TARGET_ACTIVE`、`E_POLICY_LIMIT_INVALID`、`E_POLICY_LIMIT_ZERO`、`E_NAMESPACE_POLICY_LIMIT`、`E_PRIORITY_RANGE`
- 调度：`E_WORKER_BUDGET_INVALID`、`E_BUFFER_BUDGET_INVALID`、`E_WORKER_BUDGET_EXHAUSTED`、`E_BACKPRESSURE`、`E_NAMESPACE_BUFFER_FULL`
- 检查点：`E_CHECKPOINT_OPEN`、`E_CHECKPOINT_CORRUPT`、`E_CHECKPOINT_TRUNCATED`、`E_CHECKPOINT_VERSION_MISMATCH`
- 门控结果码：`E_GATE_DENIED_NO_BASELINE`

## 代码地图

- `internal/model`：规范领域模型
- `internal/gen`：固定种子工程与验收锚点
- `internal/schema`：V1/V2/V3 DTO、编码器、装载器、换算和引用/环校验
- `internal/engine`：匹配裁决、传播、门控、准入、增量闭包
- `internal/sched`：worker/缓冲预算与有界管道
- `internal/store`：SQLite 检查点和规范工程版本
- `internal/workbench`：HTTP API、嵌入式页面和异常注入
- `cmd/meshbench`：可执行入口

## 说明

生成器刻意保留重叠选择器，因为重叠本身不是装载错误：高优先级策略胜出，其余候选进入 `basis` 作为裁决链依据。异常注入中的 `overlapping_selector` 会在严格诊断口径下返回 `E_SELECTOR_OVERLAP` 和胜出策略。

# 健康检查并行探测调度内核 — 交付说明

## 运行

```bash
go version          # 开发机实测 go1.22.12 linux/amd64
go test ./...
go test -race ./...
make demo           # 生成并复验 samples/seed-42
make offline-build  # GOPROXY=off，仅用标准库
```

CLI：

```bash
go run ./cmd/hckernel gen    -out samples/seed-42 -seed 42
go run ./cmd/hckernel verify -dir samples/seed-42
go run ./cmd/hckernel demo
```

`demo` 会重新生成三版 schema、事件、编辑、真值和异常夹具，并输出
`artifacts/verification-report.json`。

> `go mod init 健康检查调度内核模块` 在 go1.22.12 上会报
> `malformed module path ... invalid char '健'`，因此模块路径使用等价的
> ASCII 名称 `hckernel`，`go 1.22`。

## 零外部依赖决策

题目允许“无外部依赖时使用纯 Go 实现的本地持久化文件模拟 SQLite 等价能力”。
本实现没有引入 SQLite 驱动或 `go.sum`，检查点使用追加式事务记录：

1. `state` 记录完整实例健康态、连续计数、在途探测、延后队列和 `obs_seq`，带 SHA-256；
2. `fsync` 后才追加 `cursor` 全局轮次游标；
3. 重启只读取有匹配 `cursor` 的状态；
4. 只有完整 `state`、没有 `cursor` 的记录视为未提交轮次并回退；
5. 末行截断、JSON 断裂或校验和不匹配返回 `E_CHECKPOINT_TRUNCATED`；
6. 指纹/工程版本不符返回 `E_CHECKPOINT_VERSION_MISMATCH`。

这保证状态/队列先落盘、全局轮次序号最后发布。

## 代码地图

- `model.go`：规范工程、实例、机架、事件、轮次报告、健康轨迹。
- `errors.go`：稳定 `E_*` 业务码和可定位上下文。
- `schema.go`：`hcs/v1`、`hcs/v2`、`hcs/v3` DTO 与装载转换。
- `validate.go`：规范模型校验和异常码映射。
- `engine.go`、`scheduling.go`、`health.go`：逻辑时钟调度、预算/队列、状态机。
- `budget.go`：约束编号、权重排序和 Hamilton 整数配额。
- `parallel.go`、`parallel_runner.go`：有界 worker/buffer 并行执行与背压。
- `checkpoint.go`：事务型文件检查点、恢复、截断/版本异常注入。
- `incremental.go`：快照缓存、失效闭包、来源标记和全量等价比较。
- `generator.go`、`sample_schema.go`：固定种子样本、事件、编辑和三版序列化。
- `verification.go`、`cmd/hckernel/main.go`：文件级验收与本地演示入口。

## 三版 Schema 归一口径

- v1：snake_case；`tick_ms=50`，周期/超时/退避/半开等待均为 tick；阈值为绝对连续次数；扁平 `targets`；含 `active_probe`；权重为千分比。
- v2：camelCase；时间直接为毫秒；`remainingSuccesses/remainingFailures` 在初始计数为零时按“还需连续判定次数”映射为同一绝对阈值；扁平 `instances`；权重为万分比。
- v3：`schema_version`；周期/退避/逻辑轮/最大延后/半开等待使用秒，超时使用毫秒；绝对 `*_limit`；`topology` 是机架 → 节点 → 后端树；省略主动标记时按 schema 默认 `active_probe=true`；权重为 PPM。

权重统一为 PPM：千分比 × 1000，万分比 × 100。预算排序和
`AllocateWeightedSlots` 均为整数运算，因此三种刻度结论一致。

## 调度语义

轮次在逻辑时刻 `(round-1)*step_ms` 推进；每轮顺序为：

1. 处理到期注入事件和拓扑/编辑事件；
2. 判定达到 deadline 的在途探测（超时计为一次失败）并释放槽位；
3. 丢弃超过 `max_defer_ms` 的延后探测；
4. 生成周期/重试/半开候选；
5. 按声明的 `deferred_first` 或 `periodic_first` 合并延后与新周期候选；
6. 依次受全局并发和机架并发约束分配槽位；
7. 输出 `eligible`、`dispatched`、`suppressed`、`rejected`、`deferred_now`、
   `queue`、`rescheduled` 和 `dropped`。

抑制原因包括：

- `probe_in_flight_timeout`
- `backoff_active`
- `interval_not_due`
- `half_open_wait`
- `active_probe_disabled`
- `budget_deferred` / `deferred_waiting`
- `budget_zero_rejected`

约束编号：

- `C-GLOBAL-CONCURRENCY`
- `C-GLOBAL-BUDGET-ZERO`
- `C-RACK-<rack>-CONCURRENCY`
- `C-RACK-<rack>-BUDGET-ZERO`
- `C-PROBE-BUFFER`

## 健康状态机

状态为 `initial`、`healthy`、`unhealthy`、`half_open`。任意相反结果会清零当前连续计数；达到成功/失败阈值后按工程声明执行 `reset` 或 `keep`。进入不健康时安排 `half_open_delay_ms` 后的试探；试探发出时进入 `half_open`，一次成功按半开口径恢复健康，一次失败返回不健康并重新计时。每条轨迹包含事件编号、探测编号、前后状态、计数和阈值。

## 增量失效

`ResultCache` 保存每轮可恢复快照。编辑事件在逻辑时刻插入后：

- 编辑时刻之前的轮次和健康轨迹标记为 `baseline_cache` 并直接复用；
- `Closure` 给出起始/终止轮、事件游标、影响实例和依赖链；
- 阈值编辑显式包含状态迁移、半开链和可能的预算链；
- 权重/机架编辑包含加权顺序、机架容量、全局容量和延后队列链；
- 重算结果与把编辑事件放入完整事件流后的一次性全量重算逐轮比较。

## 并发与背压

规划和状态归约保持确定性串行；探测执行通过固定 worker 池 fan-out，输出按
`probe_id` 排序后再归约。请求并行度超过 worker 预算返回
`E_BACKPRESSURE_WORKER_BUDGET`；非阻塞 worker 租约耗尽返回
`E_BACKPRESSURE_WORKERS_BUSY`；批次超过缓冲上限返回
`E_BACKPRESSURE_BUFFER_FULL`。每个批次记录 `duration_ns`。

## 稳定错误码

`E_SCHEMA_VERSION_UNSUPPORTED`、`E_SCHEMA_DECODE_FAILED`、
`E_PROBE_INTERVAL_ZERO`、`E_PROBE_INTERVAL_INVALID`、
`E_NEGATIVE_TIMEOUT`、`E_NEGATIVE_DURATION`、
`E_THRESHOLD_OUT_OF_RANGE`、`E_RACK_REFERENCE_MISSING`、
`E_BUDGET_ZERO`、`E_WEIGHT_INVALID`、
`E_BACKPRESSURE_WORKER_BUDGET`、`E_BACKPRESSURE_WORKERS_BUSY`、
`E_BACKPRESSURE_BUFFER_FULL`、`E_CHECKPOINT_TRUNCATED`、
`E_CHECKPOINT_CORRUPT`、`E_CHECKPOINT_VERSION_MISMATCH` 等。

`Error` 结构携带 `path`、`instance_id`、`rack_id`、`event_id`、
`constraint_id`、`offset` 和 cause，便于定位输入或检查点位置。

# cachebench 实现与验证说明

本交付是一个零第三方 Go 依赖的领域组件与本地压测编排内核。模块路径为
`cachebench`，CLI 为 `./cmd/cachebench`。

## 运行

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go run ./cmd/cachebench --out artifacts/validation
```

CLI 会生成：

- `cachebench.v1.json`：snake_case、千分比、tick、扁平节点表、绝对 tick 观测。
- `cachebench.v2.json`：camelCase、万分比、毫秒、按机架嵌套、相对窗口序号观测。
- `cachebench.v3.json`：kebab-case、万分比/Q3 定点秒、递归机架树、绝对 Q3 秒观测。
- `checkpoint.sqlite`：真实 SQLite 文件，包含 `checkpoint_meta`、`node_state`、`window_stat`。
- `demo-output.txt` 与 `go-test-verbose.txt`：本轮实际输出。

## 领域组件

- `model.go`：统一内部模型；时间归一为整数毫秒，比率归一为 PPM，权重归一为万分比。
- `schema_dto.go` / `schema_encode.go` / `schema_load.go`：三个 schema 的 DTO、确定性样例编码和版本装载器。
- `generator.go`：固定种子 `20261127` 的工程、请求流、临界换算用例和参考真值。
- `engine.go`：逻辑毫秒内核，包含完成、恢复、熔断评估、加权路由、容量接纳、溢出和窗口统计。
- `fixedpoint.go`：声明位宽定点换算、溢出检测、饱和值和 PPM 容差比较。
- `incremental.go`：逻辑时刻参数覆盖、边界状态、失效闭包、tick/node-tick/window 缓存复用和全量等价校验。
- `parallel.go`：有界 worker 阶段并行、每 worker buffer 信号量、`E_BACKPRESSURE_*` 和每节点实际耗时诊断。
- `checkpoint.go`：`database/sql` 检查点 API、提交顺序、版本匹配恢复和起点重建。
- `sqlitedriver.go`：仅用标准库实现的 `database/sql/driver`；若运行环境未预置 `sqlite3` 驱动，则注册内置 `sqlite3`/`cachebench-sqlite3`。
- `sqlitefile.go`：标准库直接读写最小合法 SQLite 文件；已用系统 Python `sqlite3` 执行 `PRAGMA integrity_check` 验证。
- `validation.go` / `errs.go`：工程、预算、异常注入和稳定 `E_xxx` 业务码。

## 关键口径

- 权重：`400‰ -> 4000‱`，内部统一为 `WeightPermyriad`。
- 容量/错误率：容量比例和错误率统一为 PPM；熔断判定始终使用原始计数的整数乘除，不使用反复换算后的近似值。
- 时间：V1 `at_tick * tick_duration_ms`；V2 直接毫秒或 `window_index * window_ms`；V3 Q3 秒值按 `value * 1000 / fraction_scale` 转毫秒。
- 容量：请求在逻辑时刻先处理到期完成，再做恢复/熔断评估，随后按稳定请求顺序路由和接纳；`inflight >= max_inflight` 记为 `E_CAPACITY_OVERFLOW`，并带节点与 `CONN-LIMIT-<node>` 约束编号。
- 路由：FNV 稳定哈希落到 canonical 万分比桶；优先级越小越优先，同优先级按声明次序；裁决编号为 `ARB-PRI-<node>` 或 `ARB-ORD-<node>`，加权依据为 `B-WEIGHT-<node>-bucket-<n>`。
- 熔断：持续超过在途阈值或错误率严格超过上限才打开；恢复只比较 `tick - openSince >= recoveryMS`、在途恢复阈值和错误率恢复阈值，不使用真实睡眠。
- 增量：节点参数以带 `AtMS` 的 `NodeOverride` 进入同一条时间线；`AtMS` 之前的 tick 结果直接复用缓存，从该时刻的 `BoundaryState` 重放后缀，窗口按包含关系失效。
- 检查点：事务内先写全部 `node_state` 和 `window_stat`，最后写 `committed_tick`；未提交事务不会替换 SQLite 文件。版本不匹配返回 `E_CHECKPOINT_VERSION_MISMATCH`，恢复器从起点整体重算。

## 稳定异常码

| 场景 | 业务码 |
| --- | --- |
| 负并发上限/负容量 | `E_NEGATIVE_CAPACITY` |
| 运行时超过并发上限 | `E_CAPACITY_OVERFLOW` |
| 权重越界 | `E_WEIGHT_SCALE_OUT_OF_RANGE` |
| 熔断恢复/打开阈值倒序 | `E_BREAKER_THRESHOLD_ORDER` |
| 路由目标节点不存在 | `E_NODE_NOT_FOUND` |
| 无可达/未熔断节点 | `E_NO_ELIGIBLE_NODE` |
| worker 数越界 | `E_WORKER_BUDGET_OUT_OF_RANGE` |
| buffer 数越界 | `E_BUFFER_BUDGET_OUT_OF_RANGE` |
| 线程预算耗尽 | `E_BACKPRESSURE_THREAD_BUDGET` |
| buffer 预算耗尽 | `E_BACKPRESSURE_BUFFER_FULL` |
| 检查点截断 | `E_CHECKPOINT_TRUNCATED` |
| 检查点版本不匹配 | `E_CHECKPOINT_VERSION_MISMATCH` |
| schema 版本不支持 | `E_SCHEMA_VERSION_UNSUPPORTED` |
| 定点数溢出 | `E_FIXED_POINT_OVERFLOW` |

## 本轮实际验证结果

环境：`go version go1.22.12 linux/amd64`。

- `go vet ./...`：通过，`vet_exit=0`。
- `go test -count=1 ./...`：12 个验收测试全部通过。
- `go test -race -count=1 ./...`：通过，`race` 未报告数据竞争。
- `go test -cover ./...`：根包语句覆盖率 `79.3%`。
- `go run ./cmd/cachebench --out artifacts/validation`：通过；固定样本为 4 节点、112 请求、72 个逻辑时刻、9 个统计窗口。
- 三版本 canonical fingerprint 相同；三个装载结果均与生成器真值 `ResultsEqual`。
- 并行度矩阵 workers `1/2/4` × buffer `1/16` 均与顺序真值一致。
- 五类增量编辑均与一次性全量重算逐 tick、逐窗口一致。
- Python `sqlite3` 打开 `artifacts/validation/checkpoint.sqlite`：`PRAGMA integrity_check = ok`。

详细测试名称见 `artifacts/validation/go-test-verbose.txt`，CLI 输出见
`artifacts/validation/demo-output.txt`。

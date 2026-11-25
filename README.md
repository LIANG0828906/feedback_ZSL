# TLS Probe Kernel

一个不依赖真实站点的 TLS 握手并行探测、证书链逐级校验、预算/并行调度、SQLite 检查点恢复与增量失效内核。模块使用 Go 1.22+ 和纯 Go SQLite 驱动 `modernc.org/sqlite`，所有样本均由固定种子 `420261126` 的确定性生成器产出。

## 快速运行

```bash
go version
go mod tidy
go test ./...
go run ./cmd/tlscheck gen -out samples
go run ./cmd/tlscheck verify
go run ./cmd/tlscheck demo -samples samples -db artifacts/tlscheck-demo.db
go run ./cmd/tlscheck load -file samples/project.v3.json
```

CLI 成功输出 JSON 并以 `0` 退出；业务错误输出到 stderr 的 JSON `error.code`，进程以 `1` 退出，用法错误以 `2` 退出。

## 包结构

- `internal/model`：三版 schema 统一后的 canonical domain model 与快照哈希。
- `internal/bizerr`：稳定 `E_*` 业务错误码和可定位 `path/context`。
- `internal/projectio`：V1/V2/V3 DTO、严格 JSON 装载、编码、单位换算和链结构校验。
- `internal/engine`：确定性逻辑时间状态机、链校验、FIFO 等待队列、预算/并行/冻结门控、中断续做。
- `internal/runtime`：有界线程池、端点并行调度、线程租约、邮箱背压和实际耗时诊断。
- `internal/checkpoint`：SQLite 事务检查点、版本/快照匹配、截断库识别和步骤表。
- `internal/incremental`：依赖区间、缓存复用、失效来源和增量/全量一致性比较。
- `internal/sample`：固定种子工程与 `truth.json` 真值生成器。
- `tests`：验收测试，覆盖三版一致性、调度、恢复、并发、异常码和增量重算。

## 三版 Schema 归一

| 版本 | 结构/命名 | 时间单位 | 预算刻度 | 可选字段 |
| --- | --- | --- | --- | --- |
| `probe.project.v1` | snake_case、扁平常量 `targets` 表 | tick + `tick_ms`，同时兼容 ms 字段 | `1/1000` permille，乘 10 归一到 BP | 旧命名水位/固定摘要，可缺省 |
| `probe.project.v2` | camelCase、按链分组的 `probeTree` 嵌套树 | 毫秒 | `1/10000` basis points | `handshakeWatermark`、`certificatePinned/pinSha256` |
| `probe.project.v3` | `api_version`、扁平 `hosts`、秒字段 | 秒（兼容 ms） | `1/10000` per-myriad | `handshake_water_level`、`pin_fingerprint_sha256` |

canonical 预算单位为 BP（`1/10000`）：permille 乘 `10`，myriad/BP 保持原值。canonical 时间单位为毫秒。三版样本的 canonical SHA-256 相同，因此端点集合、链顺序和请求序列一致。

## 确定性样本场景

生成的 7 个端点覆盖：

- `alpha-ok`：正常 TLS 1.3 握手、三级链全部通过、固定摘要和水位字段。
- `beta-parallel`：`max_parallel=1`，验证排队序号与 `1s -> 3s -> 5s` 的准入时刻传导。
- `gamma-budget`：周期预算 500 BP、每条两级链 200 BP，验证 `G-3/G-4` 在周期重置时刻 `10s` 准入。
- `delta-freeze`：链更新 `2s` 到达、`4s` 提交，`3s` 请求以 `BUDGET_FROZEN/C05` 排队，提交后准入。
- `epsilon-fallback`：新链过期，回退到上一有效链；旧链随后过期时输出 `CHAIN_INVALID`。
- `zeta-issuer`：签发者/被签发者关系不匹配，第一级失败，后续级别为 `SKIPPED_AFTER_PRIOR_FAILURE`。
- `eta-backpressure`：缓冲上限为 1，第三个突发请求得到显式 `E_BUFFER_FULL` 背压结果。

`samples/truth.json` 是生成器调用确定性引擎形成的解析真值；测试会从三个 schema 文件重新运行并与真值逐请求、逐级、逐队列比较。

## 调度与门控

约束编号固定为：

- `C02_CERTIFICATE_CHAIN`：链无效且无有效回退。
- `C03_CHAIN_BUDGET`：链校验预算不足。
- `C04_MAX_PARALLEL`：端点并行握手上限耗尽。
- `C05_BUDGET_FREEZE`：链更新尚未提交，预算冻结。
- `C06_PENDING_BUFFER`：每端点等待缓冲已满。

队列按端点内 `endpoint_queue_seq` FIFO 判定；全局 `queue_no` 按首次入队时间、事件序号和端点队列序号生成。运行时 goroutine 调度顺序和端点处理顺序不会改变这些逻辑序号。

## 中断与 SQLite 检查点

检查点包含：

- `checkpoint_meta`：工程 ID、canonical 版本、整份工程快照哈希、已提交逻辑时间和全局序号。
- `endpoint_state`：已用预算、预留预算、冻结集合、当前/上一有效链、活动握手和完整队列状态 JSON。
- `chain_state`：端点当前链/最后有效链及提交时有效性。
- `wait_queue`：关系型等待队列和违反约束。
- `request_result`、`step_state`：请求结论和已提交的逐级校验步骤。

保存事务先写 endpoint/chain/queue/result/step，再写元数据，最后写 `global_sequence` 并提交。注入 `FaultBeforeGlobalSequence` 时事务回滚，旧序号仍可见。请求在第 N 级后被打断时，已提交级别和预算保留；重启后从 `NextLevel` 续做，不重复计算同一请求/链步骤。链更新在提交点前被打断时保持 pending/frozen，恢复后到提交时刻才解冻。

恢复时先校验 `canonical_version`，不匹配返回 `E_CHECKPOINT_VERSION`；工程快照哈希不匹配返回 `E_SNAPSHOT_MISMATCH`，调用方可清空过期判定缓存后重跑。截断或损坏的 SQLite 文件返回 `E_CHECKPOINT_TRUNCATED`。

## 增量失效

`incremental.Manager` 缓存首份报告和每个逻辑时刻的端点状态。编辑链有效期、签发关系、探测周期或增删请求后：

- 链编辑只影响引用该链的端点。
- 请求增删只影响所属端点及该请求时刻之后的队列/预算传导。
- 静态声明编辑从该依赖的最早观测区间重算，以保证与编辑后整份工程全量重算一致。
- 未来时刻生效的请求编辑会复用前置终态结果，并在 `provenance` 中输出 `CACHE_REUSE`、`SRC_*` 失效来源。

## 异常错误码

| Code | 触发 |
| --- | --- |
| `E_SCHEMA_UNSUPPORTED` | 无法识别 schema/version |
| `E_FIELD_CONFLICT` | tick 与 ms、秒与 ms、声明深度与路径等字段矛盾 |
| `E_FIELD_INVALID` | JSON/字段值不合法 |
| `E_BUDGET_SCALE_UNSUPPORTED` | 预算刻度不是 1000 或 10000 |
| `E_CHAIN_CYCLE` | 签发关系形成非根闭环 |
| `E_BUFFER_FULL` | 端点等待缓冲耗尽 |
| `E_THREAD_BUDGET` | 有界线程池无可用租约 |
| `E_INTERRUPTED_BEFORE_COMMIT` | 故障注入在提交点前打断 |
| `E_CHECKPOINT_TRUNCATED` | SQLite 文件截断、损坏或缺全局提交序号 |
| `E_CHECKPOINT_VERSION` | 检查点 canonical 版本不匹配 |
| `E_SNAPSHOT_MISMATCH` | 工程快照哈希变化 |

完整实测记录见 `docs/VALIDATION.md`。

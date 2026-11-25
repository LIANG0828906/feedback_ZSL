# ChaosDiag 交付说明

ChaosDiag 是一个无真实集群依赖的 Go 领域内核与本地诊断工作台。固定种子 `42` 的生成器产出三套 schema 文档和独立解析真值；推演、对账、增量缓存与 SQLite 检查点均在本地完成。

## 快速验收

```bash
go version                 # 已验证 go1.22.12 linux/amd64
go test -count=1 ./...
go run ./cmd/chaosdiag verify
go run ./cmd/chaosdiag samples
go run ./cmd/chaosdiag serve --addr 127.0.0.1:8088
# 浏览器打开 http://127.0.0.1:8088
```

命令行演示：

```bash
go run ./cmd/chaosdiag demo --version v2 --stop 10 \
  --checkpoint artifacts/checkpoints/demo.sqlite
go run ./cmd/chaosdiag anomaly --type cycle_dependency
```

## 代码结构

- `model/`：统一工程、选择器、指令、请求、推演结果、对账结论和稳定错误码。
- `loader/`：`v1`、`v2`、`v3` DTO、字段映射、ms/tick、绝对/相对区间、绝对/分位幅度和嵌套匹配树归一化。
- `gen/`：确定性样本、三版 JSON 渲染和独立生成器真值。
- `engine/`：确定性调度、并发/预算约束、命中裁决、豁免和逻辑毫秒推演。
- `reconcile/`：无注入基线对账，区分 `MATCH`、`INJECTION_DEVIATION`、`SERVICE_FAILURE`、`EXPECTATION_INVALID`。
- `incremental/`：编辑应用、传播闭包、请求级缓存失效、增量差异和全量等价报告。
- `checkpoint/`：`modernc.org/sqlite` 纯 Go SQLite 存储、逐 tick 事务、恢复和版本拒绝后整段重推。
- `workbench/`：`net/http` API 与嵌入的零外部 CDN 单页工作台。
- `cmd/chaosdiag/`：`verify`、`samples`、`demo`、`anomaly`、`serve` 命令。

## 需求追踪

1. **三版装载**：三套 DTO 均收敛到 `model.Project`；测试核对服务、边、指令 ID/顺序、请求 ID/路径完全一致。冲突 ms/tick 或区间返回 `CHAOS_E_FIELD_CONFLICT` 与冲突字段。
2. **命中与裁决**：按 tick、窗口、目标选择器和调度准入得到命中集合；多指令按 `priority desc, sequence asc` 输出候选与每个 effect dimension 的 winner。熔断豁免输出 exemption/source，且不参与 effect 裁决。
3. **耗时推演**：每跳输出 `start_ms`、`end_ms`、`base_ms`、`natural_ms`、延迟/重复修正、`correction_ms`、终态和来源。超时只用整数逻辑毫秒，不读取墙上运行耗时。
4. **一致性对账**：先跑无注入基线，再比较注入结果；输出分类、偏差指令和 `deviation_ms`。重复推演和乱序声明均有确定性测试。
5. **双约束调度**：请求级 `TIMEOUT-BUDGET-REQUEST` 用整数 ms 判定；调度准入还有 `TIMEOUT-BUDGET-ADMISSION`、`CONCURRENCY-GLOBAL`、`CONCURRENCY-SERVICE:<service>`。超限项进入队列并记录全部违反约束与位置，释放后按声明顺序重试。
6. **增量传播**：指令幅度/区间/选择器或边延迟变更会生成对象→目标→跳→请求的闭包；只重算闭包内请求，其余复用缓存。测试用 `reflect.DeepEqual` 对比增量结果与全量重算。
7. **SQLite 检查点**：一个事务内先写 `request_states`、`wait_queue`，再写 `project_version`，最后更新 `last_tick` 并提交。原子事务保证强杀后只见到完整 tick；版本不匹配返回 `CHAOS_E_CHECKPOINT_VERSION_MISMATCH`，`RecoverOrRun` 改为整段重推。
8. **工作台与异常**：页面支持版本切换、逐 tick 队列、请求跳详情、对账、编辑增量、闭包/缓存、检查点对照和七类异常按钮。异常只改克隆或 `anomaly-*.sqlite` 临时夹具，不改变当前工作台报告和源样本。

## 稳定业务码

| 场景 | Code |
| --- | --- |
| 不支持 schema | `CHAOS_E_SCHEMA_UNSUPPORTED` |
| 字段组合冲突 | `CHAOS_E_FIELD_CONFLICT` |
| 成环依赖 | `CHAOS_E_CYCLE_DEPENDENCY` |
| 指令/边指向缺失服务 | `CHAOS_E_TARGET_MISSING_SERVICE` |
| 负值延迟 | `CHAOS_E_NEGATIVE_DELAY` |
| 超时预算为零 | `CHAOS_E_TIMEOUT_BUDGET_ZERO` |
| 并发上限越界 | `CHAOS_E_CONCURRENCY_LIMIT_OUT_OF_RANGE` |
| 检查点截断/损坏 | `CHAOS_E_CHECKPOINT_CORRUPT` |
| 检查点版本不匹配 | `CHAOS_E_CHECKPOINT_VERSION_MISMATCH` |

## 实际验证记录

以下命令已在当前环境实际执行：

```text
go version go1.22.12 linux/amd64
go vet ./...                                                                 # 通过，无输出
go test -count=1 ./...                                                       # ok chaosdiag
go run ./cmd/chaosdiag verify                                                # VERIFY OK
```

完整测试输出：`artifacts/go-test-verbose.txt`。CLI 验收输出：`artifacts/verification-cli.txt`。

最近一次 `verify` 摘要：

```text
v1/v2/v3: services=6 edges=5 instructions=12 requests=14 truth_hop_match=14/14
incremental: recomputed=[R-03-LATENCY-SLO] reused=13 full_recompute=match
checkpoint: persisted_queue=2 resumed_tick=12 results=match
```

- HTTP 冒烟记录：`artifacts/http-smoke.txt`，覆盖 `/api/health`、`/api/state`、`/api/edit`、`/api/anomaly`、`/api/checkpoint-demo` 和首页。
- 信号冒烟记录：`artifacts/signal-smoke.txt`；`demo --tick-pause-ms 30` 收到 `SIGINT` 后停在最后完整 tick 3。
- 前端内嵌脚本通过 `node --check` 语法检查。
- `vendor/` 已固定 `modernc.org/sqlite v1.29.10` 及全部依赖，可离线执行 `go test -count=1 ./...`。

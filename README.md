# 多租户配额账本与准入工作台

这是一个零第三方 Go 依赖的首个可验收版本：在接入真实存储服务前，独立验证节点容量账本、逐逻辑时刻推进、准入闸门、突发争用公平性、增量失效和 SQLite 检查点恢复。

所有样本均由仓库内确定性生成器以固定种子 `42` 产出，不访问真实集群、云服务或外部账号。

## 快速运行

```bash
go version                 # 已在 go1.22.12 linux/amd64 验证
go test ./... -count=1
go test -race ./... -count=1

go run ./cmd/quota-workbench generate --seed 42 --out samples
go run ./cmd/quota-workbench verify \
  --seed 42 \
  --report artifacts/verification-report.json \
  --checkpoint artifacts/checkpoint.sqlite

go run ./cmd/quota-workbench serve --addr 127.0.0.1:18765 --seed 42
# 浏览器打开 http://127.0.0.1:18765
```

SQLite 检查点使用 WSL/Ubuntu 自带的 `libsqlite3.so.0`。本仓库没有引入 Go SQLite 第三方模块；cgo 仅直接链接系统版本化动态库。环境已验证 SQLite `3.46.1`。

## 代码结构

- `internal/model`：三版本归一后的节点、租户、事件、裁决、时刻快照和编辑模型。
- `internal/fixed`：64 位声明位宽定点换算、128 位乘积溢出检测、饱和结果和误差判定。
- `internal/projectio`：V1/V2/V3 JSON 装载、严格校验、稳定业务码、JSON Pointer 风格定位和三版本发射器。
- `internal/sample`：SplitMix64 固定种子生成器、争用场景、真值和临界换算用例。
- `internal/engine`：逐时刻账本、两阶段预扣/结算、公平排序、等待队列、并行只读评估和背压执行器。
- `internal/incremental`：状态缓存、失效区间分析、后缀恢复重算和全量等价比对。
- `internal/checkpoint`：真实 SQLite 表、事务提交、版本门和强制崩溃恢复测试。
- `internal/diagnostic`：CLI/HTTP 共用的八项验收诊断。
- `internal/workbench`：标准库 `net/http` 单页诊断工作台和 JSON API。
- `cmd/quota-workbench`：`generate`、`verify`、`serve` 命令。

## 三个 Schema 版本

| 版本 | 结构 | 容量 | 时刻 | 配额刻度 | 突发字段 |
| --- | --- | --- | --- | --- | --- |
| `quota.project.v1` | 扁平 `nodes` / `tenants` / `events`，snake_case | 字节 | `at_tick` | 千分比 `quota_permille` | 不出现，使用声明默认值：配额的 500‰ |
| `quota.project.v2` | `topology.racks[].nodes[]` 机架嵌套树，camelCase | `capacityKiB` | `atMillis` / `tickDurationMillis` | 万分比 `quotaBasisPoints` | `burstLimitKiB` 显式给出 |
| `quota.project.v3` | 扁平 `node_inventory` / `tenant_inventory`，snake_case | `capacity_kib` | `timestamp_ms` / `tick_ms` | 万分比 `quota_permyriad` | `burst_cap_kib` 可选；样本中刻意省略一个租户以验证默认口径 |

归一化后的规范值全部使用整数字节和逻辑 tick。样本选择 `RatioBasisBytes = 10000 MiB`、`tickMillis = 10`，因此 1‱ 正好是 1 MiB，1‰ 正好是 10 MiB，保证三个版本不存在舍入分歧。

同一工程从三个 JSON 装载后，测试使用 `reflect.DeepEqual` 验证规范项目完全一致，并分别运行引擎与 `samples/truth.json` 逐项一致。

## 稳定业务码

| 业务码 | 含义 |
| --- | --- |
| `E_SCHEMA_UNSUPPORTED` | 无法识别 schema |
| `E_FIELD_REQUIRED` | 必填字段缺失 |
| `E_FIELD_INVALID_TYPE` | JSON 类型/事件枚举无法解析 |
| `E_FIELD_AMBIGUOUS` | 同一对象同时给出字节/KiB、tick/ms、千分比/万分比等无法消歧字段 |
| `E_DUPLICATE_ID` | ID 或申请 ID 重复 |
| `E_REFERENCE_MISSING` | 租户、节点、放行申请或编辑目标不存在 |
| `E_TENANT_NOT_ALLOWED` | 租户不在节点可承载集合中 |
| `E_FIXED_SCALE_OVERFLOW` / `E_FIXED_SCALE_SATURATED` | 定点乘法或换算溢出并已饱和标记 |
| `E_GATE_LATENCY_BUDGET` | 声明预算不足以完成快速+精确判定 |
| `E_WAIT_QUEUE_LIMIT` | 每租户等待队列达到上限 |
| `E_BACKPRESSURE_WORKER_BUDGET` | worker 全忙且有界提交缓冲已满 |
| `E_CHECKPOINT_VERSION_MISMATCH` | 检查点格式版本或工程哈希不匹配 |
| `E_CHECKPOINT_CORRUPT` | SQLite 内容、事务摘要或状态无法恢复 |

错误对象包含 `code`、`path`、可选 `constraint` 和 `retry_tick`。例如歧义字段会返回 `/nodes/0` 这样的定位。

## 账本与准入口径

每个逻辑 tick 按以下顺序推进：

1. 应用该 tick 的权重调整。
2. 结算释放。
3. 重试已有等待队列。
4. 将新申请和队列申请合并为候选集合。
5. 按确定性公平键排序。
6. 并行执行无共享写入的租户额度评估。
7. 按排序顺序执行节点预扣和租户/保底精确结算。
8. 输出账本、节点占用、队列和逐申请裁决。

核心约束编号：

- `C-TENANT-CEILING`：`used <= quota_bytes + burst_limit_bytes`。
- `C-NODE-CAPACITY`：`node_used <= node_capacity_bytes`。
- `C-GUARANTEE-RESERVE`：节点剩余容量必须覆盖其他可承载租户尚未使用的保底差额。
- `C-WAIT-QUEUE-LIMIT`：每租户等待申请数不超过 `tenant_queue_limit`。
- `C-GATE-LATENCY-BUDGET`：模拟快速判定成本 1ms、精确结算成本 2ms；样本预算为 5ms。

公平排序键为：

```text
保底覆盖档（全额保底内 → 部分保底 → 纯突发）
→ 声明优先级（数值大者优先）
→ 当前权重（大者优先）
→ 租户 ID 字典序
→ 事件序号
→ 申请 ID
```

因此高优先级纯突发租户也必须排在保底额度内租户之后；完全平局可由租户 ID/申请 ID 复核。队列裁决输出 `reason_code`、`constraint`、`encroacher_tenant_id` 和建议 `retry_tick`。突发侵占来源在多个候选中按突发占用量最大、同额取租户 ID 最小确定，避免 map 遍历造成不确定性。

两阶段裁决中：

- `preheld_bytes` 是节点级快速预扣量。
- `settlement_correction_bytes = admitted_bytes - preheld_bytes`。
- 精确结算若发现租户额度或保底储备约束，会释放多余预扣并记录负修正。

worker 数和批次大小只影响并行评估方式。测试覆盖 workers `1/2/4/8/16` 与 batch `1/2/3/16`，结果通过 `reflect.DeepEqual`。

## 确定性样本场景

固定种子 `42` 生成 4 个节点、8 个租户和 40 条裁决，包含：

- 常规申请与释放。
- tick 2 的租户权重调整。
- tick 3 在 `node-a` 上的多租户同时争用。
- 高优先级 `tenant-epsilon` 已处于纯突发档，仍排在保底候选之后。
- 预扣 300/400/200 MiB 后精确结算为 274/300/150 MiB 的负修正。
- 等待、后续释放后重试和侵占来源租户编号。
- `node-q` 上同一租户三笔 200 MiB 申请；队列上限为 2，第三笔得到 `E_WAIT_QUEUE_LIMIT`。

真值位于 `samples/truth.json`，直接给出每个 tick 的：

- `available_quota_bytes`
- `used_bytes`
- `quota_remaining_bytes`
- `usable_remaining_bytes`
- `guarantee_remaining_bytes`
- `burst_used_bytes`
- `held_bytes`
- `queued_bytes` 和有序 `queue`
- 节点 `used/held/free`
- 裁决序列和全局公平次序

## 定点换算与误差界

`internal/fixed` 提供：

- 千分比 ↔ 万分比：`×10` 或带余数舍入除以 10。
- bytes ↔ KiB：整数 KiB 精确乘 1024；非整除字节用 `10^9` 缩放输出定点 KiB。
- tick ↔ ms：`tick × tick_millis`；非对齐毫秒输出定点 tick。
- 比例 → 字节：使用 `math/bits.Mul64/Div64` 做 128 位中间乘积并检测商溢出。

`samples/critical-cases.json` 包含比例边界、1025 bytes 非 KiB 整除、15ms 非 tick 对齐、`MaxInt64` 溢出/饱和，以及 8 个租户各自的可用配额、保底余量和突发额度计算值。非溢出用例均在声明的 1 byte 容差内；溢出用例显式标记 `E_FIXED_SCALE_OVERFLOW` 和饱和状态，装载器会拒绝这类工程值。准入引擎只使用归一后的整数字节，因此诊断舍入误差不会累积到裁决中。

## 增量失效

`incremental.Service` 保存每个 tick 后的可深拷贝 `engine.State`。支持四类编辑：

- `NODE_CAPACITY`
- `TENANT_QUOTA`
- `TENANT_GUARANTEE`
- `WEIGHT_EVENT`（按事件序号定位）

编辑会确定起始 tick 和依赖键，例如 `node:node-a`、`tenant:tenant-alpha`、`event:4`。`start_tick` 之前的 TickReport 标记为 `reused` 并附 SHA-256 缓存摘要；从 `start_tick-1` 恢复内部状态后只重算后缀区间，每个后缀结果标记 `recomputed` 和失效来源。

验收测试分别修改节点容量、租户配额、保底和权重事件，并将增量混合报告与带相同时刻编辑的一次性全量 `engine.Run` 做 `reflect.DeepEqual`。

## SQLite 检查点

检查点文件包含表：

- `checkpoint_meta`
- `committed_tick`
- `tenant_state`
- `node_state`
- `queue_member`
- `decision_record`
- `engine_state`

写入顺序在同一个 `BEGIN IMMEDIATE` 事务内：

1. 清理并写入租户、节点、队列、裁决和引擎状态。
2. 最后 upsert 单行 `committed_tick(id=1)` 全局时刻。
3. `COMMIT`。

崩溃测试启动子进程，在子状态写入后、全局时刻写入/提交前调用 `os.Exit(42)` 强杀。重新打开 SQLite 后只能读到上一提交 tick 和旧租户行，证明未提交事务被回滚，不会出现半写入账本。

恢复时校验：

- `checkpoint_meta.format_version == checkpoint-v1`
- `committed_tick.format_version`
- 可选 `project_hash`

不匹配时返回 `E_CHECKPOINT_VERSION_MISMATCH`。

可用系统工具独立检查：

```bash
python3 - <<'PY'
import sqlite3
con = sqlite3.connect('artifacts/checkpoint.sqlite')
print(con.execute('select tick, format_version, project_id from committed_tick').fetchone())
for table in ['tenant_state','node_state','queue_member','decision_record','engine_state']:
    print(table, con.execute(f'select count(*) from {table}').fetchone()[0])
PY
```

## HTTP 工作台

启动后访问 `http://127.0.0.1:18765`：

- `GET /api/verify`：八项诊断和增量来源摘要。
- `GET /api/schema?version=v1|v2|v3`：查看三版本 JSON。
- `GET /api/truth`：查看完整解析真值。
- `GET /api/tick?n=3`：查看某一逻辑时刻。
- `GET /api/incremental?kind=NODE_CAPACITY&target=node-a&tick=2&value=1342177280`

权重事件示例：

```text
/api/incremental?kind=WEIGHT_EVENT&sequence=4&value=180
```

## 实际验证结果

以下命令已在当前 WSL Ubuntu 环境实际执行：

```bash
go version
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
go run ./cmd/quota-workbench generate --seed 42 --out samples
go run ./cmd/quota-workbench verify --seed 42 \
  --report artifacts/verification-report.json \
  --checkpoint artifacts/checkpoint.sqlite
```

结果：

- `go vet ./...`：通过。
- `go test ./... -count=1`：全部包通过。
- `go test -race ./... -count=1`：全部包通过，无数据竞争。
- 诊断报告：`artifacts/verification-report.json` 中 8 项检查全部 `ok: true`。
- SQLite：独立 Python `sqlite3` 3.46.1 可打开 `artifacts/checkpoint.sqlite`，提交 tick 为 `8`。
- 原始测试输出保存于 `artifacts/test-results.txt`。

## 设计边界

这是接入真实存储服务前的领域组件和诊断工作台，不实现真实 RPC、磁盘容量探测、Docker 或外部账号。引擎将两阶段延迟建模为确定性的 1ms+2ms 成本，以便重复验收；接入真实服务时可把快速预扣和精确结算接口替换为实际调用，同时保留同一套裁决、错误码和检查点协议。

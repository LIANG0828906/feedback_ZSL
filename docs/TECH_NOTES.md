# 分块声学能量场：实现说明

本交付是一个仅依赖 Go 标准库的领域组件、本地 CLI、独立复核器和无外部资源的诊断页面。固定种子为 `0x42A8691E`，样本是 3×3 平面分块、3 个连续频带；每块包含吸收系数表和一个 3-4-5 几何反射面，因此直接路径长度固定为 `6000 Q`（6 m），一次反射路径固定为 `10000 Q`（10 m）。

## 快速运行

```bash
go version
go test -count=1 ./...
bash scripts/demo.sh out/demo

go run ./cmd/reverb demo -out out/demo
go run ./cmd/verify compare out/demo/full out/demo/realtime out/demo/recovery
go run ./cmd/verify audit out/demo/full
go run ./cmd/reverb serve -report out/demo/report.json -addr 127.0.0.1:8088
```

## 代码地图

- `internal/generator/generator.go`：确定性 LCG 样本生成器与独立解析真值 oracle。
- `internal/schema/validate.go`：`E_SCHEMA` 工程校验，含对象 ID、字段名和原因。
- `internal/engine/`：纯函数路径枚举、镜像源/反射筛选、定点长度、吸收能量与量化记录。
- `internal/rebuild/service.go`：缓存恢复、并行调度、跨块传递、缓存键、提交边界和报告。
- `internal/rebuild/conservation.go`：逐频带守恒台账与 `E_CONS` 违规列表。
- `internal/statefx/machine.go`：分块五态、回合四态和提交进度。
- `internal/deps/graph.go`：显式依赖图与精确失效计划。
- `internal/checkpoint/store.go`：校验和事务日志、原子 JSON 快照、版本过滤恢复。
- `internal/audit/`：不导入 `rebuild`/`engine` 的独立重算、真值核对和三方比对。
- `internal/web/server.go`：本地诊断页与 `/api/state`。
- `cmd/reverb`、`cmd/verify`：演示/服务入口和独立复核 CLI。

## 确定性与定点模型

- 几何坐标比例 `GeoScale=1000`，单位为毫米定点整数。
- 能量比例 `EnergyScale=1_000_000`，吸收系数使用百万分比，声明刻度 `AbsorptionQuantum=1000`。
- 路径段长度通过 `big.Int` 平方和整数平方根计算，记录 `length_sqrt` 截断值与 `sq-floor(root)^2` 残差。
- 吸收除法和边界透射除法均记录分子、分母、截断值和余数。
- 路径按 `(listener_id, key)` 排序，分块按 `(row,col,id)` 排序；map 不参与最终输出顺序。
- `ScreenChunk` 是无副作用纯函数；不同 worker 数、分块完成顺序不会改变路径清单或能量。

## 能量守恒口径

为同时保留“吸收系数衰减后的听点到达能量”和“逐频带守恒”，结果中区分：

- `listener_arrival_q`：经反射吸收衰减后到达听者/采样点的净能量。
- `absorbed_q`：反射过程中被材料吸收的能量台账。
- `inbound_q`：相邻块边界传递后进入本块的能量。
- `retained_q = listener_arrival_q + absorbed_q + inbound_q`（无邻块时加场外沉）。
- `arrival_energy_q`：守恒口径的分块到达/归集能量，与 `retained_q` 同值并作为复核/比对的首选字段。

对某个声源/频带，设可行路径数为 `P`、边界权重为 `W=4`：

```text
share           = floor(E / (P + W))
boundary_budget = E - P * share
reflected       = floor(pre * (EnergyScale - alpha_q) / EnergyScale)
absorbed_step   = pre - reflected
delivered       = floor(sent * transmission_permille / 1000)
boundary_loss   = sent - delivered
```

整数余数被显式归入吸收量或传递损耗，因此固定样本的全局残差为精确 `0`。全局校验为：

```text
Σ emitted_q = Σ retained_q + Σ transfer_loss_q
```

超差时返回 `E_CONS`，违规项含 `band_id`、`source_chunk_id`、`kind` 和实测 `residual_q`。

## 状态机与提交边界

分块状态：

```text
pending -> path_screening -> energy_transfer -> committed
                         └-> failed -> path_screening（重试）
```

回合状态：

```text
pending -> rebuilding -> committed
                      └-> failed -> rebuilding（重试）
```

非法迁移返回 `E_STATE`，状态保持不变。报告进度分别给出已提交分块数和已提交回合数。已提交记录带校验和与版本；新编辑生成新回合/新缓存版本，不就地改写已冻结事务。

## 失效传播与缓存键

显式依赖键包括：

```text
absorption:<chunk>:<band>
surface:<chunk>:<surface>:geometry
source:<source>:emission
source:<source>:position
boundary_transfer:<from>:<to>
downstream:<to>:energy
```

编辑吸收系数、声源能量/位置、反射面几何或边界传递后，`deps.Graph.Plan` 只返回目标块及其直接下游传递块，并在报告中输出 `{chunk_id, reason, dependency_key}`。

最终缓存键输入显式包含：

```text
chunk_id
content_hash
local_cache_key(content_hash + param_version + algorithm_version)
param_version
algorithm_version
upstream[]:
  from_chunk_id
  upstream_cache_key
  boundary_transfer_id
  boundary_transfer_version
```

## 检查点与 SQLite

题目禁止第三方包，而 Go 标准库不内置 SQLite 驱动，因此默认使用题目允许的“等价仓库内持久化方案”：

- `transactions.jsonl`：每个分块提交是一行完整 `CommitRecord`，包含路径摘要、能量、传递记录、依赖集合、内容哈希、参数/算法版本和 SHA-256 校验和。
- 互斥锁保证同进程写入不交错；`txn_seq` 单调，`order_index` 保持稳定的 row-major 提交顺序。
- `params.json`、`snapshot.json`、`truth.json`、`report.json` 使用临时文件、`fsync`、`rename` 的原子写。
- 强制终止造成的半行/坏校验尾部会在恢复时停止读取，此前完整事务仍可恢复；未提交块没有可用记录，必须整块重算。
- 只恢复 `algorithm_version` 和 `param_version` 均匹配的每块最高 `txn_seq` 记录。

`internal/checkpoint/sqlite_schema.sql` 给出了等价 SQLite 表、索引和单事务写入语句；若运行环境提供已注册的 `database/sql` SQLite 驱动，可直接映射同一提交边界。

## 三条路径与独立复核

`internal/demo` 在同一临时/输出根目录构造：

- `full/`：编辑后的关卡从空缓存一次性全量重算。
- `realtime/`：先提交基线，再依次应用吸收系数和声源发射编辑，只重算失效块。
- `recovery/`：基线后应用相同编辑，在第二个编辑块提交后取消；重启用 `Recover:true` 恢复已提交块并重算其余块。

`cmd/verify` 不调用主编排路径。`internal/audit` 只导入标准库、`internal/acoustic` 类型和检查点读取代码；依赖检查结果为：

```text
$ go list -deps ./cmd/verify | grep -E 'internal/(rebuild|engine)' || echo no-main-recompute-deps
no-main-recompute-deps
```

复核器从 `snapshot.json`、`transactions.jsonl`、`truth.json`/指纹重算路径摘要、频带合计、入界/归集台账和守恒残差；`compare` 自动定位首个不一致的分块、频带和字段名。

## 调度预算与诊断页

`internal/schedx.Pool` 支持：

- `MaxParallel`：最大 worker 数。
- `PathBufferPerWorker`：传入路径筛选内核的缓冲上限，超限返回 `E_BUDGET`，可放大缓冲后重试。
- `MaxQueuedChunks`：有界 channel 预算；满时提交方阻塞，记录 `pending_chunk_queue_budget_exhausted`、是否阻塞和等待毫秒，不丢弃任务。
- worker panic 被转换为该块错误，其他块继续；已有检查点不被污染。

页面 `/` 展示 3×3 分块热力、频带切换、听者/吸收/入界/归集能量、边界传递表、守恒残差、失效清单、提交来源和排队明细；`/api/state` 返回同一报告 JSON。

# QuotaFlow

多租户共享集群的配额账本、故障迁移停顿、增量失效、并发背压和 SQLite 检查点参考实现。项目为纯 Go，可直接在 Go 1.22+ 的 Linux/WSL 环境运行，不使用 Docker 或其他容器环境。

## 快速开始

```bash
go version
# go version go1.22.12 linux/amd64

go mod download
go test ./...

go run ./cmd/quotaflow generate -seed 42 -out samples
go run ./cmd/quotaflow load -dir samples
go run ./cmd/quotaflow verify -dir samples
go run ./cmd/quotaflow demo -seed 42 -out artifacts/demo
```

`demo` 会清理目标检查点、重新生成确定性样本，并输出：

- 三版 schema 归一化一致性
- 引擎逐时刻结果与生成器独立真值的对照
- 每时刻租户配额账本、停顿集合、迁移和等待队列
- 权重/节点容量编辑的失效来源、复用区间和全量重算一致性
- 真实子进程在 tick 7 全局提交前 `os.Exit(137)` 后的 tick 6 恢复与续算
- 九类异常注入的业务码

生成的报告位于 `artifacts/demo/demo-report.json`。

## 目录

```text
cmd/quotaflow/        CLI 工作台
internal/domain/      内部模型、定点运算、稳定业务错误、观测类型
internal/schema/      v1/v2/v3 严格装载与归一化
internal/generator/   确定性场景、三版编码、独立真值模拟器
internal/engine/      账本、准入、迁移、停顿、增量失效、并行探测
internal/checkpoint/  SQLite 检查点、事务顺序、恢复与损坏检测
internal/inject/      内存内异常注入
```

## 数据与 schema

内部模型统一为：

- 容量：`int64` 毫核
- 配额/保底：`int64` 基点，`10000 bp = 100%`
- 时间：按源时刻排序后的相对逻辑 tick，从 1 开始
- 拓扑：节点、机架、故障域父子关系分开规范化

三版输入映射：

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 字段命名 | `snake_case` | `camelCase` | 嵌套短名 |
| 配额刻度 | 百分比整数 | 基点整数 | 基点整数 |
| 容量 | 十进制核，最多 3 位小数 | 毫核整数 | 毫核整数 |
| 时刻 | `seq` 逻辑序号 | `timestampMs` 毫秒 | `at` 逻辑时刻 |
| 节点拓扑 | 扁平节点表 | 扁平节点表 | 按机架分组树 |
| 故障域 | 不提供，按机架派生 | 显式 `faultDomains`/`faultDomainId` | 可省略，按机架派生 |

装载器使用严格 JSON 解码。字段混用、未知字段、版本键冲突、负配额、保底超过 100%、故障域成环、十进制核超过毫核精度等都会返回 `E_xxx` 业务码和 JSON 路径。三版归一化后的租户、节点、故障域、事件、容量和刻度完全一致，并由 SHA-256 指纹校验。

## 账本语义

设集群总容量为 `C`，租户声明 `quotaBasisPoints=q`、`reserveBasisPoints=r`、当前权重 `w`：

```text
baseQuota  = floor(C * q / 10000)
effectiveQ = baseQuota * w
reserve    = floor(baseQuota * r / 10000)
```

保底额固定依赖基础配额，不随权重编辑变化，避免权重调整静默缩小已承诺的保底空间。每个节点只允许集合内租户准入。节点保护容量为：

```text
protected = nodeCapacity - sum(reserve of allowed tenants)
```

准入额度为：

```text
admit = min(request,
            tenantQuotaRemaining,
            protectedFree + tenantOwnUnusedReserve)
```

因此任一租户的突发只能使用共享非保底区或自己的保底余量，不能消费其他租户的保底。超出部分进入该租户等待队列，累计到 `encroached_milli`，`blocked_by` 记录确定性的保底所有者或节点占用来源。队列按租户限长；达到上限返回 `E_BACKPRESSURE_QUEUE`，不会越界放行或静默丢弃。

所有账本步骤检查：

- `used >= 0`
- `used <= quota`
- `available = quota - used >= 0`
- 节点实际占用不超过容量
- 节点非保底占用不超过保护容量
- 租户 placement 之和等于租户已用
- 节点 placement 之和等于节点已用

定点加、减、乘、除使用显式溢出检查；逻辑下限 `max(0, x)` 的饱和次数在运行统计中可见。溢出返回 `E_FIXED_POINT_OVERFLOW`。

## 故障迁移与停顿

节点故障时：

1. 找出该节点上有实际占用的租户，按租户 ID 确定性排序。
2. 目标必须健康、允许该租户、容量足够，且不在故障节点故障域子树中。
3. 目标节点执行虚拟预留，迁移记录约束编号：`fd-avoid:*`、`tenant-allowed:*`、`capacity:*`。
4. 停顿从故障 tick 开始，下一 tick 提交。

停顿期间：

- 原节点 placement 继续计账，保底份额不释放。
- 新节点只有虚拟 incoming 预留，`used_milli` 不增加。
- 该租户新申请进入等待队列，原因 `migration_pause`。
- 提交 tick 先原子移除原 placement，再加入目标 placement；迁移份额不会在两边同时计入。

节点恢复只解除故障状态，不自动回迁，避免抖动。

## 增量失效

编辑类型：

- 租户权重：从编辑 tick 起重算，因为配额观测持续依赖权重。
- 节点容量：从编辑 tick 起重算，因为每 tick 节点观测包含声明容量。
- 节点故障域或故障域父关系：从编辑 tick 后首个故障/迁移依赖 tick 起重算并向后传播。

实现保留每 tick 后状态与依赖集合。首次受影响 tick 之前全部复用缓存，之后按传播闭包重算。报告包含：

- `reused_ticks`
- `recomputed_ticks`
- `recompute_tenants`
- `propagation_closure`
- `tick_sources`

测试和演示均执行 `incremental == FullRecompute` 的逐时刻逐租户深度比较。

## 并发与背压

`ThreadBudget` 是租户工作线程上限，`QueueLimit` 是每租户等待队列上限。

- 重试探测和故障目标探测使用有界 worker pool 并行执行。
- 实际状态提交按租户/迁移 ID 确定性串行合并，因此并发度、调度顺序和批次不改变账本、迁移集合与停顿。
- 每租户耗时写入 `RunStats.PerTenantTiming`，仅用于诊断，不参与正确性判断。
- `ThreadBudget < 1` 返回 `E_BACKPRESSURE_BUDGET`。
- `QueueLimit < 1` 或队列耗尽返回 `E_BACKPRESSURE_QUEUE`。

## SQLite 检查点

检查点表包含：`tenant_state`、`placement`、`node_state`、`domain_state`、`migration_state`、`pause_state`、`tick_commits`、`meta`。

一次 tick 的事务写入顺序：

1. 租户与等待队列
2. placement
3. 节点
4. 故障域
5. 迁移进度
6. 停顿状态
7. `tick_commits` 标记
8. 最后更新全局 `meta.current_tick`

任一阶段失败都回滚整个 tick。恢复时只读取 `current_tick` 对应的已提交快照，并要求 `tick_commits` 存在同 tick 标记。更高 tick 的残留行不会被看见。

恢复规则：

- 版本或工程哈希不匹配：清空整段检查点并重建。
- 文件截断/非 SQLite/损坏：返回 `E_CHECKPOINT_TRUNCATED` 或 `E_CHECKPOINT_CORRUPT`。
- 迁移停顿已提交、迁移尚未提交：从停顿状态继续，下一 tick 重新提交换位。
- 强制终止是真实子进程 `os.Exit(137)`，没有 defer 清理和退出报告；重启只依赖 SQLite 已持久化状态。

## 异常注入

```bash
go run ./cmd/quotaflow inject -kind negative-quota -dir artifacts/demo/samples
go run ./cmd/quotaflow inject -kind fault-domain-cycle -dir artifacts/demo/samples
go run ./cmd/quotaflow inject -kind thread-budget -dir artifacts/demo/samples
go run ./cmd/quotaflow inject -kind checkpoint-truncated -dir artifacts/demo/samples
go run ./cmd/quotaflow run -dir artifacts/demo/samples -queue 1
# tick 6 返回 E_BACKPRESSURE_QUEUE，路径 $.pending.t-beta
```

稳定业务码包括：

| 场景 | 业务码 |
|---|---|
| 负配额 | `E_NEGATIVE_QUOTA` |
| 保底超过配额 | `E_RESERVE_EXCEEDS_QUOTA` |
| 故障域成环 | `E_FAULT_DOMAIN_CYCLE` |
| 未知字段或单位混用 | `E_SCHEMA_UNKNOWN_FIELD` / `E_SCHEMA_MIXED_UNITS` |
| 版本键冲突或未知版本 | `E_SCHEMA_VERSION` / `E_SCHEMA_MIXED_UNITS` |
| 线程预算非法 | `E_BACKPRESSURE_BUDGET` |
| 队列预算非法/耗尽 | `E_BACKPRESSURE_QUEUE` |
| 截断检查点 | `E_CHECKPOINT_TRUNCATED` |
| 检查点版本/工程哈希不匹配 | `E_CHECKPOINT_VERSION` |
| 定点溢出 | `E_FIXED_POINT_OVERFLOW` |

注入只在内存副本上执行，源工程文件哈希保持不变。

## 交互工作台

```bash
go run ./cmd/quotaflow generate -seed 42 -out samples
go run ./cmd/quotaflow interactive -dir samples
```

可用命令：

```text
ticks
show <tick>
edit weight <tenant> <weight> <tick>
edit capacity <node> <milli> <tick>
edit fdnode <node> <domain> <tick>
edit fdparent <domain> <parent|-> <tick>
fullcheck
inject <kind>
checkpoint <path>
resume <path>
quit
```

## 实际验证结果

环境：`go version go1.22.12 linux/amd64`，纯 Go `modernc.org/sqlite v1.34.5`。

执行：

```bash
go test ./...
go run ./cmd/quotaflow demo -seed 42 -out artifacts/demo
```

本次实际结果：

- `go test ./...` 与 `go test -race ./...`：全部包通过。
- 三版样本归一化指纹一致，容量 `12.5` 核精确映射为 `12500` 毫核，配额 `18%` 精确映射为 `1800 bp`。
- 确定性生成：相同 seed 的 `project-v1/v2/v3.json`、`truth.json`、`manifest.json` 字节一致。
- 独立真值：11 个 tick 全部与引擎逐字段相等。
- 场景覆盖：5 次迁移、5 个停顿、2 次节点故障、2 次恢复、故障期申请入队、配额/节点容量等待、权重调整。
- 运行时队列耗尽：`-queue 1` 在 tick `6` 返回 `E_BACKPRESSURE_QUEUE`，未放行或丢弃 `t-beta` 的第二条等待申请。
- 增量编辑：权重编辑复用 tick `1-4`；节点容量编辑复用 tick `1-2`；增量结果与全量重算一致。
- 故障域编辑：将 `n-b2` 改挂到 `rack-a` 后，tick `6` 的 `t-alpha` 迁移目标由 `n-b2` 改为 `n-c1`，增量结果与全量重算一致。
- 强制终止：子进程在 tick 7 的全局提交前以 137 退出，重启观察到 tick 6，保留 2 个停顿迁移，续算最终 tick 与不中断运行一致。
- 异常注入：`E_NEGATIVE_QUOTA`、`E_RESERVE_EXCEEDS_QUOTA`、`E_FAULT_DOMAIN_CYCLE`、`E_SCHEMA_VERSION`、`E_SCHEMA_MIXED_UNITS`、`E_BACKPRESSURE_BUDGET`、`E_BACKPRESSURE_QUEUE`、`E_CHECKPOINT_TRUNCATED`、`E_CHECKPOINT_VERSION` 均稳定返回。

定点容差声明为 0：所有输出均为整数毫核/基点，测试使用精确相等。生成器真值只由 `internal/generator/reference.go` 的独立构造模型产生，不读取引擎输出。

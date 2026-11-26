# 分布式锁租约续约组件说明

本仓库交付一个仅依赖 Go 标准库的领域组件 `leasekit`。确定性样本由固定种子
`20261127` 生成，覆盖三种 schema、窗口边界、双持有者竞争、续约上限、连锁失效、
增量重算、并行调度、SQLite 检查点恢复和异常码演练。

## 快速运行

```bash
# 一键：测试、生成样本、运行演示
./scripts/demo.sh

# 或分步执行
GOPROXY=off go test ./...
go run ./cmd/leasegen -out samples/generated
go run ./cmd/leasedemo demo-output
```

演示成功时会输出：

```text
schema parity=true truth parity=true incremental parity=true parallel parity=true
sqlite=true crash=E_CHECKPOINT_INCOMPLETE recovery=E_CHECKPOINT_INCOMPLETE exceptions=9 stable=true sourceUnmodified=true
```

`samples/generated/` 中包含三个 schema 样本、规范模型和解析真值；
`demo-output/summary.json` 是机器可读的验收摘要。

## 三版本装载

| 版本 | 字段风格 | 时间尺度 | 窗口表达 | 持有者结构 | 失败原因字段 |
| --- | --- | --- | --- | --- | --- |
| `lease.project/v1` | `snake_case` | tick，声明 `ms_per_tick=100` | `renew_window_ticks` 剩余提前量 | 扁平 `holders[]` | 无 |
| `lease.project/v2` | `camelCase` | 整数毫秒 | `renewWindow.remainingTtlMs` 剩余存活量 | `lockTree[].holders[]` 嵌套树 | 无 |
| `lease.project/v3` | `PascalCase` | 精确十进制秒 | `RenewWindow` 的 `OpensAtSecond` / `DeadlineAtSecond` 绝对初始窗口 | `LockTree[].Holders[]` 嵌套树 | 可选 `ExpectedFailureReason` |

入口：

- `leasekit.DetectSchema`
- `leasekit.Load`
- `leasekit.ExportV1` / `ExportV2` / `ExportV3`
- `leasekit.CoreEqual`

加载器使用 `DisallowUnknownFields`。绝对窗口与 TTL 冲突、未知字段、不支持的
schema、负值/越界时间都会返回带 `Code`、`Path`、`Seq`、`HolderID` 的 `*leasekit.Error`，
不静默采用默认值。V3 的 `ExpectedFailureReason` 是一致性注解，不属于资源、持有者和
操作序列的核心身份；`CoreEqual` 会剔除该注解后比较核心工程内容。

## 整型定点时间与窗口口径

内部统一使用 `type Milli int64` 表示整数毫秒。没有 `float32/float64` 进入时间换算：

- tick → ms：`ticks * ms_per_tick`，使用 `checkedMulInt64` 检查溢出；
- 秒 → ms：`DecimalSecondsToMS` 精确拆分整数秒和最多三位小数；
- 到期时刻：`atMS + ttlMS`，使用 `checkedAddMilli`；
- 续约计数：普通 `int` 累加并在参数边界校验，不做静默饱和。

生成样本的 tick 尺度是 `1 tick = 100ms`。测试
`TestWindowBoundariesAndScale` 同时证明错误尺度（例如 99ms/tick）会把
`20 tick = 2000ms` 的下边界移动到 `1980ms`，从而改变判定。

声明的边界策略为 `lower_inclusive_upper_exclusive`：

- `at < expiry-window`：`E_RENEW_TOO_EARLY`
- `at == expiry-window`：受理
- `expiry-window <= at < expiry`：受理（还要通过次数/预算/仲裁）
- `at == expiry`：`E_RENEW_WINDOW_CLOSED`
- `at > expiry`：`E_RENEW_EXPIRED`

## 确定性样本关键场景

| 操作 | 场景 | 期望 |
| --- | --- | --- |
| seq=10 | 1500ms，窗口 2000ms 开启 | `E_RENEW_TOO_EARLY` |
| seq=12 | 2000ms 恰好下边界 | `OK`，到期 5000ms |
| seq=13 vs 16 | 同优先级 7，同窗口竞争 | seq=13 按固定序号获胜，seq=16 `E_COMPETITION_LOST`，保留 3000ms 到期 |
| seq=14 vs 17 | 低优先级更早提交、高优先级更晚提交 | holder-prio-high 凭优先级 10 获胜，低优先级保留 3000ms 到期 |
| seq=18 | 2100ms，租约 2000ms 到期 | `E_RENEW_EXPIRED` |
| seq=25/26 | 第二次续约到达上限 2，随后再次续约 | `E_RENEW_LIMIT_REACHED`，保留 7000ms 到期 |
| seq=22/27 | 后续获取分别遇到 5000/7000ms 屏障 | `E_RESOURCE_HELD`，来源指向对应到期事件 |
| seq=28 | 7100ms 获取 | 已观察 7000ms 失效/释放，获准 |

仲裁规则是 `priority_desc_then_seq_asc`。只有候选窗口存在公共时间重叠
（`max(open) < min(close)`）时才设置唯一胜者；仲裁结构会输出
`temporalOverlap` 和人类可读的 `basis`。

## 失效传播

当前声明的失败策略是 `retain_original_deadline`：竞争落败、窗口外拒绝或达到续约
上限都不会提前延长租约，也不会丢失原到期时刻。资源在整数毫秒到期时产生两个有序
事件：

1. `lease_invalidated`
2. `resource_released`

同一毫秒内按 `(resourceId, holderId, invalidation-before-release)` 排序，保证资源
释放顺序确定。每次获取/续约结果都带 `SourceRef`，说明它依据的持有者、操作序号、
失效事件和到期时刻。真值的 `toleranceMs=0`，即逐项整数毫秒精确一致。

## 增量重算

`IncrementalSession` 首次执行全量并缓存报告。编辑入口：

- `EditRenewWindow(resourceID, windowMS)`
- `EditHolderExpiry(holderID, beforeSeq, expiryMS, renewCount)`
- `EditRenewalResult(seq, ResultOverride{...})`

重算按资源分片执行，并计算依赖闭包：目标持有者链、同窗口仲裁伙伴、之后可能观察
失效/释放的获取，以及这些获取成功后的同持有者后续操作。闭包内采用新结果并标记
`recomputed`；闭包外复用缓存并标记 `cacheHit`。`IncrementalResult.SourcesBySeq`
给出每个操作依据的失效来源。测试将增量合并报告与带相同编辑的一次性全量
`FullRecompute()` 做规范化 JSON 逐项比较。

## 并行处理

`RunParallel` 为每个独立资源启动工作池任务；同一资源内仍走确定性状态机，因此不会
因 goroutine 调度产生竞争裁决偏差。参数：

- `Workers`：允许范围 `[1,64]`
- `AttemptBudgetPerResource`：允许范围 `[1,100000]`
- `BatchSize`：批次切分
- `ReverseSubmission`：反向提交资源任务

每资源的续约请求按操作序号确定预算归属；超出预算的请求得到
`E_ATTEMPTS_EXHAUSTED`，不会越界放行。`ResourceObservation` 记录持有者数、操作数、
续约数和实际 `ElapsedNanoseconds` 诊断耗时。测试用 worker=1/2/4/8、不同批次和反向
提交比较规范化报告，并用 `-race` 检查数据竞争。

## SQLite 检查点

检查点包含：

- 全局 `committed_seq`
- `version` 与 `schema_version`
- 每持有者 `renew_count`、`expires_at_ms`、`last_seq`
- SHA-256 `checksum`

提交顺序由 `CommitCheckpoint` 固定：

1. `WriteHolderStates`：先持久化全部持有者续约状态，行内带 `state_seq`；
2. `WriteCommittedSeq`：最后更新全局 `committed_seq`。

如果在两步之间崩溃，SQLite 中会出现 `state_seq > committed_seq`。重启加载返回
`E_CHECKPOINT_INCOMPLETE`，`RecoverOrRecompute` 不信任这些未提交状态并整体重算。
版本或 schema 不匹配返回 `E_SCHEMA_VERSION_MISMATCH`，同样触发整体重算。截断的
SQLite/JSON 返回 `E_CHECKPOINT_TRUNCATED`，校验和损坏返回 `E_CHECKPOINT_CORRUPT`。

本环境没有第三方 Go SQLite 驱动。`sqlite_checkpoint.go` 使用 CGO 链接系统已有的
`libsqlite3.so.0`，`sqlite_driver.go` 在仓库内实现一个最小
`database/sql/driver`（驱动名 `lease-sqlite3`），因此检查点读写仍经过标准库
`database/sql`。设置 `CGO_ENABLED=0` 时可纯标准库编译，SQLite 专属测试会跳过，
文件检查点仍可用。

## 稳定错误码

| 错误码 | 含义 |
| --- | --- |
| `E_DUPLICATE_HOLDER` | 持有者标识重复 |
| `E_DUPLICATE_RESOURCE` / `E_DUPLICATE_OPERATION` | 资源或序号重复 |
| `E_UNKNOWN_HOLDER` / `E_UNKNOWN_RESOURCE` | 引用不存在 |
| `E_INVALID_SCHEMA` / `E_SCHEMA_VERSION_MISMATCH` | schema 无法识别、严格字段错误或版本不匹配 |
| `E_FIELD_CONFLICT` / `E_FIELD_OUT_OF_RANGE` | 跨字段语义冲突或参数越界 |
| `E_INVALID_DURATION` / `E_TIME_CONVERSION` / `E_COUNT_OVERFLOW` | 非法时间、换算失败或溢出 |
| `E_RENEW_TOO_EARLY` | 未到窗口上沿 |
| `E_RENEW_WINDOW_CLOSED` | 撞上声明为排他的到期相等边界 |
| `E_RENEW_EXPIRED` | 已超过到期时刻 |
| `E_RENEW_LIMIT_REACHED` | 超过最大续约次数 |
| `E_COMPETITION_LOST` | 同窗口仲裁落败 |
| `E_RESOURCE_HELD` | 资源仍被占用 |
| `E_ATTEMPTS_EXHAUSTED` | 每资源尝试预算耗尽 |
| `E_WORKER_BUDGET_OOR` / `E_ATTEMPT_BUDGET_OOR` | 并行预算越界 |
| `E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_CORRUPT` / `E_CHECKPOINT_INCOMPLETE` | 检查点截断、损坏或崩溃在提交间隙 |

## 实际验证结果

验证环境：

- Go `go1.22.12 linux/amd64`（`go.mod` 声明 Go 1.21）
- 系统 SQLite 运行库 `3.46.1`
- `GOPROXY=off`，无第三方模块

已执行并通过：

```text
GOPROXY=off go test ./...
ok  leasekit

GOPROXY=off go test -race ./...
ok  leasekit 1.122s

CGO_ENABLED=0 GOPROXY=off go build ./...
# 成功，无输出

GOPROXY=off go vet ./...
# 成功，无输出

./scripts/demo.sh
schema parity=true truth parity=true incremental parity=true parallel parity=true
sqlite=true crash=E_CHECKPOINT_INCOMPLETE recovery=E_CHECKPOINT_INCOMPLETE exceptions=9 stable=true sourceUnmodified=true
```

测试覆盖 12 个顶层用例，包括三版本样本文件装载、窗口边界、尺度敏感性、优先级/平局
仲裁、连锁失效、两类增量编辑、并行调度确定性、文件/SQLite 检查点、异常注入和源工程
不可变性。生成器曾在两个全新临时目录分别产出样本，`diff -ru` 逐字节一致。

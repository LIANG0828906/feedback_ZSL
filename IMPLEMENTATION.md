# 回合制对战规则回放验证内核 · 技术说明

## 1. 目录与职责

| 路径 | 职责 |
| --- | --- |
| `internal/model` | 领域模型、类型化错误码（`E_SCHEMA/E_ORDER/...`）、定点整型与溢出边界 |
| `internal/prng` | splitmix64 确定性随机源；子随机源仅由“对局种子+回合号”派生 |
| `internal/gen` | 固定种子样本生成器，独立实现结算规则产出回放与真值；异常样本构造 |
| `internal/engine` | 加载校验、确定性结算、冲突裁决、窗口状态机、自适应算法、三路径运行器、比对器、有界并发池 |
| `internal/store` | `modernc.org/sqlite` 纯 Go 驱动的原子检查点、哈希链、只读查询 |
| `internal/verify` | 独立复核工具，只读 SQLite + 真值指纹，不 import `engine` |
| `cmd/demo` | 可重复演示管线 |
| `cmd/verify` | 独立复核 CLI |

## 2. 定点化与确定性

- 全部生命、资源、技能威力均为 `int64` 毫单位（`Milli=1000`），无浮点。
- 物理溢出边界为 `int64` 上下限，`AddChecked/MulChecked/ScaleMilli` 命中即返回 `ErrOverflow`。
- 领域安全边界 `SafeLimit=10^15`，由 `WithinSafeLimit` 声明；HP/MP 结算后收敛到 `[0,Max]`。
- 每回合随机源 `prng.New(prng.TurnSeed(matchSeed, turnNo))`，与 goroutine 标识、回合完成顺序无关。
- 暴击固定 15%（千分 150），倍率 1.5（`ScaleMilli(x,1500)`），仅攻击技能消耗一次随机抽取。

## 3. 结算规则与冲突裁决

单回合顺序：冷却统一推进 1 拍 → 按回放 `order` 逐单位处理 → 资源消耗/伤害治疗/冷却登记 → 回合末资源回复与胜负判定。

前置校验违规按固定优先级裁决（不依赖 map/遍历顺序）：
1. `insufficient_resource`
2. `cooldown_active`
3. `target_dead`

多条违规同时存在时全部进入裁决明细，首要原因取优先级最高者。无任何违规却被判失败时返回 `E_CONFLICT`，并把 phantom 裁决明细持久化。

## 4. 窗口状态机与提交边界

合法迁移表：

```
pending -> running -> committed
                   \-> failed -> pending（重试）-> running -> committed
```

- 非法迁移返回 `E_STATE` 且保持原状态；已提交窗口无迁出边（冻结）。
- 窗口固定大小、连续不重叠；重叠划分返回 `E_STATE` 并指出窗口编号。
- 进度以“已提交窗口数/总窗口数”表示。

## 5. SQLite 原子检查点与恢复

每个窗口在单事务内写入：逐回合快照、裁决明细、窗口记录（行动偏移、RNG 抽取数、末回合子随机种子、内容哈希、参数/算法版本）、状态迁移记录。

- 连接级串行（`SetMaxOpenConns(1)` + 互斥锁），写入不交错、窗口编号严格递增。
- 哈希链：`chain_n = ChainHash(chain_{n-1}, content_hash_n)`，创世值 `store.RootChain`。
- 恢复时回放哈希、真值指纹、参数版本、算法版本、窗口大小任一不匹配，整体作废并从回放起点重算，禁止部分混用。

## 6. 自适应算法

- `fine_grained`：保留全量逐回合工作集。
- `window_pre_aggregate`：按窗口预聚合紧凑表 + 修正阶段校验，内存随窗口数增长。
- 两条路径共用同一结算原语，仅内存布局不同，逐字段结果必须一致（有专门比对测试）。
- 决策依据（估算内存/时间与预算比较）写入 `decisions` 表并在演示中打印。

## 7. 三条路径与独立比对

- live：首次边结算边提交。
- recovery：复制 live 的磁盘 DB 模拟重启，校验通过后续跑，未提交窗口重新结算。
- full：作废全部检查点后一次性从头重放。
- `engine.CompareStores` 只读两个 SQLite 库，比对快照/裁决/窗口序列，定位首个不一致回合、字段、取值，不一致返回 `E_DIFF`。
- `internal/verify` 不经过结算主路径，逐字段核对 HP/MP/冷却/胜负并输出首个违规回合与违规项。

## 8. 异常注入链路

| 注入 | 错误码 | 定位 |
| --- | --- | --- |
| 回放损坏 | `E_DECODE` | JSON 解析偏移 |
| 时序倒置/跳号 | `E_ORDER` | 字段路径 + 期望/实测回合号 |
| 无违规却失败 | `E_CONFLICT` | 窗口 + 回合 + 单位/技能 |
| 单窗口内部失败 | `E_STAGE` | 窗口 + 回合；窗口置 failed，可重试且不污染其他窗口 |

## 9. 并发资源边界

并行只发生在对局之间，单局内部严格串行。`engine.Pool` 以带缓冲信号量限制最大并行度与每线程缓冲上限（容量 `MaxParallel*WorkerBufferCap`），缓冲耗尽时 `Submit` 阻塞排队，不丢弃任务。

## 10. Windows 工作区说明

代码仅使用 `filepath.Join`、相对路径与 `os.*` 标准接口，无路径分隔符硬编码，已通过 `GOOS=windows GOARCH=amd64 go build ./...` 交叉编译验证。在 Windows PowerShell 中于项目根目录执行相同的 `go` 命令即可，`samples/`、`db/`、`logs/` 均为项目根下本地相对目录。

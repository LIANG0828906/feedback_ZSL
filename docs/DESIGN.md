# 战斗技能资源预算与冷却排程 — 技术说明

## 1. 范围与约束

- 纯 Go 实现（Go 1.22），无 Docker、无外部数据库服务、无前端依赖。
- 持久化仅使用 `database/sql` + 纯 Go 驱动 `modernc.org/sqlite`。
- 样本、施放意图与逐 tick 真值全部由仓库内确定性生成器按固定种子
  （默认 `20240517`）产出，运行时不访问任何线上战斗服或账号。
- 全链路统一内部口径：时间为逻辑 tick（`fixed.TickMillis=100ms`），
  比例为万分比（`fixed.InternalScale=10000`），全部 `int64` 运算，禁止浮点累积。

## 2. 包结构

| 包 | 职责 |
| --- | --- |
| `internal/errx` | 统一结构化错误码与诊断字段（字段路径、版本、节点、操作数、tick 等） |
| `internal/fixed` | 千分比/万分比、tick/毫秒整型换算，取舍方向、残差记录、溢出 E_OVF、整型开方 |
| `internal/expr` | 表达式树与扁平常量表两种形态、整型求值、E_EVAL、线性折叠/降级 E_LOSSY |
| `internal/model` | 内部统一规则模型、规范化内容哈希、深拷贝与索引 |
| `internal/schema` | v1/v2/v3 装载、字段级转换痕迹、升级默认补齐、降级损失报告与再导出 |
| `internal/gen` | 固定种子 LCG 生成规则/意图，并用独立参考模拟构造逐 tick 真值 |
| `internal/engine` | 逐 tick 回复/冷却/结算、确定性争用裁决、双层状态机、三路径调度 |
| `internal/depgraph` | 节点依赖图、组合缓存键、编辑后精确 tick 区间失效传播 |
| `internal/checkpoint` | SQLite 单连接串行事务原子窗口检查点与重启版本校验 |
| `internal/pool` | 受限预算 goroutine 池（并行度/缓冲上限/阻塞排队诊断/异常隔离） |
| `internal/web` | 标准库 `net/http` 服务端渲染诊断工作台 |
| `cmd/skillsched` | CLI：`serve` / `demo` / `version` |

## 3. 三个 Schema 版本差异

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 字段命名 | camelCase | snake_case | PascalCase |
| 时间单位 | 毫秒 | 毫秒 | tick |
| 冷却缩减刻度 | 千分比 | 千分比 | 万分比 |
| 回复/缩减表达 | 扁平常量表 | 扁平常量表 | 表达式树 |
| 共享冷却组 | 无字段 | 有 | 有 |
| 冷却缩减上界 | 无 | 无 | 有 |

未知版本号或必填字段缺失返回 `E_SCHEMA`，错误体带 `source_version` 与
`field_path`。

### 升级与降级

- 升级（v1→内部、v2→内部）按声明默认值补齐：
  - 共享冷却组缺失 → 空组，并记录 `shared_groups` 默认补齐；
  - 冷却缩减上界缺失 → `DefaultCDRCapPermyriad=5000`。
  每次补齐记录字段路径、旧值（`<absent>`）与来源默认值。
- 降级（内部→v1/v2）对无法表达项生成 `E_LOSSY` 清单：
  - 非线性表达式树无法折叠为常量表 → `collapse_to_constant`；
  - 共享组导出到 v1 → `dropped`；
  - 冷却缩减上界导出到 v1/v2 → `dropped`。
  导出产物可被对应版本装载器读回，除被降级字段外全部复原。

## 4. 定点与时间换算

- `Scale(value, fromScale, toScale, round)`：先乘后除，纯 `int64`。
- 取舍方向：`down`（向零截断，默认）、`up`、`near`；每次发生余数都写入
  `fixed.Truncation`（位置、除数、商、残差）。
- 乘法/加法溢出返回 `E_OVF`，带 `field`、`operand_a`、`operand_b`、`op`。
- 生成器只产出 10 的倍数的比例系数，因此
  万分比→千分比→万分比与 tick→毫秒→tick 往返取值和刻度完全一致；
  非整除输入的残差行为由 `internal/fixed` 单测固定。

## 5. 排程与裁决

每个 tick 的固定顺序：

1. 技能状态机迁移到“待结算”；
2. 冷却与共享组锁各推进 1 tick；
3. 资源池按回复表达式（无浮点）回复并夹取到 `[0, capacity]`；
4. 本 tick 意图按 `(priority, skillID, seq)` 稳定排序，逐技能前置校验；
5. 成功者扣资源、写冷却、加共享组锁并抑制同组其余成员；
6. 汇总每技能可用状态、冷却剩余、池余量、状态与裁决明细。

- 冷却缩减：`reduce = cooldown * min(cdr, cdr_cap) / 10000`（向下取整），
  保证剩余冷却非负且不超过声明上界。
- 争用裁决不依赖 map 遍历或线程标识；同 tick 对同一技能出现无法消解的
  重复意图时返回 `E_CONFLICT`，并通过窗口快照回滚保留该 tick 原状态。
- 表达式树与扁平常量表在同一规则、同一环境变量下求值结果逐 tick 相同；
  除零、负数开方返回 `E_EVAL` 并指出节点 ID 与两个操作数。

## 6. 状态机

- 技能五态：`pending → ready → settling → committed`，任意阶段可 `failed`，
  `failed → ready` 允许重试；`committed` 为终态，非法迁移返回 `E_STATE` 且保持原态。
- 窗口四态：`pending → scheduling → committed`，可 `failed → pending` 重排。
- 每个窗口在单个 SQLite 事务内整体写入技能状态、资源余量、冷却剩余、
  意图偏移、内容哈希、源版本、参数版本与算法版本；单连接 + 互斥保证写入不交错，
  窗口索引必须单调递增。重启时任一版本/哈希不匹配返回 `E_ENTRY` 并整窗作废。

## 7. 依赖图与失效传播

- 节点：CDR 系数、资源回复表达式、共享组、逐 tick 结算结果。
- 缓存键 = 节点标识 + 节点内容哈希 + 参数版本 + 上游缓存键的组合哈希。
- 编辑（冷却/共享组/刻度）声明受影响 tick 区间，仅失效该区间内的下游 tick
  节点并输出含节点标识与区间的失效清单；区间外缓存条目继续复用。

## 8. 三条执行路径一致性

- `RunFull`：一次性全量重算（基准）。
- `RunRealtime`：逐窗口实时排程，走完整窗口状态机。
- `RunResume`：从检查点运行态副本按窗口边界对齐续算，不改写检查点。
- `RunConcurrent`：受限预算线程池重算独立窗口，按窗口索引归并，结果与串行一致。

`Compare` 自动定位首个不一致的 tick、技能标识与字段名。

## 9. 错误码一览

| 码 | 触发场景 |
| --- | --- |
| `E_SCHEMA` | 未知版本、必填字段缺失、非法 JSON |
| `E_LOSSY` | 降级无法表达表达式树/共享组/上界 |
| `E_OVF` | 整型换算溢出（带字段与两操作数） |
| `E_EVAL` | 除零、负数开方、未绑定变量 |
| `E_CONFLICT` | 同 tick 无法裁决的争用，保留原状态 |
| `E_STATE` | 技能/窗口状态机非法迁移、窗口乱序 |
| `E_STORE` | SQLite 打开/事务/读写失败 |
| `E_ENTRY` | 重启检查点版本/哈希不匹配，整窗作废 |

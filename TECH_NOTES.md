# 对局回放审计内核 — 技术说明

纯 Go 1.22 实现，唯一第三方依赖是纯 Go 的 `modernc.org/sqlite`（无 CGO，
可 `GOOS=windows` 交叉编译）。所有样本由固定种子 `428691` 在本地确定性生成。

## 目录结构

- `internal/errs`：稳定错误码 `E_FORMAT / E_SCHEMA / E_OVF / E_CONFLICT` 与字段路径。
- `internal/model`：归一事件时间轴、状态、参数/版本、报告、确定性哈希。
- `internal/timeconv`：回合序号 ↔ 毫秒的纯整型换算（含溢出检查）。
- `internal/formats`：三种发行格式 DTO、严格解码、三格式合并、回放包加载。
- `internal/gen`：固定种子样本生成器 + 由构造记账得到的解析真值。
- `internal/engine`：裁决、确定性结算、依赖图、精确失效、增量服务、比对器、
  有界 worker pool、SQLite 区段续算运行器。
- `internal/store`：SQLite 检查点存储（事务原子写 + 互斥串行写）。
- `internal/audit`：23 项端到端验收检查。
- `cmd/replayaudit`：CLI（`all` / `gen [dir]`）。

## 三格式差异矩阵

| 维度 | Format 1 `RND-CAST` | Format 2 `MSQ-TARGET` | Format 3 `MSQ-FULL` |
| --- | --- | --- | --- |
| 时间字段 `at` | 1 基回合序号 | 毫秒时间戳 | 使用 `at_ms` 毫秒 |
| `chain` 列表语义 | 施法者序列 | 目标序列（互换） | 无歧义的 `caster_seq/target_seq` |
| 条件事件 | 有 `cond_flag/cond_want` | 不编码 | 有 |
| 互斥约束 | 有 `mutex_groups` | 无 | 无 |
| 强制同步点 | 无 | 有（ms） | 有（ms） |

合并加载器按稳定事件 ID 对齐三份描述，校验时间/序列/数值一致，取并集补全
条件、互斥与同步；每个事件保留 `source_formats:[1,2,3]` 与字段级来源
`field_source`。未知 `format` 号 → `E_FORMAT`（路径 `format`）；必填缺失 →
`E_SCHEMA`，路径形如 `events[2].at`。

## 时间换算声明

参数：`ms_per_turn=3000`，`anchor=start`，`round=floor`。

- 回合→内部毫秒：`ms=(turn-1)*3000`（`anchor=start`），全程 `int64` 检查乘法溢出。
- 毫秒→回合：`turn=floor(ms/3000)+1`，另有 `ceil/nearest` 的纯整型实现。
- 生成器发出的毫秒值全部是回合锚点，因此高（ms）→低（turn）→高（ms）为
  严格恒等，生效时刻与 `(at,id)` 结算次序和直接加载完全一致；非对齐输入则
  体现声明的 floor 量化且幂等。溢出返回带事件 ID 的 `E_OVF`。

## 确定性结算规则（algo-1.0）

1. 规范次序：`(at ASC, id ASC)`，ID 零填充保证字典序即构造序，与 map/random 无关。
2. 标志裁决：同名标志多声明时高 `priority` 胜出；最高优先级仍存在矛盾值则不可裁决。
3. 条件事件：`flag == want` 才参与，否则 `skipped_cond`。
4. 互斥：同一 `at` 且条件成立的候选中，`priority` 大者胜，平手取 ID 字典序小者；
   负者 `suppressed` 并写明细。若要压制的是 `mandatory` 事件 → `E_CONFLICT`。
5. 效果：`damage/heal` 改 `hp:<target>`，`gain/spend` 改 `mp:<caster>`，
   技能写 `cd:<caster>|<skill>=at+cooldown`；HP/MP 折叠到 `[0,max]`。
6. 单事件原子性：克隆累积状态后应用全部效果，算术溢出/注入故障则丢弃克隆，
   事件置 `failed`，既有状态与已提交区段不被污染；可经编辑/重试恢复。
7. 强制同步：同步点前 `at <= sync.at` 的事件必须全部离开 `failed`；否则逐条
   记录 `sync_pending` 并返回指向该事件的 `E_CONFLICT`。
8. 胜负：仅两方时按阵营全灭判定 `blue/red/draw`，否则为空。

## 依赖图与精确失效

显式节点：`event:*`、`flag:*`、`mutex:*`、`sync:*`、`segment:*`。

- `flag → 条件事件`；`event ↔ mutex 组 ↔ 全部成员`。
- 资源键上的真 RAW 依赖：同一 `hp:/mp:` 键“最后写入者 → 后续访问者”。
- `event/sync → segment`，且 `segment n → segment n+1`（累积状态依赖）。

编辑后在**新图**上从种子做确定性 BFS：只把到达的事件标记失效并重算
`Outcome`（整值原子替换缓存），未到达事件复用缓存；清单逐项给出
`id/kind/reason/invalidated/reused`，并分别列出失效/复用区段。时间编辑会
额外显式置脏旧区段。区段因保存累积状态，其后续区段会沿区段链失效，
但区段内无事件依赖的事件结果仍标记为复用。

## 三路径一致性与比对器

- 连续路径：`SettleFull` / `SettleFullPooled`。
- 增量路径：`Service.RunFull` 后逐次 `ApplyEdit`。
- 断点路径：`RunCheckpointed` 每段提交 SQLite，可注入 `ErrInterrupted` 模拟崩溃，
  重开库后续算。

`engine.CompareReports` 逐项比较：事件 ID 序列、`effective_at`、`order_index`、
`segment`、`status`、逐实体 `state.hp/mp/cd.*`、同步点集合、提交序列
（区段号、`commit_seq`、事件列表、state/segment 哈希）与胜者；返回首个
`{event_id, field, want, got}`。

## SQLite 检查点

每区段一行 `segment_checkpoints`（有序事件结果 + 事件 ID 集合 + 区段末累积
`State` 的 JSON 载荷），`checkpoint_meta` 保存 `content_hash`、
`format_version`、`params_version`（声明版本 + 参数哈希后缀）、`algo_version`。
`CommitSegment` 在单个事务内 upsert 区段与 meta，互斥锁 +
`SetMaxOpenConns(1)` 保证写不交错；运行器按区段索引升序提交，`commit_seq`
连续稳定。重启时 `CheckCompatible` 发现内容哈希或任一版本不符即删除该回放
全部行，禁止新旧区段混用；相符则从最后连续区段恢复累积状态。

## 并发资源边界

`engine.Pool`：固定 worker 数（`params.max_parallel`），每 worker 一个容量
为 `params.worker_buffer` 的缓冲 channel。提交先尝试任意非满队列；全部缓冲
耗尽时先记录一条 `BlockRecord{seq,task,worker,reason:"worker_buffer_full"}`
再阻塞入队，绝不丢弃任务。测试验证任务零丢失、峰值并行不越界、背压记录与
阻塞/排空行为。检查点写入由 store 互斥锁串行化。

## 错误码

| 码 | 触发 |
| --- | --- |
| `E_FORMAT` | 非法 JSON、未知格式号、不可读文件 |
| `E_SCHEMA` | 必填字段缺失、路径/类型/取值非法、跨格式不一致 |
| `E_OVF` | 时间换算或结算整型溢出（带事件 ID/字段路径） |
| `E_CONFLICT` | 强制型互斥被压制、最高优先级标志矛盾、同步点前仍有失败事件 |

可解释但可裁决的冲突不返回错误，而是在 `report.conflicts[]` 输出
`type / primary_event_id / related_ids / decision / detail`。

## 常用命令

```bash
go mod download
go vet ./...
go test -count=1 ./...
go run ./cmd/replayaudit all     # 生成 samples/ 并跑全部验收，写 artifacts/verification-report.json
go run ./cmd/replayaudit gen samples
```

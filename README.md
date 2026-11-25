# 多路对白口型包帧预算仲裁内核

这是一个仅依赖 Go 标准库与纯 Go SQLite 驱动 `modernc.org/sqlite` 的本地领域组件，同时提供可重复运行的诊断页面。它覆盖三版本扬声器工程装载、显式升级/降级、整数单位换算、逐帧预算仲裁、抢占恢复、依赖失效、SQLite 原子检查点、有界并行和故障隔离。

## 快速运行

环境已验证：

```text
Go: go1.22.12 linux/amd64
模块: lipsync
SQLite: modernc.org/sqlite v1.34.5（纯 Go）
```

```bash
go test ./...
go test -race ./...
go vet ./...

# 一次性输出完整诊断 JSON
go run ./cmd/lipsync-diag -once -fresh -seed 42

# 同时写出三版原文、真值、损失报告
go run ./cmd/lipsync-diag -once -fresh -dump-dir artifacts/generated

# 本地回环诊断页（只允许 loopback）
go run ./cmd/lipsync-diag -addr 127.0.0.1:8099 -fresh
```

页面地址为 `http://127.0.0.1:8099`。常用参数：

```text
-seed 42               固定样本种子
-db artifacts/...db    本地 SQLite 文件
-fresh                 启动前清理同名数据库
-window 4              每个提交窗口的帧数
-workers 4             最大工作线程数
-queue 8               待处理帧任务队列预算
-candidate-buffer 3    每工作线程候选包缓冲上限
-dump-dir <dir>        写出生成样本、真值与损失报告
```

诊断 API：

```text
GET /api/state                      完整状态、逐帧结果、版本、失效与故障报告
GET /api/invalidation?kind=budget   预算编辑失效报告
GET /api/invalidation?kind=order    仲裁顺序编辑失效报告
GET /api/invalidation?kind=recovery 恢复策略编辑失效报告
GET /api/fault?kind=decode          工程损坏：E_DECODE
GET /api/fault?kind=order           帧序倒置：E_ORDER
GET /api/fault?kind=cap             槽位/预算矛盾：E_CAP
```

## 目录

```text
domain/       内部规范模型、五态窗口、结构化错误码、内容哈希
units/        采样点/毫秒、千分比/万分比整型换算与截断记录
formats/      v1/v2/v3 装载、升级补全、显式降级与 E_LOSSY 清单
scenario/     固定种子生成器和独立参考真值构造
arbitration/  逐帧仲裁、抢占恢复、错误/并行帧推进
deps/         依赖图、缓存键、实际差异失效与持久化复用
parallel/     有界工作线程池、帧队列和候选缓冲背压
checkpoint/   SQLite 单事务窗口检查点与帧缓存
engine/       状态机驱动、三路径运行、失败重试和提交序列
diag/         本地诊断状态、HTTP API 和页面
cmd/lipsync-diag/
```

## 三版本映射

| 版本 | 字段命名 | 时间基准 | 预算刻度 | 通道表 | 抢占字段 |
|---|---|---|---|---|---|
| `v1` | snake_case | 采样点 | 千分比 | 扁平通道表 | 无 |
| `v2` | camelCase | 毫秒 | 万分比 | 按场景分组树 | 部分 |
| `v3` | snake_case | 采样点 | 万分比 | 按场景分组树 | 完整 |

内部模型统一为：

- 时间：整数帧；
- 预算：整数万分比（basis points）；
- 通道：扁平内部通道表，场景作为来源元数据保留；
- 恢复：`next_frame`、`next_window`、`fixed_delay` 三种显式策略；
- 来源：`SchemaVersion` 始终保留。

低版本升级会把缺失字段追加到 `DefaultFills`，记录字段路径、旧值、来源默认值和来源版本。`v1` 的恢复策略与恢复偏移、`v2` 的恢复偏移与放弃许可都走该路径。

降级导出返回独立 `LossReport`：

- `Code = E_LOSSY`；
- 每项包含字段路径、通道、包、旧值、导出值、处置方式；
- `v1` 明确记录场景树扁平化和抢占策略丢弃；
- `v2` 对无法表达的恢复偏移、放弃许可逐项记录；
- 所有余数同时写入 `Truncations`。

预算从万分比降到千分比时使用声明方向 `floor`。例如 `4567 bp -> 456‰ -> 4560 bp`，残差 `7`，不超过一个千分比；该残差在报告和测试中均可审计。

## 换算规则

```text
samples -> frames = samples * frameRate / sampleRate
millis  -> frames = millis  * frameRate / 1000
permille -> bp    = permille * 10000 / 1000
bp -> permille    = bp * 1000 / 10000
```

全链路不使用浮点。乘除、取整和余数均通过 `int64`/大整数溢出检查完成；非零余数逐处生成 `Truncation`。溢出返回 `E_OVF`，并带 `field`、`operand_a`、`operand_b`。

## 确定性样本与真值

`scenario.Canonical(seed)` 在帧/万分比整数域内构造 3 个通道、8 路对白。固定种子 `42` 的样本有：

- 12 个逐帧结果；
- 2 次抢占；
- 1 次实际恢复；
- 1 次窗口不足放弃；
- 3 个窗口提交。

生成器真值由 `scenario.referenceArbitration` 独立构造，不导入生产 `arbitration` 包，也不从引擎输出反推。测试逐字段比较 `Frames`、`Preemptions`、`Resumes`、`Abandonments`、`PacketStates`。

候选决策顺序固定为：

1. 对话优先级降序；
2. 同优先级时当前正在下发包优先；
3. 起始帧升序；
4. 包标识升序。

通道输出始终按工程声明的 `arbitration_order` 重组，与工作线程完成顺序无关。

## 仲裁与恢复

每帧对每个通道同时校验：

- 占用槽位数不超过 `slot_limit`；
- 已用预算不超过 `frame_budget_basis_points`；
- 每个包成本不超过通道预算。

高优先级候选无法直接装入时，可从低优先级开始抢占当前占用包。每个 `PreemptEvent` 记录：

- 被抢占包与触发包；
- 已下发帧数、剩余帧数；
- 计划恢复帧号。

恢复策略：

- `next_frame`：不早于下一帧；
- `fixed_delay`：不早于 `preempt_frame + recovery_offset_frames`；
- `next_window`：先按偏移计算，再对齐到下一个窗口边界。

恢复后设置 `ProtectedUntil`，保护区间内不允许再次被抢占，因此连续下发帧数满足声明的最小连续可读帧数。当剩余帧数本身少于最小连续帧数时，要求值取 `min(remaining, min_continuous_frames)`。

恢复窗口不足时：

- `allow_abandon = true`：记录 `AbandonEvent`，包含帧号、原因、可用窗口、所需帧数；
- `allow_abandon = false`：返回 `E_RESUME`，包含包标识、可用窗口和所需帧数。

## 依赖图与精确失效

图中参数节点包括：

- 每个通道的槽位与帧预算；
- 全局仲裁顺序；
- 全局恢复策略。

包节点和逐帧通道结果节点显式保存上游节点。缓存键输入包括：

```text
对白内容哈希 + 包标识 + 通道标识 + 仲裁参数版本 + 有序上游缓存键
```

编辑预算、顺序或恢复策略后，失效器会重新运行确定性仲裁，并逐帧比较：

- 受理、延后、抢占、恢复集合；
- 槽位占用和预算；
- 包状态中的恢复帧、抢占帧、放弃帧等下游状态。

只有实际变化帧被标记失效；未变化帧从 `Store` 重新验证并复用持久化载荷。预算编辑只影响 `spk_main` 的实际变化帧；顺序编辑影响每帧的聚合通道顺序；恢复策略编辑会传播到恢复帧和重新判定为延后的下游帧。报告包含通道标识、包标识、连续帧区间、复用数和 store 命中数。

## 窗口状态机与 SQLite

五态：

```text
pending -> arbitrating -> preempting -> committed
                      \-> failed -> pending (显式重试)
```

非法迁移返回 `E_STATE`，状态和进度保持不变。

每提交一个窗口，以下数据在同一个 `BEGIN IMMEDIATE` 事务中写入：

- 窗口范围、已提交状态、单调提交序号；
- 逐帧通道受理/延后/抢占/恢复集合；
- 抢占事件的已下发余量、恢复帧；
- 恢复与放弃事件；
- 最新包状态快照；
- 已消费对白帧偏移；
- 内容哈希、仲裁参数版本、算法版本。

存储层使用固定单连接和进程内互斥串行化写入，提交序号唯一且严格递增。窗口重复提交是幂等的，不重复写事件。

三条路径：

1. 实时运行：直接逐窗口推进并写 SQLite；
2. 断点恢复：提交前 N 个窗口后重建引擎，从最新包状态继续；
3. 一次性全量重算：直接比较生产仲裁结果与生成器真值。

验收测试要求三者逐帧集合、恢复帧号、窗口提交序列完全一致。

## 并行与异常隔离

- 帧任务队列容量受 `queue_budget` 限制；队列满时发送方阻塞，并记录 `frame_queue_budget` 原因与等待时长。
- 每工作线程的候选包缓冲受 `candidate_buffer` 限制；超限候选分块校验，块间背压时长写入 `candidate_buffer` 事件。
- 结果不按完成顺序拼接，而是按 `arbitration_order` 恢复稳定顺序。
- 任一通道错误会先把运行态回滚到窗口开始，窗口进入 `failed`；已提交窗口不受影响。
- 调用方再次执行 `Run` 即显式重试。成功提交后，失败记录标记为 `retried/recovered`，失败通道集合清空。
- 共享检查点写入由 SQLite store 串行化，窗口序号稳定。

三类注入链路：

| 注入 | 错误码 | 典型路径 |
|---|---|---|
| 损坏 JSON | `E_DECODE` | `$` |
| 帧序倒置 | `E_ORDER` | `frame_sequence` |
| 单包成本高于通道预算 | `E_CAP` | `packets[n].cost` |

## 本次实际验证结果

已执行：

```text
go version
  go version go1.22.12 linux/amd64

go test ./...
  全部包通过

go test -race ./... -count=1
  全部包通过，无数据竞争

go vet ./...
  通过
```

固定种子 `42` 的 `go run ./cmd/lipsync-diag -once -fresh` 实测：

```text
truth_matches        true
frames               12
preemptions          2
resumes              1
abandonments         1
commit_sequence      [1 2 3]
max_parallel         <= 4
v1 default_fills     5
v2 default_fills     2
v3 default_fills     0
v1 loss items        4
```

编辑失效实测：

```text
budget:   9 帧失效，18 帧复用，失效包 5 个
order:    36 帧失效（所有帧的聚合通道顺序变化）
recovery: 8 帧失效，28 帧复用，失效包 5 个
```

故障接口实测：

```text
decode -> E_DECODE path=$
order  -> E_ORDER  path=frame_sequence
cap    -> E_CAP    path=packets[0].cost channel=spk_main packet=pkt-d1-main-low
```

本地 HTTP 验证：

```text
GET /healthz                       -> ok
GET /                              -> 200，包含逐帧仲裁与异常注入区
GET /api/state                     -> 200，JSON 状态
GET /api/fault?kind=cap            -> E_CAP
GET /api/invalidation?kind=budget  -> 200，含失效帧与包清单
```

未使用 Docker、真实配音设备、账号或外部站点；仅从 Go module proxy 获取了纯 Go SQLite 依赖。

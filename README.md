# dialoguecal

游戏对白响度校准与逐帧声学特征内核。仓库从空目录实现，仅使用 Go 标准库；
固定种子生成器同时产出单声道 PCM、音素标注、工程参数和逐帧解析真值。演示与测试
不依赖外部样本、数据库服务、CGO 或第三方包。

## 快速开始

要求 Go 1.21 或以上版本。

```bash
go test ./...
go vet ./...
go run ./cmd/...
# 或指定种子/输出目录
go run ./cmd/... -seed 42 -out /tmp/dialoguecal-demo
```

默认演示种子为 `20261127`，产物写入 `artifacts/demo/`：

```text
dialogue-v1.json            扁平片段表、毫秒、千分比刻度
dialogue-v2.json            对白嵌套片段、采样点、万分比刻度
dialogue-v3.json            场次分组嵌套、校准标记、内部同刻度
lossy-v1.json / lossy-v2.json  E_LOSSY 明细（组织展平、标记降级）
truth.json                  生成器构造的逐帧解析真值
checkpoints.db              主运行与增量编辑的原子检查点
crash-checkpoints.db        强制终止/恢复演示的检查点
report.json                 实际验收结果
```

当前默认样本的实跑输出：

```text
status=ok seed=20261127 fingerprint=84896f5467ebe2614643fc4f1272e6fa5b4032924352664adbb27aaa62af8720
schemas=v1/v2/v3 frames=66 deviation=0 full=9 resumeReused=9 crashResumeReused=4
gainInvalidation=1 reusedAfterEdit=8 queueEvents>0 artifacts=artifacts/demo
```

## 目录

```text
errors.go            稳定错误码与错误定位字段
model.go             统一内部模型、深拷贝、哈希
generator.go         固定种子样本与独立逐帧真值
schema.go            v1/v2/v3 解析、默认补齐、字段路径诊断
export.go            显式降级、E_LOSSY、语义指纹、往返比较
units.go             整型时间/刻度/线性幅度与 dB 换算
frame.go             音素边界帧量化与逐帧能量/ZCR/瞬态峰值
scheduler.go         受限并行工作池、采样缓冲与队列预算
loudness.go          分段响度测量、增益求解与 E_LOUD
deps.go              缓存键、依赖图、精确失效计划与编辑入口
state.go             片段/帧区间双层状态机
engine.go            full/realtime/resume 统一执行器与 E_DIFF
store.go             database/sql 检查点事务与恢复读取
localsql.go          仓库内本地文件 SQL 驱动（原子替换提交）
compare.go           真值逐帧比对
cmd/dialogue-kernel  本地可重复演示命令
```

## 内部口径

- 时间统一为采样点；v1 的毫秒仅在装载/导出边界转换，半值远离零取舍。
- 增益、目标响度、容差统一为 `1/10000 dB`；v1 千分比乘 10 进入内部，
  v2/v3 万分比恒等映射。
- PCM 固定为单声道 `pcm_s16le`。生成器使用自有 xorshift 序列，不依赖运行时随机源。
- 核心处理链路只使用整数；没有浮点累加。每次有余数的换算都记录
  `ConversionTrace`，包含来源路径、单位、操作数、结果、余数、取舍方向和截断点。
- 版本字段命名/结构差异记录在 `FieldMapping`；低版本默认值记录在
  `DefaultTrace`，两者都保留源 schema 版本。
- 线性幅度换算使用 Q16 线性值与 `1/10000 dB`，以整数 log2/exp2 实现；规范值
  与常见 Q16 值可零残差往返，其余值显式返回截断残差。溢出统一返回 `E_OVF`，
  并带字段和两个操作数。

三个 schema 的差异被集中映射到 `Project`：

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 命名 | snake_case | camelCase | 嵌套 camelCase |
| 时间 | 毫秒 | 采样点 | 采样点 |
| 增益/响度 | 千分比 | 万分比 | 万分比 |
| 片段组织 | 全局扁平片段表 | 对白内嵌套 | 场次分组内嵌套 |
| 校准标记 | 无 | 无 | 支持 |

低版本升级的每次默认补齐都会写入 `DefaultTrace`；降级时无法直接表达的场次组织
和设计标记写入 `ExportResult.Lossy`，但片段/对白身份会被等价展平。语义指纹覆盖
可听内容、边界、权重、增益和校准参数，不包含仅表现层的分组与标记，因此生成样本
在 v3→v1→v3 后逐字段残差为 0。

## 处理管线

1. `BuildIntervals` 将片段边界按半值远离零量化为帧区间；`QuantizePhonemeBoundaries`
   为每个音素给出相同的帧号口径。
2. `ExtractFrames` 把帧任务按每线程采样缓冲上限切块，最多使用声明的并行度。
   待处理 channel 容量等于队列预算；满队列时生产者阻塞，并记录
   `queue_budget_exhausted` 原因和等待微秒数，不丢弃帧。
3. 帧结果先写回输入索引，再按对白 ID/帧号排序，因此线程数、完成顺序和缓存状态
   不影响能量、过零率和瞬态峰值位置。
4. `CalibrateFeatures` 用逐帧能量、响度权重、会话增益和片段增益求加权响度；
   目标与增益上界比较后才接受结果。不可达时返回 `E_LOUD`，包含目标值、可达上界
   和首个受阻片段/帧。
5. 检查点按稳定逻辑顺序提交。`full` 重算全部，`realtime` 同样从帧函数重算，
   `resume` 仅采用源版本、换算版本、算法版本、内容哈希和两类缓存键都匹配的区间。

缓存依赖拆成两类键：

```text
featureKey     = interval contentHash + interval ID + 帧率/采样率
                 + quantizationVersion + parallelVersion + 并行预算值 + upstreamKey
calibrationKey = featureKey + 片段增益 + 会话增益 + 目标响度
                 + 增益上界 + 容差 + conversionVersion
```

因此修改片段增益或目标响度时，原始逐帧特征可复用，只重算对应校准；切换帧率会
改变区间身份和均匀性，所有帧区间都重新提取。`BuildDependencyGraph` 显式输出
音频、片段、帧特征、校准和下游标记节点及依赖边。

## 集成入口

```go
generated, _ := dialoguecal.Generate(dialoguecal.DefaultGenerateOptions(42))
project, _ := dialoguecal.LoadProject(v3JSON) // 或使用 generated.Project
ctx := context.Background()

store, _ := dialoguecal.OpenStore("checkpoints.db")
defer store.Close()
engine := dialoguecal.NewEngine(store)

full, _ := engine.Run(ctx, project, dialoguecal.RunOptions{Mode: dialoguecal.ModeFull})
resumed, _ := engine.Run(ctx, project, dialoguecal.RunOptions{Mode: dialoguecal.ModeResume})
_ = dialoguecal.CompareRunResults(full, resumed)

edited, _ := dialoguecal.ApplyEdit(project, dialoguecal.ProjectEdit{
    Kind: dialoguecal.EditSegmentGain, SegmentID: "seg-01-01", IntValue: 2500,
})
incremental, _ := engine.Run(ctx, edited, dialoguecal.RunOptions{
    Mode: dialoguecal.ModeResume, PreviousProject: project,
})
// incremental.Invalidation 仅含实际受影响区间及其下游校准标记。
```

## 状态与持久化

片段状态：

```text
pending_load -> extracting -> calibrating -> committed
       \             \             \
        +-------------+-------------+-> failed -> pending_load
```

帧区间状态：

```text
pending -> processing -> committed
   \           \
    +-----------+-> failed -> pending
```

非法迁移返回 `E_STATE`，不修改状态、进度或序号。每个区间提交时，在单一
`database/sql` 事务中写入逐帧特征摘要、校准摘要、采样消费偏移、内容哈希、
源版本、换算版本、算法版本、缓存键和完整 payload；提交使用临时文件写入、
同步和原子 rename，并由存储层互斥锁串行化，序号严格递增。

仓库内没有引入第三方 SQLite 驱动。`localsql.go` 注册名为
`dialoguecal-sqlite` 的本地文件驱动，实现本组件所需的最小 SQL 子集
（建表、插入、等值删除/查询）和 `driver.Tx` 原子提交；它承载 SQLite 风格的
事务检查点语义，同时保持零外部依赖。这样满足“标准库 `database/sql` + 仓库内
本地文件持久化”的约束；若后续允许第三方驱动，可替换 `OpenStore` 的驱动名而
不改变 `Store` API。

## 异常与恢复

稳定错误码：

| 错误码 | 场景 |
|---|---|
| `E_SCHEMA` | 未知 schema、必填字段缺失，带 JSON 字段路径 |
| `E_LOSSY` | 低版本无法直接表达的组织/标记清单 |
| `E_OVF` | 整型换算溢出，带字段和操作数 |
| `E_LOUD` | 目标响度超过可达上界 |
| `E_STATE` | 非法片段/帧区间状态迁移 |
| `E_DIFF` | 两条运行结果首个逐帧不一致 |
| `E_DECODE` | PCM/JSON/样本计数损坏 |
| `E_ORDER` | 音素时序倒置、重叠或非严格递增 |
| `E_CAL` | 刻度、能量上限或帧率约束矛盾 |
| `E_STAGE` | 单区间内部处理失败 |
| `E_CANCEL` | 主动取消或模拟强制终止的提交边界 |

`RunOptions.StopAfterCommits` 可在指定个区间提交后停止，模拟强制终止；
重启时只读取此前已原子提交、且版本匹配的区间。主动取消不会提交未完整处理的区间。
`CompareRunResults` 对 full/realtime/resume 的逐帧特征、校准响度、失效清单和
逻辑提交序列做自动比对，并在 `E_DIFF` 中给出首个片段和帧号。

## 已验证结果

本仓库实际执行结果：

```text
go test ./...        ok dialoguecal
go test -cover ./... 核心包 77.9% statements
go test -race ./...  ok dialoguecal
go vet ./...         无输出
go run ./cmd/...     status=ok，三 schema 指纹一致
```

默认种子固定为 3 条对白、9 个片段、66 帧：

- 三版本装载后的语义指纹均为
  `84896f5467ebe2614643fc4f1272e6fa5b4032924352664adbb27aaa62af8720`。
- 逐帧特征与真值完全一致，校准响度最大偏差为 `0`。
- full/realtime/resume 三路径 `CompareRunResults` 均无差异。
- 模拟第 4 个区间提交后终止，恢复时复用 4 个已提交区间并补齐其余区间。
- 修改 `seg-01-01` 增益只失效 `0-7` 帧区间，其余 8 个区间复用持久化结果。
- 受限于队列预算记录了真实阻塞排队事件（次数随线程调度时机变化），未丢帧。

测试覆盖还包括：未知版本/缺字段路径、v1 往返零残差、`E_OVF`、非法状态保持、
目标不可达、音素倒置、刻度矛盾、单区间失败、版本不匹配拒绝恢复，以及
线程数变化下的逐帧确定性。

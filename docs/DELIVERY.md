# 无障碍呈现内核交付说明

## 1. 运行与验证

```bash
go version                       # 实测 go1.22.12 linux/amd64
go test ./... -count=1
go vet ./...
CGO_ENABLED=0 go build ./cmd/presentkernel

go run ./cmd/presentkernel generate --out samples
go run ./cmd/presentkernel verify \
  --db artifacts/final-checkpoint.sqlite \
  --report artifacts/final-state.json

go run ./cmd/presentkernel serve --port 38080 \
  --db artifacts/diagnostic.sqlite
```

不传 `--port` 时默认地址为 `127.0.0.1:8080`；`--port 0` 可选择临时空闲端口，也可用 `--addr` 配置。页面提供：

- 三版本装载与真值比对；
- 每个分区的字符区间、逐帧字符、逐帧口型和过渡帧；
- 过渡插入位置、类别和确定性依据；
- 编辑步骤、失效/复用分区高亮；
- 实时、断点恢复、全量重算三路径结果；
- SQLite 检查点行和内容哈希/版本；
- `E_ORDER`、`E_RATE`、`E_LIP`、`E_STAGE`、`E_READ` 夹具；
- 队列/线程缓冲阻塞与等待毫秒数。

完整实测控制台输出保存在 `artifacts/validation-output.txt`，结构化验收状态保存在 `artifacts/final-state.json`。

## 2. 代码结构

| 路径 | 职责 |
| --- | --- |
| `internal/domain` | 统一工程、分区结果、结构化错误码、标准口型类别族 |
| `internal/quant` | 十进制秒、纳秒、采样点、帧、千分比/万分比的溢出检查整型换算 |
| `internal/schema` | v1/v2/v3 DTO、版本探测、字段校验、加载痕迹、升降级导出和 `E_LOSSY` 清单 |
| `internal/generator` | 固定种子 `domain.Seed` 的样本工程、三版本 JSON 和逐帧真值生成 |
| `internal/engine` | 字符区间排程、最短可读压缩、口型映射、过渡插入、内容/缓存哈希、依赖图与编辑失效 |
| `internal/scheduler` | 最大并行度、每 worker 缓冲、队列预算、阻塞等待记录、panic/错误隔离 |
| `internal/checkpoint` | 纯 Go SQLite、内容寻址缓存、分区原子事务、有序运行检查点、快照失效 |
| `internal/runtime` | 实时并行、一次性全量、SQLite 断点续跑、编辑序列验证和首个 `E_DIFF` 定位 |
| `internal/diagnostic` | `net/http` API 与无外部 CDN 的本地诊断页 |
| `cmd/presentkernel` | `generate`、`verify`、`serve`、`version` 命令 |

## 3. 三版本 Schema 差异

同一固定种子工程导出为三个版本：

- `1.0.0`：snake_case 字段；秒用精确十进制字符串；字符权重和音量为千分比；口型表是扁平数字 `viseme_id`；没有过渡字段。
- `2.0.0`：camelCase 字段；时间为整数毫秒；权重为万分比/BP；口型表按 family 分组并通过 `shapeId -> standardCode` 映射；过渡容差字段可选。
- `3.0.0`：嵌套 `presentationProject/timingProfile/lipMapping`；同时给毫秒和采样点；权重/音量显式万分比；口型表为 category 加 `phonemeToCategory`；可携带显式过渡覆盖项。

加载结果保留 `SchemaVersion`，`ConversionTrace.Fields` 记录源字段、目标字段、换算器、旧值/新值、余数和截断；缺失默认值记录字段路径、`OldValue=nil` 和默认来源。未知版本或必填字段错误均为 `E_SCHEMA`，例如 `project_id`、`dialogue_cues[0].lines[0].text_content`。

降级到 v1/v2 时会产生 `E_LOSSY` 清单：

- v1 不支持过渡容差/显式过渡，处置为“省略并在加载时补声明默认值，由同一确定性边界规则重建”；
- v1/v2 的口型映射组织被扁平化/分组化，处置为“保留 standard code，加载时恢复标准 category/family”；
- v2 不支持 v3 显式 transition records，处置为“省略标准覆盖项，由确定性规则重建相同过渡帧”。

生成样本中的显式 v3 过渡与自动规则选择同一 bridge、规则和依据，因此低版本导出物重新加载并重建后，逐帧摘要与真值相同。

## 4. 量化与确定性

全链路不累加 `float64`：

- 秒字符串先解析为整数纳秒；
- 毫秒、纳秒、采样点通过整数乘除和声明的 `floor` / `ceil` / `round-half-up` 模式转帧；
- 每次非整除换算保留分子、分母、余数、取舍模式和结果；
- 权重在 `1/1000` 与 `1/10000` 间整数缩放；生成器产出的权重对齐粗刻度，因此 v3 → v1 → v3 不变；
- 乘法溢出返回 `E_OVF`，包含字段路径和两个操作数。

样本使用 48 kHz、24 fps，标准边界为整数采样点；切换到 30 fps 时从采样点重新量化，而不是在旧帧号上做浮点累加。`Project.SourceFPS` 保存源帧基准，仅用于校验源 sample/frame 一致性；当前呈现帧率可为编辑后的 `TargetFPS`。

## 5. 字幕与口型规则

字符排程：

1. 取音素首边界和尾边界形成实际朗读窗口；
2. 至少给每个 rune 一帧；
3. 用权重和最大余数法分配帧；
4. 若低于最短可读帧数，按空格/标点、低权重等确定性优先级压缩停留帧；
5. 记录压缩时的帧号、字符、优先级、压缩前后长度和规则；
6. 帧数少于字符数或所有字符已到 1 帧下限仍不可行时返回 `E_READ`，冲突列表逐字符列出区间。

最终 `CharIntervals` 满足 `EndFrame[i] == StartFrame[i+1]`、首区间从 0 开始、末区间到 `FrameCount` 结束，完整覆盖 rune 文本。

口型：

- 音素采样区间按当前帧率量化到帧；
- 每帧映射标准 category/family；
- 当相邻帧 family 既不相同也不在连续族表中时，在边界前插入 `TR` bridge；
- transition 记录 boundary frame、插入后的 sequence index、左右 category、bridge、规则和依据；
- `TransitionToleranceFrames < 1` 或显式 offset 超差时返回 `E_LIP`，错误含 dialogue、partition/frame、左右 category；
- category 不在编码表也返回 `E_LIP` 并定位对白/分区/音素帧。

## 6. 依赖图、缓存与编辑失效

每个 segment 是一个固定 `PartitionID`（如 `D0-P0`），图中显式建立：

- `segment_duration`
- `char_weights`
- `phoneme_boundaries`
- `fps:<n>` 与 `quant-rule`
- 下游 `char-frames`、`lip-frames`、`transitions`
- 最终 `partition-result`

缓存键为：

```text
CacheKey = SHA256(ContentHash + QuantParamVersion + UpstreamKey + AlgorithmVersion)
UpstreamKey = SHA256(ContentHash + FPSKey)
```

实测编辑序列：

1. 初始工程：6 个分区一致；
2. 修改 `D0-P0` 字符权重：仅 `D0-P0` 失效，5 个复用；
3. 修改 `D1-P1` 片段时长：仅 `D1-P1` 失效，5 个复用；
4. 24 fps 切到 30 fps：帧率量化上游变化，6 个分区全部失效。

## 7. 三路径与 SQLite 检查点

三条路径为：

- **realtime**：受限 worker pool，带内存内容缓存；
- **resume**：SQLite 有序检查点，可在提交 2 个分区后模拟中断，重开库继续；
- **full**：绕过缓存的一次性全量并行重算。

`runtime.VerifyEditSequence` 对初始状态和每次编辑后的状态比较：

- partition 提交序列；
- 字符区间和逐帧字符 range；
- 逐帧口型 category/family/base/sequence/inserted；
- transition 位置和类别；
- compression 记录。

首个差异通过 `E_DIFF` 返回 dialogue、partition、frame 和 field。实测四个步骤全部一致。

SQLite 有两类数据：

- `partition_cache`：以 `cache_key` 为主键的内容寻址结果 JSON，编辑后未受影响键仍可复用；
- `run_checkpoint`：`(run_id, sequence_no)` 有序分区提交记录，含逐帧摘要数组 JSON、总摘要、content hash、source schema、quant version、algorithm version。

`Store` 设为单连接，每个分区的 cache upsert 和 checkpoint insert 在同一事务提交，因此不会交错。重启时若有序号缺口、未知分区、cache key/hash 或版本不匹配，会删除该 run 的 checkpoint 快照；内容寻址 `partition_cache` 中键匹配的未受影响结果仍保留并可复用，随后从当前工程缺失/失效分区继续。

## 8. 预算调度与异常隔离

`scheduler.Config` 控制：

- `MaxParallel`：worker 数；
- `PerWorkerBuffer`：每个 worker channel 缓冲上限；
- `QueueBudget`：已接受但未完成分区的令牌预算。

队列或 worker 缓冲满时，`Submit` 阻塞并写入 `QueueEvent{PartitionID, Reason, WaitedMS}`，不会丢弃任务。测试和诊断页均包含可重复的阻塞等待样例。

异常注入链路：

| 注入 | 错误码 | 定位 |
| --- | --- | --- |
| 交换前两个音素 | `E_ORDER` | dialogue/partition/frame/symbol 两侧 |
| 破坏 sample/frame 量化一致性 | `E_RATE` | dialogue/partition/field/operands |
| 设置未知口型 category | `E_LIP` | dialogue/partition/frame/category |
| tolerance=0 遇到非连续族 | `E_LIP` | dialogue/partition/frame/left/right |
| 注入 stage failure/panic | `E_STAGE` | dialogue/partition；worker recover |
| 把时长压缩到少于字符数 | `E_READ` | dialogue/partition/conflicts |

并行任务的错误放在对应 `Outcome.Error` / `Snapshot.Errors`，不会取消或污染其他分区；成功分区仍可提交。

## 9. 确定性样本

生成器固定种子为 `domain.Seed = 0x5EED42`，包含 3 个对白、6 个分区。样本和真值不是外部下载内容：

```text
samples/subtitle.project.v1.json
samples/subtitle.project.v2.json
samples/subtitle.project.v3.json
samples/truth.json
samples/lossy-1.0.0.json
samples/lossy-2.0.0.json
```

`truth.json` 直接记录每个分区最终的 `char_intervals`、`char_frames`、`lip_frames`、`transitions`、`compressions` 和摘要，是本地精度比对基准。

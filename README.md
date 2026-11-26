# 逐帧音效响度包络重建内核

这是一个只依赖 Go 标准库的领域组件，用于替换旧版音效响度脚本。它包含：

- 固定种子、无录音素材的 PCM 工程与逐帧真值生成器；
- `game-audio/v1/v2/v3` 三套 schema 的统一加载、升级、降级与损失报告；
- 时间、增益、响度、线性幅度/分贝的整型定点换算；
- 逐帧能量/峰值测量、分段响度包络与逐片段增益校准；
- 五态片段状态机、精确片段/帧区间依赖失效；
- 原子片段检查点、受限并行调度，以及全量/增量/断点恢复三条一致重建路径；
- 本地演示与自动逐帧比较入口。

当前验证环境：`go version go1.22.12 linux/amd64`。`go.mod` 仅声明模块，没有第三方依赖。

## 快速验证

```bash
go build ./...
go test ./...
go test -race ./...
go run ./cmd/loudnessdemo
```

演示默认写入 `artifacts/demo/`，重复运行会重建该目录。固定种子为 `20261127`。

实测结果：

- 26 个测试全部通过，`go test -race ./...` 通过；
- 三版本各 7 个片段、174 个逐帧真值样本；
- v1 -> v3 升级补入 14 条默认字段记录；
- v3 -> v1 降级返回 `E_LOSSY`，13 条损失记录，导出文件可被 v1 加载器读回并保留 7 个片段；
- 实时增量、全量、断点恢复三条路径逐帧比较无差异；
- 调度器实际观测到并行度 3，并记录采样缓冲与队列预算等待；
- 固定种子演示重复执行后，三份 truth JSON 与基线检查点 SHA-256 均保持不变。

## 目录

| 路径 | 内容 |
|---|---|
| `model.go` | 统一内部模型、内部刻度、合法性校验、深拷贝 |
| `fixed.go` | 128 位中间量 `MulDiv`、取舍方向、时间/刻度/DB 换算 |
| `schema.go` | v1/v2/v3 wire 结构、统一加载、导出、升级与 `E_LOSSY` |
| `generator.go` | 固定种子工程、确定性 PCM 描述、真值与 PCM manifest |
| `measure.go` | PCM 合成、逐帧 Q32 能量、Q16 RMS、峰值、分段包络 |
| `calibrate.go` | 会话增益、逐片段增益求解、上界截断、目标校验 |
| `state.go` | 五态状态机、合法迁移、提交冻结、单片段失效 |
| `dependency.go` | 源/逐帧包络/校准节点、缓存键、精确失效传播 |
| `scheduler.go` | 最大并行度、队列预算、采样缓冲预算、等待事件 |
| `checkpoint.go` | 片段级原子检查点、摘要校验、恢复读取 |
| `engine.go` | 全量/增量/恢复三路径集成、缓存命中、串行提交 |
| `compare.go` | 三路径和真值的首个不一致定位 |
| `cmd/loudnessdemo/main.go` | 本地可重复演示与诊断输出 |

## 三版本 Schema

三份样本表达同一组片段核心内容，但 wire 表达不同：

| 项目 | v1 | v2 | v3 |
|---|---|---|---|
| schema ID | `game-audio/v1` | `game-audio/v2` | `game-audio/v3` |
| 片段表 | 扁平 `clips` | `scenes[].clips[]` 场景树 | 扁平 `segments`，每段有可选 `scene` |
| 时间基准 | 采样点 | 毫秒 | 毫秒 |
| 帧长/帧移 | 采样点 | 毫秒 | 毫秒 |
| 片段增益 | 线性 Q12 | 基点，`10000 == 1.0` | Q8 dB |
| 会话/上界增益 | Q12 | 基点 | Q8 dB |
| 目标响度 | Q8 dB | 百分之一 dB | 千分之一 dB |
| 响度权重 | Q8 | 基点 | Q8 |
| 校准标记 | 无 | 可选字符串 | 可选 `calibration.marker` |
| 字段命名 | 下划线、短名 | 下划线、独立场景名 | 小驼峰、`emitterClass`/`pitchTrackHz` |

加载后统一为：

- 时间：内部采样点；
- 增益：线性 Q12，`4096 == 1.0`；
- 响度：Q8 dB，`256 == 1 dB`；
- 权重：Q8，`256 == 1.0`；
- 线性幅度：Q16，`65536 == 全幅 1.0`。

每次加载都保留：

- `Provenance.OriginalVersion`：源版本；
- `Provenance.CurrentVersion`：当前内部版本；
- `Provenance.Traces`：源字段路径、内部路径、旧值、新值、默认来源、余数与残差。

未知版本或缺失必填字段返回 `E_SCHEMA`，并带 `Path`、`SourceVersion`。嵌套缺失同样定位到具体路径，例如：

```text
E_SCHEMA path=$.scenes[0].clips[0].start_ms source=game-audio/v2
```

## 整型定点规则

`MulDiv(a, b, c, rounding)` 使用 128 位中间量，不先做有损缩放：

- `RoundDown`：朝零截断；
- `RoundHalfUp`：按绝对值四舍五入；
- 返回 `Truncation`，包含路径、操作、分子、分母、商、余数、残差与取舍方向；
- 商或乘积超界返回 `E_OVF`，携带 `Field` 与两个操作数。

分贝换算：

```text
dB = (log2(linearQ16 / 65536)) * 6.020599913...
linearQ16 = 65536 * 2^(dB / 6.020599913...)
```

实现使用整数 `log2` 逐位平方和 16 个整数 `2^(1/2^k)` 因子，不做浮点累积。Q16 线性幅度/Q8 dB 的声明往返容差为 `DBRoundTripToleranceQ8 = 12`，即约 `0.047 dB`。

## 确定性样本与真值

`GenerateCase(seed, version)` 使用本地 SplitMix64 风格 PRNG，不读取录音或网络资源。样本包含：

- 4 个场景、7 个单声道片段；
- 采样率 `8000 Hz`，帧长 `256`，帧移 `128`；
- 每片段起点、终点、类别、4 点基频轨迹、幅度、响度权重、源增益、可选标记；
- 以整数相位累加器和三角波生成 PCM，并由基频轨迹决定相位增量；
- `TruthDocument` 保存逐帧 Q32 能量、Q16 RMS、峰值、测量 dB、校准后能量/dB；
- `PCMManifest` 保存 PCM 样本数与 SHA-256 摘要。

生成器先构造 canonical 工程，再导出到目标 schema 并重新加载，随后生成该版本真值。因此 v2/v3 的毫秒、基点和分贝量化也会进入真值与转换痕迹。

## 逐帧测量与校准

每个片段按 `start, start+shift, ...` 生成逻辑帧，最后一帧按片段边界裁剪：

1. 对窗口样本求 `sum(sample^2)`；
2. 得到 Q30 均方能量，整数开方得到 Q15 RMS；
3. 转为 Q16，再依次应用类别、响度权重、片段源增益；
4. 帧能量为 `rmsQ16^2`（Q32）；
5. 记录均值、开方、类别、权重、增益与 dB 转换的截断轨迹；
6. 分段能量由各帧能量按实际样本数加权；
7. 整段开方得到 Q16 分段 RMS。

类别因子固定为：

| 类别 | Q12 |
|---|---:|
| dialog | 4096 |
| sfx | 3584 |
| music | 3072 |
| ambient | 2048 |

校准按片段独立求解：

```text
currentRms = segmentRms * sessionGain
currentDB  = linearToDB(currentRms)
deltaDB    = target - currentDB
desiredGain = dBToGain(deltaDB)
finalGain  = min(desiredGain, maxGain)
```

然后对每一帧重新应用会话增益和 `finalGain`，输出校准后 Q32 能量、Q16 RMS 与 Q8 dB。若目标高于所有片段增益上界可达到的最小上界，返回 `E_LOUD`，带目标值、可达上界和受限片段编号。

## 依赖图与失效

依赖节点：

- `clip-source`：内容哈希、片段区间、权重、源增益、增益/响度刻度；
- `frame-envelope`：片段内容哈希、帧区间 ID、帧长/帧移、量化版本、上游源节点缓存键；
- `gain-calibration`：所有逐帧包络缓存键、目标、会话增益、增益上界、容差和量化版本。

缓存键统一为：

```text
SHA256(
  contentHash |
  frameOrCalibrationIntervalID |
  quantizationVersion |
  sorted(upstreamCacheKeys...)
)
```

编辑失效规则：

| 编辑 | 失效范围 |
|---|---|
| 片段增益 | 仅该片段的逐帧包络 + 校准 |
| 目标响度 | 所有片段的校准；逐帧测量缓存仍有效 |
| 帧长/帧移 | 所有片段的逐帧包络 + 校准 |

设置成相同值视为 no-op，不产生失效。失效清单会合并相邻帧区间，输出 `clipId`、采样起止和阶段。

## 状态机与提交

五态：

```text
pending-load -> measuring-envelope -> calibrating-gain -> committed
                                  \-> failed
committed/failed --显式失效/重试--> pending-load
```

- 其他迁移返回 `E_STATE`，状态与进度保持不变；
- 进度为 `已提交片段数 / 总片段数`；
- 提交时深拷贝并冻结逐帧包络与增益调整量；
- 失效只影响目标片段，已冻结的其他片段不变。

## 检查点与并行调度

每个片段提交时写入：

- 包络摘要、全部逐帧包络；
- 增益调整量、期望增益、上界标记；
- 内容哈希、PCM 哈希、测量/校准缓存键；
- 源版本、当前版本、量化版本、算法版本、帧参数、目标参数。

写入流程在单个 `CheckpointStore` 互斥区内执行：

```text
CreateTemp -> Write -> Sync -> Close -> Rename -> fsync(directory)
```

因此并发工作线程不会交错写同一片段或留下半文件。恢复时重新校验摘要、内容哈希、帧参数、量化/算法版本和缓存键，只复用有效检查点。

调度器限制：

- 最大工作线程数；
- 待处理队列容量；
- 队列样本预算；
- 全局采样缓冲预算；
- 每线程任务采样成本上限。

预算耗尽时任务阻塞等待，不丢弃；`WaitEvent` 记录任务、原因（`queue-sample-budget`、`queue-capacity`、`sampling-buffer-budget`）和等待微秒数。单任务超过每线程上限返回 `E_PARA`。

## 三条重建路径

- `RunFull`：忽略旧检查点，一次全量重建；
- `RunIncremental`：按缓存键复用未失效的测量/校准；
- `RunResume`：从本地检查点恢复，缺失或失效片段重新计算。

`CompareBuildResults` 比较：

- 提交序列；
- 失效清单；
- 每片段增益调整量、分段校准响度；
- 每帧区间、Q32 能量、Q16 RMS、测量/校准 dB、峰值、缓存键。

返回首个不一致的 `ClipID`、`FrameIndex` 与字段名。三路径测试会对同一编辑场景分别执行三种模式，并要求比较结果为 `nil`。

## 最小集成示例

```go
caseV3, err := loudness.GenerateCase(20261127, loudness.SchemaV3)
if err != nil {
    return err
}
project, err := loudness.Load(caseV3.Wire)
if err != nil {
    return err
}

engine, err := loudness.NewEngine(project, loudness.EngineOptions{
    CheckpointDir: "artifacts/checkpoints",
    Scheduler: loudness.SchedulerConfig{
        MaxWorkers:          3,
        QueueCapacity:       1,
        QueueBudgetSamples:  8000,
        BufferBudgetSamples: 6000,
        MaxSamplesPerWorker: 6000,
    },
    Rounding: loudness.RoundHalfUp,
})
if err != nil {
    return err
}

_, err = engine.ApplyChanges([]loudness.Change{
    {Kind: loudness.ChangeGain, ClipID: 2, GainQ12: 6144},
    {Kind: loudness.ChangeTarget, TargetLoudnessDBQ8: -7936},
})
if err != nil {
    return err
}

result, err := engine.Run(context.Background(), loudness.RunIncremental)
if err != nil {
    return err
}
```

## 错误码

| 错误码 | 场景 | 关键诊断 |
|---|---|---|
| `E_SCHEMA` | 未知版本、必填字段缺失、结构非法 | `Path`, `SourceVersion` |
| `E_LOSSY` | 导出到低版本时丢失场景树/标记 | `Losses[]`，同时仍有可回读 payload |
| `E_OVF` | 整数换算或累加溢出 | `Field`, `Operands` |
| `E_PARA` | 帧长/帧移/预算参数越界 | `Field`, `LegalMin`, `LegalMax` |
| `E_LOUD` | 目标响度超出可达上界 | `Target`, `Reachable`, `LimitedClips` |
| `E_STATE` | 非法状态迁移 | `FromState`, `ToState`, `CommitCount`, `ClipTotal` |

## 演示产物

`go run ./cmd/loudnessdemo` 生成：

```text
artifacts/demo/project_v1.json
artifacts/demo/project_v2.json
artifacts/demo/project_v3.json
artifacts/demo/truth_v1.json
artifacts/demo/truth_v2.json
artifacts/demo/truth_v3.json
artifacts/demo/pcm_manifest_v1.json
artifacts/demo/pcm_manifest_v2.json
artifacts/demo/pcm_manifest_v3.json
artifacts/demo/downgraded_v3_to_v1_lossy.json
artifacts/demo/downgrade_losses.json
artifacts/demo/checkpoints_baseline/
artifacts/demo/checkpoints_full/
artifacts/demo/checkpoints_resume/
```

演示同时打印：

- 每版本加载/真值对账结果；
- 升级默认值、降级损失；
- 全部错误码样例；
- 三路径逐帧一致性、失效清单；
- 依赖节点计数；
- 逐帧包络与增益决策；
- 原子检查点摘要。

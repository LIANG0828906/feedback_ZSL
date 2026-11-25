# 音素边界到逐帧口型包调度链路：技术说明

本仓库在 Go 1.22 下交付一个可集成的根领域包 `lipsync`、CLI `cmd/lipsync` 和本地诊断站 `internal/web`。SQLite 使用纯 Go 驱动 `modernc.org/sqlite v1.29.10`，不需要 `libsqlite3-dev` 或本地 C 库。

## 快速运行

```bash
go version
go test ./... -count=1

go run ./cmd/lipsync generate \
  --project data/project.json \
  --truth data/truth.json

go run ./cmd/lipsync verify \
  --workdir . \
  --report artifacts/acceptance-report.json

go run ./cmd/lipsync serve \
  --addr 127.0.0.1:8080 \
  --db data/lipsync.db

go run ./cmd/lipsync inject --kind order
go run ./cmd/lipsync inject --kind label
go run ./cmd/lipsync inject --kind vis
go run ./cmd/lipsync inject --kind stage
```

诊断页为原生 HTML/CSS/JavaScript，无 CDN、账号或外部数据依赖。页面包含对白/分片列表、口型包时间轴、逐帧预算柱图、依赖失效清单、SQLite 提交顺序和队列等待明细。

## 领域模型与确定性

- 固定种子生成 4 条对白/分片、43 个音素、单声道 `pcm_s16le` PCM 描述、音素区间、类别、权重、相邻亲和度和配方。
- PCM 样本由整数 LCG/三角波生成，`PCMDescription.SHA256` 是小端 PCM 字节哈希。
- 所有枚举、合并候选、调度队列和提交顺序均显式排序；不依赖 Go map 迭代顺序、goroutine 完成顺序或线程 ID。
- `Project`、`Dialogue`、`Pack`、`DispatchResult` 等结构均支持 `encoding/json`。

## 整型量化与权重

帧区间统一采用半开区间 `[StartFrame, EndFrame)`，可见帧数为 `EndFrame-StartFrame`。采样点到帧号使用整数有理式：

```text
scaled = sample * fps
frame  = scaled / sampleRate
remainder = scaled % sampleRate
```

取舍方向：

- `down`：整数地板除；
- `up`：`remainder != 0` 时加一；
- `nearest`：`2*remainder >= sampleRate` 时加一。

每处 start/end 截断都写入 `Truncation`，包含对白、分片、音素、边界名、分子、分母、余数和量化帧号。

权重规范刻度为万分比：

- 千分比 → 万分比：`permille * 10`；
- 万分比 → 千分比：`permyriad / 10`，余数为 `permyriad % 10`。

核心路径不使用浮点累加。

## 口型包归并

`PackDialogue` 的规则为：

1. 静音类别形成硬边界，任何包不跨越静音段；
2. 合法相邻边按 `affinity DESC, earlier_phoneme_start_frame ASC, boundary_index ASC` 排序；
3. 用并查集合并，合并后音素数不得超过 `Recipe.MaxPhonemesPerPack`；
4. 最小可见帧不足时，只允许在不跨静音且不超过最大音素数的相邻包之间做可见性修复合并；
5. 仍不满足 `MinVisibleFrames` 时返回带对白、分片和包字段路径的 `E_VIS`。

每个包的 `decision` 记录候选边界、是否选中、拒绝原因、亲和度并列组的起始帧和最终裁决说明。

## 逐帧下发语义

`DispatchPacks` 是实时路径、恢复路径和全量路径共用的纯函数：

- `FramePackBudget` 限制该逻辑帧新受理的下发动作数，输出字段为 `budgetUsed` / `budgetOccupied`；
- 包一旦在实际下发帧受理，就按原始时长 `EndFrame-StartFrame` 占用最低编号的空闲扬声器通道；
- `speakerChannels` 是同时活动通道上限；
- 通道状态同时输出当前活动包 `packIds` 和截至该帧已下发包的累积集合 `deliveredPackIds`；
- 无法受理时记录 `DeferralRecord`，下一帧按 `originalStartFrame, shardIndex, packIndex` 的稳定优先级重试；
- 延迟达到 `MaxDeferFrames` 后写入 `RejectionRecord`，拒绝包含 `channel_busy` 或 `budget_exhausted` 原因和解释，不静默丢弃；
- 每帧都校验 `budgetUsed <= FramePackBudget`、每通道最多一个活动包、活动包数不超过通道上限。

## 依赖图、缓存和精确失效

`BuildDependencyGraph` 显式建立：

```text
phoneme boundary/weight/affinity nodes
  -> mouth pack nodes
    -> dialogue frame attribution nodes
    -> global dispatch frame nodes
```

音素缓存键：

```text
SHA256("phoneme-v1", dialogueContentHash, phonemeID, recipeVersion, pcmUpstreamKey)
```

包缓存键额外包含有序音素 ID 和各音素上游键：

```text
SHA256("pack-v1", dialogueContentHash, packID, recipeVersion,
       phonemeIDs..., upstreamPhonemeKeys...)
```

因为键按要求包含会随任意编辑变化的对白内容哈希，未受影响包通过旧持久化键读取后，以新键原子复写/复用，并在统计中计入 `reused`。实际输出载荷未变化的包不会进入失效清单；变化的包才进入 `manifest.packs`。依赖图传播出的下游帧写入 `downstreamFrames`，真实逐帧 attribution/dispatch 差异分别写入 `dialogueFrames` 和 `dispatchFrames`。

`DiffResults` / `DiffDispatch` 比较包音素序列、起止帧、可见帧数、摘要和逐帧下发集合；首个差异返回 `E_DIFF`，并携带对白 ID、分片、帧号和字段路径。

## SQLite 检查点与恢复

`Store` 使用 `database/sql` + 纯 Go SQLite 驱动，并设置 `SetMaxOpenConns(1)`。主要表：

- `shard_commits`：版本化、追加式分片提交；
- `cache_entries`：持久化领域缓存；
- `runs`：`running` / `completed` / `cancelled` 运行日志。

每个分片在 `CommitShard` 的单个事务中写入：

- 包划分摘要 JSON；
- 该分片的逐帧记录视图；
- 完整逐帧 dispatch JSON；
- 完整 `DialogueResult` JSON；
- 对白内容哈希、配方版本、算法版本；
- 缓存条目。

应用层提交按 shard index 串行进行，`commit_seq` 单调递增。重启时 `InspectAndInvalidateCheckpoints` 读取每个分片最新版本；内容哈希、配方版本或算法版本不匹配时，在事务中整体删除该分片检查点和缓存，然后只续算 pending shard。

用户取消会把 run 标记为 `cancelled` 并可保存退出报告；进程被强制终止时来不及写终态，重启看到最后的 run 仍为 `running`，据此判定 `forcedTermination=true`，只信任已经提交的 SQLite 状态。

## 并发资源边界

`RunShardPool` 提供：

- 固定 worker 数：`PoolOptions.MaxParallelism`；
- 每 worker `StateBuffer.Capacity`：超出返回带定位的 `E_STAGE`；
- 全局有界队列：channel 容量等于 `QueueBudget`；
- 队列预算耗尽时生产者阻塞发送，并记录 `queue_budget_exhausted`、分片、原因、等待时长和预算占用。

worker 只负责计算；分片检查点在收集结果后按 shard 顺序串行提交，因此并行计算不会造成 SQLite 写入交错。

## 异常注入

| 注入 | 错误码 | 定位示例 |
| --- | --- | --- |
| 音素时序倒置/重叠 | `E_ORDER` | `dlg-000` shard 0，`phonemes[3].endSample` |
| 类别越界 | `E_LABEL` | `dlg-000` shard 0，`phonemes[2].class` |
| 最小可见帧不可满足 | `E_VIS` | `dlg-000` shard 0，`packs[0].visibleFrames` |
| 单分片内部失败 | `E_STAGE` | `dlg-003` shard 3，`shard.task` |

## 实际验证结果

验证环境：

```text
go version go1.22.12 linux/amd64
```

已执行并通过：

```bash
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
```

最近一次普通测试结果：

```text
?       lipsync/cmd/lipsync     [no test files]
ok      lipsync                 ...
ok      lipsync/internal/web    ...
```

竞态测试结果保存在 `artifacts/go-test-race.txt`，退出码为 0。

本地确定性验收（`artifacts/acceptance-report.json`）的最近结果：

```text
ok=true dialogues=4 phonemes=43 packs=21 frames=77
resultsEqual=true commitOrdersEqual=true finalCommitOrder=[0 1 2 3]
realtime/recovery/full commitOrder=[0 1 2 3]
defer=5 reject=0
forcedTermination=true
exceptions: E_ORDER/E_LABEL/E_VIS/E_STAGE all ok
```

同一组四次编辑的增量路径均与空缓存全量重算逐项一致；例如权重编辑精确失效 1 个包，边界编辑失效 1 个相邻包，亲和度编辑失效 2 个候选包，预算编辑不失效任何包但产生实际逐帧调度差异。完整命令输出见 `artifacts/validation-summary.txt`。

# 口型包调度原型架构

本仓库是一个可集成的 Go 领域组件与 `net/http` 本地诊断应用。固定种子生成器产出工程、三版本 schema 文档、音素标注、PCM 描述和解析真值；不依赖外部音频、账号或服务。

## 目录

- `internal/domain`：核心模型、参数、帧/包/检查点 DTO 与带字段路径、分片、帧号、操作数的错误类型。
- `internal/generate`：splitmix64 风格确定性生成器、三版本导出/回载样本和真值装配。
- `internal/schema`：`lip.project/v1|v2|v3` 加载、严格字段路径校验、显式升级补齐、降级 `E_LOSSY` 与转换影子。
- `internal/convert`：采样点/毫秒、千分比/万分比的纯整数换算、溢出检查和余数台账。
- `internal/pipeline`：确定性包归并、帧量化、类别族过渡帧、预览降采样、精确路径和 `E_DIVERGE` 比对。
- `internal/cachegraph`：复合缓存键、音素→包→帧→下发依赖图和编辑前后差异失效清单。
- `internal/checkpoint`：六态状态机与 `database/sql` + `github.com/mattn/go-sqlite3` 原子检查点。
- `internal/schedule`：最大并行度、每 worker 候选包缓冲、有界帧队列、通道槽位、阻塞等待留痕和异常隔离。
- `internal/service`：编排样本、缓存、编辑、三条下发路径、恢复和故障注入。
- `internal/web`：JSON API 与 `embed` 打包的单页诊断台。
- `cmd/lipctl`：本地服务入口。

## Schema 版本矩阵

| 版本 | 字段风格 | 时间基准 | 权重 | 类别编码 | 亲和度 |
| --- | --- | --- | --- | --- | --- |
| `lip.project/v1` | camelCase，如 `startMs`、`phonemeId` | 整数毫秒 | permille/千分比 | 平坦 `categoryCodes` | 无原生字段 |
| `lip.project/v2` | snake_case，如 `start_sample`、`phoneme_id` | 采样点 | permyriad/万分比 | `category_tables` 分组 | 无原生字段 |
| `lip.project/v3` | 嵌套 `timing`/`weight`/`transition` | 采样点 | permyriad/万分比 | `category_groups` 分组 | `transition.affinity_to_next` |

低版本加载时会写入 `domain.FillRecord`，包含字段路径、旧值（缺失为 `nil`）、默认值和原因。高版本导出低版本时，每个无法原生表达的亲和度或分组字段产生一条 `E_LOSSY`，并给出处置：

- strict native：删除字段，回载使用声明默认值；
- 默认导出：扁平化原生字段，同时写 `x_conversion_shadow` 保存精确亲和度/分组，供同版本加载器恢复语义等价模型。

## 整数一致性

全链路内部单位统一为采样点。换算不使用 float：

- `ms = floor(samples * 1000 / sample_rate)`；48 kHz 等 `sample_rate % 1000 == 0` 时，台账残差直接以剩余采样点表示。
- `samples = floor(ms * sample_rate / 1000)`。
- `weight10000 = weight1000 * 10`；降级 `floor(weight10000 / 10)` 并记录余数。
- 帧号为 `floor(sample * fps / sample_rate)`。
- 乘法使用 `math/bits.Mul64` 检查溢出；`E_OVF` 记录字段路径和两个操作数。

生成器选择 48 kHz、30 FPS、100 ms 整数倍边界（4800 采样点 = 3 帧）和 10 的倍数权重，因此固定样本在 v3→v1→v3 后边界、权重、包和帧完全一致；非对齐换算由单元测试覆盖余数与残差。

## 包、过渡与真值

归并规则是严格确定性的：

1. 静音音素立即闭包并单独成包，包不跨越静音边界；
2. 非静音相邻音素的前向亲和度低于 `affinity_merge_threshold_permyriad` 时切包；
3. 达到 `max_phonemes_per_pack` 时切包；
4. 保持音素顺序，包序号按分片内顺序生成。

口型族为 `X=0`（静音）、`C/T=1`（辅音/过渡）、`V=2`（元音）。当相邻稳定口型族跨度大于 1 且不超过 `transition_tolerance` 时，在新段首帧插入 `C` 桥接/过渡帧并记录 `TransitionMark`；跨度超过容差返回 `E_LIP`，带帧号、包序号和实测跨度。

`generate.BuildSample` 用同一确定性构造产生 `TruthByShard`。测试从 v1/v2/v3 回载后重新计算并逐包、逐帧、逐过渡位置与真值比较；并发重复构建结果序列化后必须完全相同。

## 预览与精确路径

`pipeline.BuildPreview` 使用 `PreviewFrameStride` 降低采样密度，并且只检查音素边界帧而不扫描全部帧。`PreviewStats` 记录：

- 源/预览 FPS、步长、采样密度；
- 总帧、实际评估帧、跳过帧；
- 过渡判定扫描帧数/预算；
- wall-clock 微秒、延迟预算、整数万分比预算占用；
- 判定依据字符串。

服务的 `/api/preview` 同时运行全精度 `BuildShard` 并用 `ComparePreviewExact` 比较采样帧类别、kind 和全部过渡插入位置。首个差异返回 `E_DIVERGE`，包含帧号、包序号、预览值和精确值。

## 缓存键与精确失效

缓存键形式为：

```text
ck_<sha256(dialogue_hash | entity_id | pack_index | param_version | upstream_hash)[:24]>
```

依赖图显式记录：

- `phoneme -> pack`：边界、权重、类别、亲和度；
- `pack -> frames`：FPS、量化规则、过渡容差；
- `param -> frames`：渲染参数；
- `frames -> dispatch`。

编辑后服务重算该分片，比较新旧包摘要、音素集合和逐包帧摘要。只有摘要变化或新增/删除/移位的包进入 `invalidated`；未变化节点进入 `reused`，并从 SQLite `cache_entries` 读取旧键、复制到新键或确认旧键命中。清单每项包含缓存键、旧键、来源 schema 版本、原因和字段。

## 状态机与 SQLite

六态为 `pending`、`quantizing`、`merging`、`delivering`、`committed`、`failed`。非法跳跃返回 `E_STATE`，状态对象保持原状态和进度。活动态中断后先经 `failed` 再回到 `pending`，表达“从未提交点重算”。

`Store.CommitShard` 在单事务中写：

- `shard_checkpoints`：状态、进度、内容哈希、参数版本、算法版本、来源版本、包摘要、过渡 JSON；
- `packs`：包 JSON 与缓存键；
- `frames`：逐帧 JSON；
- `dispatch_records`：逐帧下发记录；
- `commit_journal`：单调提交序号。

`sync.Mutex` 与 `SetMaxOpenConns(1)` 保证共享检查点写入串行。`pack_digest` 是整分片包边界与包摘要的聚合哈希。恢复时内容哈希、参数版本、算法版本或来源 schema 版本任一不符，就在事务中删除该分片物化结果并重置为 `pending`，随后只重算该分片。

## 并行预算与隔离

`schedule.Run` 启动固定 worker：

- `MaxParallelism` 限制 worker 数；
- 每个 worker 有容量为 `CandidateBufferPerWorker` 的候选包 channel；
- `FrameQueueBudget` 限制待处理 `DispatchRecord` channel 容量；
- `ChannelSlots` 为每个扬声器通道提供信号量；
- 候选缓冲或帧队列耗尽时阻塞发送，并记录 reason、candidate/worker/channel 和等待微秒数；
- 不丢弃任务或帧。

候选包错误或 panic 被转换为分片失败结果；调度器继续处理其他候选。服务只提交没有错误的分片，因此单分片 `E_STAGE` 不写检查点、不污染其他分片；恢复路径会补算失败分片。

## HTTP/页面

启动：

```bash
go run ./cmd/lipctl serve --addr 127.0.0.1:8080 --db artifacts/lip-pipeline.db
```

页面支持：

- 三版本工程和补齐/换算/损失台账；
- 对白、分片、音素、包、帧条和依赖图诊断；
- 拖动参数触发降采样预览，停止后全精度比对；
- 编辑音素字段或 FPS，查看失效/复用缓存键；
- 并行实时、断点恢复、全量重算和破坏哈希后恢复；
- 一键注入 `E_ORDER`、`E_DECODE`、`E_VAL`、`E_STAGE`，以及 `E_LIP`、`E_OVF`、`E_SCHEMA`、`E_STATE`、`E_DIVERGE`。

主要 API：

- `GET /api/overview`
- `GET /api/schema-reports`
- `GET /api/schema/{v1|v2|v3}`
- `GET /api/shards/{id}`
- `POST /api/preview`
- `POST /api/edit`
- `POST /api/run/realtime`
- `POST /api/run/resume`
- `POST /api/run/full`
- `POST /api/checkpoint/corrupt`
- `GET /api/faults/{kind}`

## 验证入口

```bash
gofmt -w cmd internal
go test ./...
go test -race ./internal/schedule ./internal/service ./internal/pipeline
go vet ./...
```

实际验证结果见 `docs/VERIFICATION.md`。

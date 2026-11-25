# 逐帧指纹标注组件交付说明

本仓库交付一个可集成的 Go 领域组件、确定性样本生成器、可运行 CLI 示例和本地 Web 标注台。未使用 Docker 或外部数据服务。

## 目录结构

- `errors.go`：`E_SCHEMA`、`E_RANGE`、`E_PARA`、`E_QUAL`、`E_STATE`、`E_DIFF` 结构化错误。
- `model.go`、`load.go`：PCM 描述/清单模型、字段存在性、范围、重叠、枚举和有限性校验。
- `samplegen/generator.go`：固定种子 `2026112501` 的单声道样本与真值清单生成器。
- `fingerprint.go`、`fft.go`、`params.go`：确定性 Hann/FFT、Mel 频带能量、量化指纹和参数版本。
- `template.go`、`tone.go`、`aggregate.go`：类别音调模板、逐帧相似度、间隙合并、区间与置信度。
- `quality.go`：匈牙利一对一匹配、类别正确率、采样点偏差、置信度分布和边界偏差专列。
- `state.go`：`pending -> extracting -> committed/failed` 四态机、非法迁移保护、提交快照冻结。
- `worker.go`：有界 worker pool、每线程采样缓冲上限、队列满时阻塞反压。
- `checkpoint.go`：纯 Go SQLite（`modernc.org/sqlite`）版本化检查点、串行原子提交和身份失配整体作废。
- `pipeline.go`、`compare.go`：全量、实时、断点恢复路径及首个帧/事件/质量差异定位。
- `server/server.go`、`server/web/index.html`：本地 HTTP API 与零外部 CDN 的交互页面。
- `cmd/gensample`：写入确定性样本。
- `cmd/server`：启动标注台。
- `examples/demo`：并发一致性、中断恢复、三路径比对和导出的命令行示例。
- `*_test.go`、`server/server_test.go`：领域、并发、状态、检查点、质量和 HTTP 工作流测试。

## 快速运行

```bash
go run ./cmd/gensample -out data
go run ./examples/demo -data data
go run ./cmd/server -addr 127.0.0.1:8080 -data data
```

浏览器打开 `http://127.0.0.1:8080`。页面支持调整帧长、帧移、频带数、间隙容忍、相似度阈值和并发上限，支持增量标注、逐窗口重试/模拟失败、断点恢复、三路径比对、JSON/CSV 导出。

## 已验证环境

- Go：`go1.22.12 linux/amd64`，满足 Go 1.22+。
- `GOPATH=/home/liang/.feedback/runtime-home/.../go`
- `GOPROXY=https://proxy.golang.org,direct`
- SQLite 驱动：`modernc.org/sqlite v1.29.10`，纯 Go/transpiled 实现。
- `go mod verify`：`all modules verified`。
- `CGO_ENABLED=0 go build ./...`：通过，确认不需要本地 C 工具链链接 SQLite。

## 实际测试结果

以下命令已于当前工作区实际执行：

```bash
go vet ./...
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test -count=1 ./...
go test -race -count=1 ./...
go run ./examples/demo -data data
```

结果摘要：

- `go vet ./...`：无问题。
- `CGO_ENABLED=0 go build ./...`：`BUILD_OK`。
- 普通测试：`ok audiofp`、`ok audiofp/server`，其余命令/生成器包无测试文件。
- `-race`：`ok audiofp`、`ok audiofp/server`，未发现数据竞争。
- HTTP 工作流测试覆盖 `/api/state`、`/api/compare`、重置部分提交、模拟失败、单窗重试、增量标注和非法参数 `E_PARA`。

## 确定性样本结果

默认参数为 `frame_length=1024`、`frame_shift=256`、`band_count=32`、`gap_tolerance_samples=512`、`match_threshold=0.82`；清单声明容差为 512 采样点。样本包含 49152 个采样点、189 个完整帧和 6 个事件。

四种并发度（1、2、4、8）以及不同队列长度产出的帧指纹通过 `reflect.DeepEqual` 逐帧逐字段比较。示例实际输出：

```text
concurrency=1 frames=189 events=6 quality=true
concurrency=2 frames=189 events=6 quality=true
concurrency=4 frames=189 events=6 quality=true
concurrency=8 frames=189 events=6 quality=true
concurrency check: identical frames/events/quality
realtime: frames=189 events=6 passed=true accuracy=1.000
simulated stop after window #40: durable_prefix=40 reset=false
resume: recovered=41 frames=189 events=6 passed=true reset=false
three-path check: consistent frames=189 events=6
quality: passed=true category_accuracy=1.000 mean_abs_start=106.7 mean_abs_end=21.3 boundary_deviations=3
```

逐事件偏差（实际示例输出）：

```text
#1 voice [2048,8192)  -> voice [1792,8192)  delta=(-256,0)
#2 ui    [9344,13440) -> ui    [9216,13312) delta=(-128,-128)
#3 music [15360,22528)-> music [15360,22528)delta=(0,0)
#4 sfx   [24576,30720)-> sfx   [24576,30720)delta=(0,0)
#5 voice [32768,38912)-> voice [32512,38912)delta=(-256,0)
#6 music [40960,48128)-> music [40960,48128)delta=(0,0)
```

最大绝对起点偏差 256、终点偏差 128，均在声明容差 512 内。3 个非零边界偏差进入 `quality.boundary_deviations` 专列。容差序列 `1024 -> 512 -> 256 -> 128 -> 0` 的通过事件数单调不增，测试 `TestTighteningToleranceIsMonotonic` 守护该性质。

## 状态机与检查点

合法迁移只有：

```text
pending    -> extracting
extracting -> committed | failed
failed     -> extracting
committed  终态，指纹和 Params 快照冻结
```

非法迁移返回 `E_STATE`，且状态保持不变；对应测试为 `TestWindowStateMachineLegalTransitions` 和 `TestCommittedParameterSnapshotFrozen`。

SQLite `frame_windows` 表逐窗保存：

- `fingerprint_digest` 与完整 `fingerprint_json`
- `param_version`
- `material_hash`
- `algorithm_version`
- 窗口序号、起点和提交时间

写入使用事务同时更新 `next_window_index`，连接池限制为 1，并在 store 内加互斥锁；乱序写窗口返回 `E_STATE`。重启时如果 `material_hash`、`param_version`、`algorithm_version` 任一不匹配，或摘要/序号损坏，会在同一事务中删除全部旧窗口并返回空快照供全量重算。测试包括：

- `TestSQLiteCheckpointResumeAndIdentityReset`
- `TestResumeAfterInterruptMatchesFull`
- `TestMismatchedCheckpointForcesFullRecompute`

另实测启动一个新的服务进程打开已有 `data/server-checkpoint.db`，`/api/state` 返回：

```text
path=resume recovered=189 reset=false frames=189 quality=True
```

## 三路径与 E_DIFF

`EnsureIdentical(full, realtime, resume)` 依次比较：

1. 帧数量与每帧全部字段，包括每个 `bands[i]` 和摘要；
2. 事件数量、类别、边界、帧号、相似度、置信度；
3. 质量计数、准确率、误差、置信度分布和逐事件对照。

首个差异以 `E_DIFF` 返回，`Error.FrameIndex/EventIndex/Field/Expected/Actual/Details.FirstDiff` 给出定位。测试 `TestEnsureIdenticalLocatesFirstFrameField` 与 `TestEnsureIdenticalLocatesEventField` 分别验证能定位到 `frames[3].bands[0]` 和 `events[1].category`。

## HTTP API

- `GET /api/state`：素材摘要、波形 min/max、清单、参数、窗口状态/指纹、当前结果、质量和比对报告。
- `POST /api/params`：校验并更新参数；已提交窗口的快照不会被改写。
- `POST /api/annotate`：`{"mode":"incremental"|"full"}`。
- `POST /api/windows/{index}/retry`：重试失败/待提取窗口。
- `POST /api/windows/{index}/simulate-failure`：按合法迁移构造失败窗口，便于演示重试。
- `POST /api/reset`：重置或构造前 N 个已提交窗口。
- `POST /api/resume`：走 SQLite 断点恢复路径。
- `POST /api/compare`：运行实时、恢复、全量三路径比对。
- `GET /api/export?format=json|csv`：导出标注。

## 并发资源边界

`ResourceLimits` 包含：

- `MaxWorkers`：最大并行 worker；
- `PerWorkerBufferSamples`：每个 worker 复用的采样缓冲容量；
- `MaxQueuedJobs`：有界等待队列。

帧长超过单线程缓冲上限会返回 `E_PARA field=per_worker_buffer_samples`。队列满时 `Submit` 阻塞或响应 context 超时，不丢弃任务；`TestWorkerPoolDoesNotDropJobsAndBlocksWhenSaturated` 验证 200 个任务全部处理且饱和提交会被反压阻塞。

# channelpipe：美术贴图通道转换领域组件与本地验收示例

纯 Go 1.22+ 实现，SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，不需要 CGO、Docker、外部素材或网络服务。样本生成器以固定种子生成本地图像、三版本计划和真值；命令行示例会完成加载、转换、检查点、中断续跑、增量失效、三路径比对和独立验证。

## 快速运行（Windows PowerShell）

```powershell
go version
go env GOPATH GOMODCACHE CGO_ENABLED
go mod download
go test ./...

$out = Join-Path $env:TEMP "channelpipe-demo"
go run ./cmd/channelpipe demo --out $out --workers 2

go run ./cmd/verify `
  --sample (Join-Path $out "sample") `
  --db (Join-Path $out "checkpoint-main.sqlite") `
  --out (Join-Path $out "standalone-verify.json")

Get-Content (Join-Path $out "validation-report.md")
```

不传 `--out` 时，`demo` 会在系统临时目录创建 `channelpipe-demo-*`。Linux/macOS 使用相同命令，把 PowerShell 路径替换为 `"$(mktemp -d)/demo"` 即可。

仅生成样本：

```bash
go run ./cmd/channelpipe generate --out ./artifacts/sample
```

## 实际验证结果

最近一次标准样本和测试结果记录在 `VALIDATION.md`，原始命令输出保存在 `reports/`。当前结果：

- `go test ./... -count=1`：PASS。
- `go test -race ./internal/acceptance -count=1`：PASS。
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`：PASS。
- 标准样本：3 张图，覆盖 `320x240 RGBA8`、`1024x512 BGRA16`、`1280x720 RGBA16`。
- 真值：36 个采样点、108 个条目采样；独立工具扫描 4,568,064 个条目阶段像素。
- 实时增量、断点续跑、一次性全量三条路径：5 个修订版的像素、阶段摘要和分块提交序列一致。

## 代码结构

- `errors.go`：`E_VERSION`、`E_SCHEMA`、`E_LOSSY`、`E_STATE`、`E_DIFF`、`E_CHECKPOINT` 等统一错误码。
- `model.go`：内部 `Plan`、`Stage`、`Buffer`、矩形分块、规范 JSON 和 SHA-256 辅助。
- `plan_codec.go`、`plan_v1.go`、`plan_v2.go`、`plan_v3.go`：三版本探测、解析、默认值补齐和字段路径校验。
- `export.go`：导出到 v1/v2/v3、默认字段降级和 `E_LOSSY` 损失清单；`ExportSetAsStrict` 在有损失项时返回 `E_LOSSY`。
- `convert.go`：整型位深换算、通道重排、填充、丢弃标识、alpha 处理、内容哈希和缓存键。
- `graph.go`：阶段依赖图、区域依赖、参数/顺序编辑失效范围。
- `state.go`：`pending -> processing -> committed/failed` 双层状态机和非法迁移错误。
- `compare.go`：阶段缓冲区摘要与首个不一致坐标、条目、字段定位。
- `imageio.go`：标准库 PNG 预览输出。
- `internal/sample`：固定种子样本、独立构造真值、原始像素文件和清单写入。
- `internal/checkpoint`：SQLite schema、原子分块事务、串行写入、快照身份和哈希校验。
- `internal/runner`：worker 池、像素缓冲门、有序提交、失败隔离、恢复和增量重算。
- `internal/independent`：只通过 `database/sql` 直读 SQLite BLOB、manifest 和 truth 的独立验证器；不调用 `ApplyStage` 或 runner 主链路。
- `internal/acceptance`：完整本地验收场景和 Markdown/JSON 报告。
- `cmd/channelpipe`：完整示例入口。
- `cmd/verify`：独立验证命令。

## 三版本计划差异

加载入口是 `channelpipe.LoadPlanSet([]byte)`：

| 版本 | 版本字段 | 典型差异 |
|---:|---|---|
| v1 | `plan_ver` | `jobs[].tasks[]`、`src_bitness`、`depth_code=D8/D16`、字符串布局 `BGRA`、`index_map`、数字 `alpha_rule`、`pad` 填充来源 |
| v2 | `schemaVersion` | `pipelines[].operations[]`、`source.bitDepth/pixelFormat`、`quantization.outputDepth`、`channelFlow.sources` 语义通道引用、`fillSource.samples`、`alphaConvention` |
| v3 | `version` | `plans[].stages[]` 直接使用内部模型：`target_depth`、`swizzle`、`fills`、`alpha_mode` |

错误约定：

- 未知版本：返回 `E_VERSION`，`Error.Field` 指向实际版本字段，例如 `plan_ver`。
- 必填字段缺失：返回 `E_SCHEMA`，`Error.Field` 是完整路径，例如 `plans[0].width` 或 `plans[0].stages[1].target_depth`。
- 非法状态迁移：返回 `E_STATE`，存储层不更新原状态。
- 三路径像素不一致：返回 `E_DIFF`，`Mismatch` 含 `image_id`、`x`、`y`、`stage_index`、`field`、期望值和实际值。

## 版本往返与损失痕迹

低版本加载时，省略的 `round_mode`、`drop_policy`、`alpha_rule` 等字段会按默认值补齐，并在 `LoadResult.UpgradeTraces` 中记录：

- `field_path`
- `old_value`（缺失时为 JSON `null`）
- `source_default`
- 所属 plan/stage 和原因

导出低版本时，核心语义字段（目标位深、通道顺序、填充/丢弃、alpha）均可往返。任意 `metadata` 等低版本未声明字段会进入 `LossReport.Items`，每项带 `code=E_LOSSY`、字段路径和 `disposition`。示例生成的 v1/v2 损失仅丢弃非语义元数据，因此重新加载后 `SemanticEquivalentPlan` 为 true。

## 整型换算

所有位深和 alpha 预乘换算均使用 `uint64` 整数运算：

```go
scaled := QuantizeRatio(uint64(value)*toMax, fromMax, rounding)
```

支持 `nearest`、`floor`、`ceil`，没有浮点累积。8 到 16 的标准扩展为 `v * 257`，16 到 8 使用同一整数比例公式。

## 缓存键与失效图

每个“图像分块 × 条目”的缓存键输入为：

```text
SHA256(cache_version, input_region_hash, stage_index, stage_params, upstream_key, param_version, algorithm_version)
```

首个条目的 `upstream_key` 为 `source:<源区域哈希>`；后续条目使用上一条缓存键。SQLite 的 `chunk_outputs` 持久化 `cache_key`、`output_hash`、计划指纹、参数版本和算法版本。

编辑位深、置换、填充/alpha 或顺序时，`InvalidatePlanEdit` 输出每个受影响的 `stage_index`、`chunk_index`、实际 `Rect` 和原因。顺序链中只有被改条目及其下游失效；其前条目可在增量提交时复用，同时仍随该分块所有条目在一个 SQLite 事务中重写身份字段，避免同一快照内新旧指纹混用。

## 状态机与检查点

合法迁移：

```text
pending    -> processing
processing -> committed | failed | pending(crash recovery)
failed     -> processing | pending
committed  -> pending(explicit graph invalidation/new revision)
```

每个分块提交是一个 SQLite 事务，写入该分块全部条目的 BLOB、缓存键、输出哈希、计划指纹、参数版本、算法版本和提交日志。连接池限制为 1，另有互斥锁串行化写入；执行器按分块序号有序收集结果，因此正常路径提交序列稳定。

重新打开数据库时会检查：

- `meta.plan_fingerprint`
- `meta.param_version`
- `meta.algorithm_version`
- 每行身份字段
- 每个 BLOB 的重算内容哈希

任一不符都会清空旧表并以新身份重建快照，不会部分混用旧结果。

## 并发与资源边界

- 默认 worker 数为 2，可通过 `--workers` 调整。
- `BufferGate` 限制总缓冲预算为 `workers * PerThreadBufferPixels`，并拒绝超过单线程上限的分块任务。
- 缓冲不足时阻塞等待，恢复后继续，不丢弃任务。
- `queue_events` 记录 `worker-slot` 和 `pixel-buffer-budget` 的原因与等待毫秒数。
- 注入的单分块错误只将该分块置为 failed；其他分块继续提交，重试成功后不污染已提交数据。

## 输出文件

一次完整 demo 的输出目录包含：

- `sample/images/*.chimg.json`、`*.pixels.bin`、`*.preview.png`
- `sample/plans/plan-v1.json`、`plan-v2.json`、`plan-v3.json` 及 `*.lossy.json`
- `sample/truth/truth.json`
- `sample/manifest.json`
- `checkpoint-main.sqlite`
- `demo-report.json`
- `validation-report.md`
- `three-path-report.json`
- `invalidation-report.json`
- `cache-keys-chunk0.json`
- `queue-events.json`
- `standalone-verify.json`（如果运行 `cmd/verify --out ...`）
- `final/*.final.bin`、`*.final.png`、`*.summary.json`

## 集成方式

领域组件可直接被其他 Go 程序引用：

```go
loaded, err := channelpipe.LoadPlanSet(planJSON)
if err != nil { /* E_VERSION / E_SCHEMA */ }

ok, diff, err := channelpipe.SemanticEquivalentPlan(original, loaded.Set.Plans[0])
key, err := channelpipe.StageCacheKey(regionHash, stage.Index, stage, upstreamKey)
mismatch := channelpipe.CompareBuffers(imageID, stageIndex, expected, actual)
if mismatch != nil {
    return channelpipe.DiffError(mismatch) // E_DIFF
}
```

检查点和调度器位于 `internal/checkpoint` 与 `internal/runner`，也可以按同样的构造函数和 Config 模式接入自己的 CLI 或服务。

# 技术说明与实测验收

## 1. 架构总览

模块名：`dialoguecore`，Go 版本 `go 1.22`，零第三方依赖。根包是可集成领域内核，
`cmd/dialogue` 是本地 CLI，`web/index.html` 由 `net/http` 内嵌提供。

| 关注点 | 主要文件 |
| --- | --- |
| 业务错误码 | `errors.go` |
| 三版本 DTO、装载与字段映射 | `schema.go` |
| 版本导出、降级损失 | `export.go` |
| 往返残差与路径漂移 | `roundtrip.go` |
| 整数换算、定点 dB/指数 | `convert.go` |
| 确定性 PCM 生成 | `generator.go` |
| 独立解析真值 | `reference.go` |
| 帧量化与声学特征 | `features.go` |
| 响度求解 | `calibrate.go` |
| 状态机 | `state.go` |
| 缓存键、依赖图、精确失效 | `cache.go`、`graph.go`、`pipeline.go` |
| 检查点与恢复比对 | `checkpoint.go` |
| 有界并行调度 | `scheduler.go` |
| 诊断 API/页面/报告 | `server.go`、`diagnostic.go`、`web/index.html` |

## 2. 声明尺度

- 固定种子：`42042`。
- 采样率：`24000 Hz`；默认帧率：`25 fps`，帧长 `960` 采样。
- 样本：单声道 S16LE 描述，内部为 `[]int16`。
- 能量定点：`EnergyScale = 1_000_000`，能量值为归一化均方能量的百万倍。
- 幅度定点：`LinearScale = 1_000_000`，`1_000_000` 表示满刻度线性幅度 1.0。
- 增益：基点 BP，`10_000 BP = 1.0`；千分比 `1000‰ = 1.0`。
- 响度：毫分贝，`DBScale = 1000`；零样本使用 `-120000 mdB` 下限。
- 瞬态阈值：`18000` S16 振幅。
- 生成样本：4 个片段，每片段 5 个 960 采样逻辑帧，共 20 帧；包含三角波、
  整帧静音和覆盖峰值 `26000` 的瞬态。

## 3. 三版本 Schema

| 版本 | 组织 | 时间 | 增益 | 目标响度 | 校准标记 |
| --- | --- | --- | --- | --- | --- |
| v1 `dialogue-v1` | 扁平 `clips[]`，单 `scene` 字符串 | `start_ms` / `duration_ms` | `gain_pm` 千分比 | `target_rms_pm` | 不支持 |
| v2 `schema_version=2` | 扁平 `clips[]`，`scenePath` | `startSample` / `sampleCount` | `gainBasisPoints` 万分比 | `targetLoudnessCentibel` | 不支持 |
| v3 `schema_version=3` | `scenes[].children[]` 嵌套树 | 采样点 | `gain_linear_micro` | `target_loud_mdb` + `target_amp_micro` | `calibration_markers` |

装载入口：`LoadDocument(data, sourceID)`。未知版本、坏 JSON、缺失必填字段返回
`*Error{Code:"E_SCHEMA", Path, SourceID}`。默认值补齐和字段换算写入
`Project.Traces` / `FieldTrace`，包含字段路径、旧值、新值、默认值来源和余数轨迹。

导出入口：`ExportProject(project, version)`。降级不静默丢字段：

- v3 → v2/v1：每个无法表达的场景节点生成一条 `E_LOSSY`。
- v3 → v2/v1：每个校准标记生成一条 `E_LOSSY`，处置为
  `drop_optional_calibration_marker`。
- 嵌套树降级为 `scene/path` 字符串，处置为
  `flatten_nested_scene_tree_to_scene_path`。

`VerifyRoundTrip` 对会话增益、目标幅度/响度、帧率、片段 ID、增益、起点、
采样数和音频哈希逐字段记录残差。v1/v2 实测均为 `OK=true`，各含 9 条损失项和
17 条残差记录。

## 4. 整数换算

`convert.go` 不使用浮点累加：

- `SamplesToMillis`：`samples * 1000 / sample_rate`。
- `MillisToSamples`：`ms * sample_rate / 1000`。
- `PermilleToBasis`：`pm * 10`。
- `BasisToPermille`：按 `floor` / `ceil` / `nearest_ties_up` 处理余数。
- `LinearToDBMilli`：Q24 定点 `log2` 级数。
- `DBMilliToLinear`：Q24 定点指数级数和整数幂。
- `IntSqrt`：整数 Newton 平方根。

每次除法可返回 `Truncation{Numerator, Denominator, Quotient, Remainder, Direction, Position}`。
乘法溢出返回 `E_OVF`，`Operands` 固定携带两个整型操作数。实测
`MaxInt64` 级输入返回 `E_OVF`。

## 5. 特征与校准

帧边界声明为：

```text
frame_start(n) = floor(n * sample_rate / fps)
first_frame   = floor(clip_start * fps / sample_rate)
last_excl     = ceil(clip_end * fps / sample_rate)
```

每帧计算：

- `energy_fixed = floor(sum(sample^2) * 1e6 / (N * 32768^2))`
- `rms_micro = isqrt(energy_fixed * 1e6)`
- `zcr_bp = floor(crossings * 10000 / (N-1))`
- 最大绝对峰位置；只有峰值达到阈值才标记 `transient=true`。

生成器通过 `reference.go` 直接遍历 PCM 产出期望真值；测试再用生产提取器核对。
实测 20/20 帧在容差 `0` 下能量通过率为 100%。容差序列
`1000,100,2,0` 的通过率均为 `1.0`，满足“收紧时单调不增”。

校准使用整个片段的定点 RMS，避免帧率重量化后“活动帧平均”随分帧变化。
求解：

```text
required_total_bp = ceil(target_amp_micro * 10000 / clip_rms_micro)
required_clip_bp  = ceil(required_total_bp * 10000 / session_gain_bp)
adjust_bp         = clamp(required_clip_bp - clip_gain_bp, -max_adjust, +max_adjust)
```

当前样本的片段调整量为
`[1085, 879, 1271, 963] BP`，绝对和 `4198 BP`，小于总预算 `12000 BP`。
若 `clip_gain + max_adjust` 仍达不到目标，`CalibrateProject` 返回 `E_LOUD`，
`Details.Gaps` 给出每个片段的当前响度、可达响度上界、目标、缺口和所需调整量。

## 6. 状态机与提交边界

状态：`pending`、`extracting`、`calibrating`、`committed`、`failed`。

合法边：

```text
pending     -> extracting | failed
extracting  -> calibrating | failed
calibrating -> committed | failed
failed      -> pending | extracting
committed   -> terminal
```

非法迁移返回 `E_STATE`，并保持原状态和 `committed/total` 进度。提交后
`FreezeClip` 保存量化区间、特征、校准和缓存键；增益编辑直接返回 `E_STATE`，
目标/帧率编辑把已提交片段放入 `frozen_clips` 且不重建其结果。

## 7. 缓存、依赖图和三路径

缓存键结构见 `cache.go:CacheKey`，必填输入包括：

- `ContentHash`：采样率、起点和 PCM 的 SHA-256。
- `RangeID`：`clip:f帧号:起点-终点`。
- `QuantVersion`：量化规则版本。
- `UpstreamKey`：特征键的上游是 decode 键，校准键的上游是特征键。
- 额外的 `ParamsVersion`：会话增益、目标、最大调整和片段增益参数哈希。

依赖链：

```text
frame_rate ─┐
clip/pcm ───┴─> feature interval ─> calibration frame <─ gain/target params
```

实测失效范围：

- 修改 `clip-01` 增益 `+200 BP`：重建 0 个特征帧、5 个校准帧。
- 修改目标响度：重建 0 个特征帧、20 个校准帧。
- `25 -> 30 fps`：生成 27 个新的具体帧区间，重建 27 个特征帧和 27 个校准帧；
  同时记录 15 条采样点帧归属变化。

三条一致性路径：

1. 一次性 `FullBuild`。
2. `Engine.BuildAll -> Apply*Edit -> RebuildPlan`，再与相同编辑后的全量构建比较。
3. 先持久化 `clip-01`、`clip-02`，模拟强杀后 `RecoverOrBuild` 恢复这两个片段，
   仅重建 `clip-03`、`clip-04`，再与全量结果比较；随后在恢复引擎上增量修改
   `clip-03` 增益，与相同修改后的一次性全量构建比较。

`CompareFrames` 对特征和校准结构做规范化哈希比较。实测恢复/全量、恢复后增量/全量、
增益增量/全量、目标增量/全量、帧率增量/全量均 `equal=true`。篡改 `project_hash` 后恢复报告会
`discarded=true` 并整体重建。

## 8. 调度预算与异常隔离

`SchedulerConfig` 控制：

- `MaxParallelism`：worker 数。
- `SampleBufferCap`：每 worker 的 `[]int16` 缓冲上限，默认 `4096`。
- `QueueBudget`：待处理任务 channel 容量。

入队先尝试非阻塞发送；队列满后记录
`reason=queue_budget_exhausted`、开始等待并在入队后记录微秒等待时长，不丢弃任务。
结果按片段下标排序后串行提交；若提供 `FileCheckpointStore`，检查点互斥写入并
用临时文件 + `rename` 原子替换。竞态检测 `go test -race` 通过。

异常映射：

| 注入场景 | 业务码 | 定位 |
| --- | --- | --- |
| PCM 声明长度大于实际样本 | `E_DECODE` | `clip_id`、`declared_sample_count` |
| 两个片段采样区间相交 | `E_OVER` | 首个 `clip_id` 与 `details.clip_ids` |
| 线性增益与 BP 增益矛盾 | `E_PARA` | `clip_id`、字段路径、两个操作数 |
| `force_stage_failure` | `E_STAGE` | `clip_id` |
| 目标超过增益上界 | `E_LOUD` | 每片段 gap |
| 非法状态迁移 | `E_STATE` | `clip_id`、from/to |
| 低版本不支持场景/标记 | `E_LOSSY` | JSON 路径、原值、处置 |
| 必填 schema 字段缺失/未知版本 | `E_SCHEMA` | `path`、`source_id` |
| 整数乘法溢出 | `E_OVF` | `field`、两个操作数 |

## 9. 本地诊断页

```bash
go run ./cmd/dialogue serve -addr 127.0.0.1:18086
```

页面提供：

- 12 项自验状态。
- 逐帧能量柱和瞬态峰标记。
- 校准前/后响度曲线和目标线。
- 五态状态矩阵。
- 增益、目标、帧率三种编辑的失效传播图、节点列表和采样归属追溯。
- 三路径比对结果。
- 九类业务码异常表。
- 版本往返残差和 `E_LOSSY` 列表。
- 20 帧明细表。

## 10. 实测记录

环境：

```text
go version go1.22.12 linux/amd64
WSL Ubuntu 26.04 兼容环境
```

最终命令与结果：

```text
go test -count=1 -v ./...     # 29/29 PASS，exit 0；日志 artifacts/go-test-v.txt
go vet ./...                  # exit 0；日志 artifacts/go-vet.txt
go test -race -count=1 ./...  # ok dialoguecore 3.564s；日志 artifacts/race-test.txt
go run ./cmd/dialogue verify  # 12/12 PASS，exit 0
```

机器可读报告：

- `artifacts/verification-report.json`
- `artifacts/diagnostic.json`

HTTP 冒烟在 `127.0.0.1:18093` 完成（默认 `18080` 在当前共享环境被无关进程占用，
因此冒烟改用空闲端口；CLI 默认端口已设为 `18086`）：

```json
{"ok": true, "frames": 20, "checks": 12, "errors": 9, "combined_path": true}
```

## 11. 集成建议

根包可直接导入：

```go
p, truth, _ := dialogue.GenerateProject(dialogue.GeneratorOptions{})
rows, bizErr := dialogue.FullBuild(p)
engine := dialogue.NewEngine(p)
plan, editErr := engine.ApplyClipGainEdit("clip-01", 10200)
rows, bizErr = engine.RebuildPlan(plan)
```

生产集成时可把 `FileCheckpointStore` 替换为数据库事务实现；只需保持
“按稳定片段顺序串行提交已完成帧区间”的边界即可。

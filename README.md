# dialogueforge

面向游戏配音管线的确定性对白响度内核与本地诊断工作台。项目只使用 Go 标准库，不依赖外部声学库、录音素材、网络服务或容器。固定种子 `42` 生成 2 个场景、4 个片段、18,560 个单声道采样点和 112 个逻辑帧；PCM、音素标注、工程参数与真值均由生成器构造。

## 快速开始

```bash
go test ./...
go run ./cmd/samplegen -out samples -seed 42
go run ./cmd/verify -seed 42
go run ./cmd/dialogue-workbench \
  -addr 127.0.0.1:8080 \
  -sample samples/dialogue-v3.json \
  -checkpoint artifacts/workbench.checkpoint.json
```

浏览器打开 `http://127.0.0.1:8080/`。也可使用 `make test`、`make samples`、`make verify`、`make workbench`。

## 目录

| 路径 | 内容 |
| --- | --- |
| `dialogue/` | 可集成领域内核：三个 schema、定长换算、包络、校准、状态机、依赖图、检查点、三路径验证 |
| `dialogue/generator.go` | 固定种子 PCM/标注/真值生成器 |
| `dialogue/schema.go` | v1/v2/v3 原生结构、统一加载器与校验 |
| `dialogue/export.go` | 三版本导出与 `E_LOSSY` 降级清单 |
| `dialogue/envelope.go` | 逐帧定点能量、峰值、分段归一化包络 |
| `dialogue/calibration.go` | 逐片段增益调整、目标响度与不可达界 |
| `dialogue/pipeline.go` | 增量重建、检查点提交、编辑失效 |
| `dialogue/verify.go` | 实时、恢复、全量三路径与 `E_DIFF` 定位 |
| `cmd/samplegen/` | 写出 v1/v2/v3 样本和清单 |
| `cmd/verify/` | 一键验收，输出 JSON 报告 |
| `cmd/dialogue-workbench/` | `net/http` 页面、状态 API、编辑 API |
| `samples/` | seed `42` 的确定性样本与 `manifest.json` |
| `docs/ACCEPTANCE.md` | 本轮实际命令与验证结果 |

## Schema 与内部模型

内部模型只有采样点和万分比（`1/10000`）定点数，不保存浮点时间或浮点增益。

| 维度 | v1 | v2 | v3 |
| --- | --- | --- | --- |
| 版本标识 | `"1.0"` | `"2.0"` | `"3.0"` |
| 主要命名 | `snake_case` | `camelCase` | 混合/嵌套 |
| 时间基准 | 采样点 | 毫秒 | 采样点 |
| 片段表 | 扁平 `phrases` | 扁平 `segments` + `sceneId` | `scenes[].segments[]` 树 |
| 增益刻度 | 千分比 | 万分比 | 千分比 |
| 响度刻度 | 千分比 | 万分比 | 万分比 |
| F0 折点 | 无，只有基频标量 | 有，`atMs` | 有，`sample` |

统一加载器 `LoadProject` 自动识别版本。加载时校验：

- PCM 为单声道 `pcm_s16le`，`samples` 数量等于 `declared_total_samples`；
- 声明总时长与采样点/毫秒基准一致；
- 片段与音素均严格递增且不重叠，音素位于所属片段内；
- 音素类别只能是 `vowel`、`consonant`、`silence`；
- 片段会话增益位于声明的 `[sessionGainMin, sessionGainMax]`；
- 真值帧数、帧几何与片段一一对应；
- 缺失字段返回 `E_SCHEMA`，同时带对白标识和字段路径。

v1 加载时补齐默认场景组和常量 F0 折点；v2 在缺失 `sceneId` 或 F0 折点时补齐。每次补齐进入 `LoadResult.Conversions`，记录路径、旧值、默认值、来源与处置说明。`ConvertVersion` 执行显式升/降级并保留这些痕迹。

降级时 v1 无法表达场景树或 F0 曲线，v2 无法表达场景显示名，或毫秒时间基准产生取整时，`ExportProject` 返回 `Lossy` 清单，每项使用 `E_LOSSY` 并写明降级处置；导出文件仍可被目标版本加载。`CompareSemantic` 对目标版本可表达字段检查读回一致性，并按该版本量化残差比较。

## 定点换算

`dialogue/rounding.go` 只使用整数和 `math/big` 防止中间乘积溢出：

- 采样点 ↔ 毫秒：`ms * sampleRate / 1000` 或 `samples * 1000 / sampleRate`；
- 千分比 ↔ 万分比：`value * 10000 / 1000` 及其反向；
- 定点值和时长采用声明的 `half-away-from-zero`；
- 每次有余数都记录 `Truncation{Location, Field, Operation, OperandA, OperandB, RoundedValue, Remainder, Residue, Mode}`；
- 结果超出 `int64` 返回 `E_OVF`，携带字段和两个操作数。

千分比降到万分比再升回会落在最多 5 个内部单位内；v2 毫秒时间基准的单次换算最大产生半个毫秒的时间残差。v1 真值比较容差因此声明为 5 个内部单位，v2/v3 为 2 个内部单位。

## 包络与校准

对片段 `[start,end)`，帧数为：

```text
frameCount = floor((segmentLength - frameLength) / hop) + 1
```

每帧 `N` 个采样点执行：

```text
energy        = sum(sample^2)
weightedSum   = sum(sample^2 * phonemeWeight)
weightedMean  = floor(weightedSum / N / 10000)
rms           = floorSqrt(weightedMean)
rawEnvelope   = floor(rms * 10000 / 32768)
peakPosition  = first max(abs(sample))
envelope      = round(rawEnvelope * 10000 / segmentMaxRaw)
```

静音音素权重强制为零。分段归一化后的包络为内部万分比。每个除法步骤的余数和舍入残差进入 `Truncation`。实现为纯函数，并额外提供按片段并行测量；输出按片段索引写回，结果与顺序执行逐帧完全一致。

校准对每个片段：

```text
requiredGain   = round(targetLoudness * 10000 / baseLoudness)
adjustment     = clamp(requiredGain - sessionGain, -gainLimit, +gainLimit)
appliedGain    = clamp(sessionGain + adjustment, gainMin, gainMax)
calibratedF[i] = round(envelope[i] * appliedGain / 10000)
```

片段达标要求校准后平均响度与目标的偏差不超过 2 个内部单位。若 `[gainMin,gainMax]` 内不可达，返回 `E_LOUD`，带目标响度、可达下界、可达上界和全部受限片段；不会静默截断或伪造达标。

生成器真值在 PCM 合成阶段使用独立的真值循环计算，不从 `MeasureSegmentEnvelope` 或校准器输出回填。

## 状态与增量

片段状态为 `pending -> measuring -> calibrating -> committed`，任一步可进入 `failed`，失败可重试回 `pending`；`committed` 不可直接迁移。非法迁移返回 `E_STATE`，原状态和提交进度不变。状态机对外返回副本，已提交包络和校准不能被调用方原地改写。

依赖图节点类型：

- 项目目标响度、项目帧配置；
- 片段信号（边界、音素权重、PCM）；
- 每个帧区间；
- 片段包络；
- 片段校准。

缓存键由信号/内容哈希、帧区间标识、量化参数版本、算法版本及上游节点键计算。编辑失效范围：

| 编辑 | 失效节点 | 复用 |
| --- | --- | --- |
| 片段会话增益 | 该片段校准 | 包络与帧区间 |
| 目标响度/增益上界 | 全部片段校准 | 全部包络与帧区间 |
| 帧长/帧移 | 全部信号、帧、包络、校准 | 无 |
| 片段 PCM | 该片段信号及其下游帧、包络、校准 | 其他片段 |

`InvalidationPlan` 同时输出 `invalidated`、`rebuild_order`、`reused` 和受影响片段。重建只计算键不匹配或显式失效的节点。

## 检查点与三路径

`CheckpointStore` 将记录整体序列化到同一文件，使用同目录临时文件、`fsync`、原子 `rename` 和互斥锁串行提交。记录包含片段内容哈希、信号哈希、包络摘要、增益调整量、源版本、量化版本、算法版本以及恢复所需的包络/校准数据。已存在片段保留原提交序号，新片段取下一个稳定序号。

重启验证逐片段检查：

- 当前内容哈希和信号哈希；
- 源 schema 版本；
- 量化参数版本和算法版本；
- 包络摘要及校准中的包络摘要；
- 增益调整量一致性。

任一不匹配都会使整段失效重算。

`VerifyThreePaths` 对同一工程和编辑序列运行：

1. 实时路径：按提交边界持续写检查点并复用有效缓存；
2. 恢复路径：每个编辑/重建后重新构造管线并从磁盘恢复；
3. 全量路径：清空缓存，对最终工程一次性重算全部片段。

随后逐段、逐帧比较能量、峰值、包络、校准增益和校准后包络。第一处不一致返回 `E_DIFF`，包含 `SegmentID`、`FrameIndex`、`Field` 以及两个值。

## HTTP 工作台

页面和 API 仅使用 `net/http`：

- `GET /`：逐场景、逐帧包络曲线，播放头，片段状态，失效节点和预算占用；
- `GET /api/state`：当前完整诊断快照；
- `POST /api/edit`：提交 `{"kind":"segment-gain","segment_id":"seg-02","value":9900}`、目标响度或帧率编辑；
- `POST /api/rebuild`：仅重建失效节点；
- `GET /api/health`：算法版本与健康状态。

页面没有第三方前端依赖。预算显示逻辑帧、缓存字节、检查点字节及失效/复用节点数。

## 错误码

| 码 | 含义 |
| --- | --- |
| `E_SCHEMA` | 字段缺失、结构/枚举/区间/增益范围或声明时长不合法 |
| `E_LOSSY` | 降级时无法表达字段的处置项（非阻断） |
| `E_OVF` | 定长换算溢出，含字段和操作数 |
| `E_LOUD` | 目标响度在增益界内不可达，含上下界和受限片段 |
| `E_STATE` | 非法状态迁移，状态与进度保持不变 |
| `E_DIFF` | 三路径输出首次不一致，含片段、帧、字段 |
| `E_IO` | 检查点或文件持久化失败 |

## 实测结果

seed `42` 当前实测：

- `112` 帧、`4` 片段、`2` 场景；
- 直接生成真值包络最大误差 `0`；
- v1 读回最大误差 `5`，声明容差 `5`；v2/v3 最大误差 `0`，容差 `2`；
- 三版本 `CompareSemantic` 不一致数均为 `0`；
- 三个版本校准全部达标；
- 实时、恢复、全量路径的提交序列均为 `seg-01 → seg-02 → seg-03 → seg-04`，112 帧逐帧一致；
- v1 生成 `23` 条 `E_LOSSY`（2 个场景树 + 21 条 F0 曲线），v2 生成 `4` 条（2 个场景名 + 2 个毫秒 F0 点取整），v3 无降级项。

完整命令输出见 `docs/ACCEPTANCE.md`。

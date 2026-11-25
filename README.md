# 字幕与口型对时校准内核

这是一个可嵌入游戏服务端的 Go 领域组件，也是完全离线、固定种子的本地演示工程。它从语音片段的音素采样区间与字符朗读权重出发，生成逐帧字幕字符区间和逐帧口型，提供精确依赖失效、窗口状态机、SQLite 原子检查点、三路径对拍和本地诊断页面。

当前验证结果（种子 `42`，6 条对白）：

- 总帧数 `456`，过渡插入 `44`。
- 实时排程、SQLite 重启恢复、一次性全量重算三路径逐帧一致。
- 三路径与生成器解析真值逐帧一致，包括过渡插入帧位置。
- `E_ORDER`、`E_RATE`、`E_LIP`、`E_STAGE` 四条异常注入链路全部命中。
- 音素边界编辑和字幕权重编辑只失效依赖帧并复用 SQLite 中的未失效帧。

## 快速开始

环境要求：Go 1.22 或更高版本。Windows PowerShell 示例：

```powershell
go version
$env:Path += ";C:\Program Files\Go\bin"  # 如 go 不在 PATH
[Environment]::SetEnvironmentVariable("Path", $env:Path, "User")  # 永久写入用户环境变量
cd C:\path\to\project
go test ./...
go run ./cmd/lipsync demo -out artifacts\demo
go run ./cmd/lipsync serve -dir artifacts\demo -addr 127.0.0.1:8099
```

Linux / WSL / macOS：

```bash
go test ./...
go run ./cmd/lipsync demo -out artifacts/demo
go run ./cmd/lipsync verify -dir artifacts/demo
go run ./cmd/lipsync serve -dir artifacts/demo
```

不需要 Docker、数据库服务、音频素材或网络访问。SQLite 驱动为纯 Go 的 `modernc.org/sqlite`。

生成的 `artifacts/demo/`：

| 文件 | 用途 |
|---|---|
| `projects.json` | 固定种子对白工程 |
| `truths.json` | 生成器解析构造的逐帧真值 |
| `manifest.json` | 工程、真值与元数据清单 |
| `lipsync.db` | 窗口检查点与逐帧缓存 |
| `report.json` | 完整验证、失效、队列、比对报告 |
| `report.md` | 可读验收摘要 |
| `index.html` | 自包含本地诊断页面 |

## 领域模型

```json
{
  "dialogue_id": "dlg-001",
  "segments": [{
    "id": "seg-1",
    "voice_ref": "voice://dlg-001/seg-1.wav",
    "start_sample": 0,
    "end_sample": 72000,
    "phonemes": [{
      "id": "ph-1-1",
      "start_sample": 0,
      "end_sample": 8000,
      "lip": "silent"
    }]
  }],
  "subtitle": {"text": "你好，欢迎来到训练场。", "weights": [8, 6, 2, 7, 5, 5, 5, 7, 9, 8, 2]},
  "fps": 24,
  "sample_rate": 48000,
  "quantization": {"direction": "ceil", "version": "qint/v1"},
  "min_readable_frames": 2,
  "lip_transition_tolerance": 2,
  "algorithm_version": "lipcal-go/1"
}
```

加载入口为 `LoadProjects` / `LoadProjectsFile` 与 `LoadTruths` / `LoadTruthsFile`。字段缺失、类型错误或结构冲突返回 `E_SCHEMA`，并携带 `dialogue_id` 与点路径，例如 `projects[0].segments[0].phonemes[0].lip`。音素重叠/倒置返回 `E_ORDER`，帧率/采样率越界或矛盾返回 `E_RATE`，未知口型返回 `E_LIP`。

## 对时规则

### 1. 整型量化

所有采样点映射仅使用 `int64`：

```text
floor: sample*fps / sample_rate
ceil:  (sample*fps + sample_rate - 1) / sample_rate
round: (sample*fps + sample_rate/2) / sample_rate  // 半值向上
```

音素边界、片段边界、总时长和字幕边界共用同一方向。乘法、加法和帧数转换均检查溢出，失败返回 `E_RANGE`，并给出 `fps`、`sample_rate`、`sample` 与字段路径。实现中不存在浮点累积。对固定采样点，`fps` 增大时量化帧号单调不减。

帧 N 表示半开区间 `[N, N+1)`；覆盖帧号的音素提供该帧基础口型。量化为零宽的音素会被记录但不占据帧。

### 2. 字幕排程

`ScheduleSubtitle` 以总帧数和字符权重做整数 Hamilton 分配，先用 `min_readable_frames` 保底，再按余数、权重、字符序稳定分配剩余帧。若所有字符的最短可读约束无法同时满足，但帧数仍不少于字符数，则把有效最短停留压缩为 1 帧，并记录 `priority=all_characters_then_weight_descending`。若帧数连每个字符一帧都无法覆盖，返回 `E_SCHED` 和对白帧号。

输出满足：

- 每帧恰好一个字符半开区间 `[char_start, char_end)`；
- 所有字符 span 首尾相接、不重叠，覆盖完整字幕；
- 帧数等于量化后的对白总时长；
- 压缩策略与有效最短帧数可观测。

口型映射不读取字幕排程，因此字幕换行、字符切换不会重置口型连续性。

### 3. 口型与过渡

支持类别：`silent`、`closed`、`press`、`round`、`wide`、`smile`、`open`。

连续性族：

- 静止族：`silent`、`closed`、`press`
- 口型族：`round`、`wide`、`smile`、`open`

族内切换直接呈现；跨族切换需要一帧 `closed` 桥接，桥接帧替换新音素的第一帧并记录到 `transitions[].inserted_frames`。`lip_transition_tolerance=0` 时跨族切换返回 `E_LIP`，携带帧号与两侧类别。当前实现中容差 `>=1` 表示允许一个桥接帧；生成器默认容差为 `2`。

## 依赖、缓存与失效

每个帧记录显式依赖：

- `TimingKey`：fps、采样率、总采样、量化方向、量化版本、算法版本、最短字幕帧、过渡容差；
- `PhonemeKey`：片段 ID、音素 ID、采样区间、音素口型、上一帧基础口型；
- `SubtitleKey`：字符区间与字符权重；
- `ContentHash`：上述输入的内容哈希；
- `CacheKey`：内容哈希、帧号、量化版本、算法版本和全部上游键的组合哈希。

```text
CacheKey = H(ContentHash, frame, quantization_version,
             algorithm_version, TimingKey, PhonemeKey, SubtitleKey)
```

`ComputeIncremental` 对比新旧逐帧依赖，只重算依赖变化的帧；未变化帧必须按 `CacheKey` 从 `frame_cache` 表读取。失效清单包含对白、半开帧区间、原因与字段：

- 音素边界：`phoneme_interval_changed`
- 音素/片段身份：`phoneme_identity_changed`
- 口型/过渡：`lip_or_transition_change`
- 字符区间移动：`subtitle_schedule_shift`
- 字符权重：`subtitle_weight_changed`
- fps/采样率/量化参数：`fps_or_sample_rate_changed`
- 新帧/帧删除/缓存缺失：`new_frame`、`frame_removed`、`cache_miss`

报告中 `frame_end` 是排他上界。同签名相邻失效帧会合并为区间。

## 窗口状态机与检查点

固定帧窗口默认 12 帧。合法迁移：

```text
pending -> scheduling -> lip_mapping -> committed
                    \-> failed
scheduling ---------> failed
failed -> pending   // 显式重试
```

非法迁移返回 `E_STATE`，状态和进度保持不变。每个窗口提交时，`CommitWindow` 在一个 SQLite 事务中同时写入：

- 逐帧字符区间摘要；
- 逐帧口型序列；
- 逐帧依赖缓存键与缓存行；
- 已消费音素偏移和采样点；
- 工程内容哈希、量化版本、算法版本；
- 检查点 payload 哈希。

`Store` 使用互斥锁与 `MaxOpenConns(1)`，写入串行且按窗口索引稳定提交。窗口可在工作线程池并行计算，但提交顺序固定。

重启恢复时，`RunResume` 只接受 SQLite 记录。它验证窗口连续性、payload 哈希、工程哈希、量化/算法版本、被引用 `frame_cache` 行的哈希/版本/输出。任一不匹配会删除该对白的检查点与逐帧缓存，并从头全量重算，返回 `reset_reason`。有效前缀则只恢复该前缀，后续窗口继续提交。

## 三路径

- `RunRealtime`：有界并发窗口排程、口型映射和顺序检查点提交。
- `RunResume`：新 `Store` 从 SQLite 恢复版本匹配窗口，再计算剩余窗口。
- `BuildTimeline`：不读检查点的全量纯构造路径。

`CompareFrames` / `CompareTransitions` 自动定位首个不一致的 `dialogue_id`、`frame` 与字段名。演示同时与生成器真值比较，真值由 `ConstructTruth` 在运行缓存之前独立构造。

## 并发与资源边界

- `MaxWorkers` 控制同时执行的窗口 worker 数，默认 2。
- 每个 worker 的待处理帧队列上限由 `FrameBufferLimit` 控制，默认 4。
- 缓冲耗尽时 worker 阻塞、先消费队首帧，并记录 `frame_buffer_exhausted_blocking`；不会静默丢帧。
- 共享 SQLite 写入串行；完成计算后按窗口索引依次提交。

诊断页展示音素/帧边界叠加、逐帧字符与口型、失效传播、三路径比对、窗口状态、排队事件和错误注入定位。

## 错误码

| 错误码 | 场景 | 关键定位字段 |
|---|---|---|
| `E_SCHEMA` | 字段缺失、类型错误、权重数量不符、真值结构错误 | `dialogue_id`、`field`、`path` |
| `E_ORDER` | 片段/音素重叠、倒置或非法递增 | `dialogue_id`、`field` |
| `E_RATE` | fps/采样率越界或矛盾 | `fps`、`sample_rate` |
| `E_RANGE` | 整型换算溢出、零帧时长 | `sample`、`fps`、`sample_rate` |
| `E_SCHED` | 字幕无法覆盖或排程冲突 | `dialogue_id`、`frame` |
| `E_LIP` | 口型越界、跨族超容差 | `frame`、`left`、`right` |
| `E_STATE` | 非法窗口状态迁移 | `current`、`expected`、`window` |
| `E_STAGE` | 窗口内部失败、检查点写失败 | `window`、`frame`、`stage` |

## 代码结构

| 文件 | 内容 |
|---|---|
| `model.go`, `load.go`, `truth_load.go` | 领域模型、严格加载、真值校验 |
| `quantize.go` | 整型采样点到帧换算与边界清单 |
| `schedule.go` | 字幕字符区间排程 |
| `lip.go` | 逐帧口型与过渡连续性 |
| `timeline.go`, `truth.go` | 纯构造链路与生成器真值 |
| `dependency.go` | 依赖图、缓存键、精确失效 |
| `state.go` | 窗口状态机 |
| `store.go` | SQLite 原子检查点与逐帧缓存 |
| `engine.go` | 并发窗口、实时/恢复、增量计算 |
| `compare.go` | 首差定位 |
| `generator.go` | 稳定 PRNG 与固定种子样本 |
| `demo.go`, `html.go` | 报告、验收产物和诊断页面 |
| `cmd/lipsync` | `demo` / `serve` / `verify` CLI |

## 测试与验收

```bash
go test ./...
go test -race ./...
go run ./cmd/lipsync demo -out artifacts/demo
go run ./cmd/lipsync verify -dir artifacts/demo
```

测试覆盖：

- 三种量化方向的已知点、单调性和溢出；
- 缺失字段路径、权重数量、音素倒置、非法口型、非法帧率；
- 字幕完整覆盖、最短帧压缩和不可满足 `E_SCHED`；
- 口型切换、桥接帧、字幕边界不影响口型、高帧率下基础口型顺序；
- 窗口合法/非法迁移与进度保持；
- 检查点恢复、部分前缀恢复、哈希/版本篡改整体作废；
- 音素边界/字幕权重只失效依赖帧并从 SQLite 复用；fps 改变全量失效；
- 有界缓冲阻塞事件、`E_STAGE` 注入、首差字段定位；
- 固定种子确定性、完整 demo 产物和真值校验。

实际验证记录以重新生成的 `artifacts/demo/report.json` 和 `report.md` 为准。


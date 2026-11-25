# 音素边界到逐帧口型包调度原型

这是一个可集成的 Go 领域组件，同时提供本地可重复的诊断入口。固定种子生成器会产出对白工程、逐音素标注、口型包/逐帧口型真值；生产链路完成量化、归并、逐帧映射、通道调度、依赖失效、SQLite 检查点和三路径恢复。

## 状态与实测结果

- Go：`go1.22.12 linux/amd64`
- SQLite：`github.com/mattn/go-sqlite3 v1.14.24`，使用仓库内 vendor 的 SQLite amalgamation，通过本地 CGO 构建。
- 离线构建：`vendor/` 已落盘到工作目录；后续 `go test -mod=vendor ./...` 与 `go run -mod=vendor ...` 不需要联网。
- 固定种子：`20261126`
- 当前样本：6 条对白、40 个口型包、390 个逐帧口型、16 个过渡帧。
- 三路径验收：实时、断点恢复、全量重算的包划分、逐帧口型、逐帧下发集合、分片提交序列完全一致。
- 调度样本：40 个包受理，8 个包发生过延后重试，0 个包被拒。
- 失效样本：编辑 `dlg-01/ph-01` 权重后仅 `dlg-01/pkg/001` 失效，其余 39 个包从 SQLite 包缓存复用。
- 恢复样本：先提交 `shard-000`，恢复运行只承认该已提交且版本匹配的分片，最终提交序列仍为 `shard-000#1` 到 `shard-005#6`。

实际汇总见 `artifacts/demo_summary.json`。最近一次运行输出：

```text
truth_verified=true path_parity=true dialogues=6 packages=40 frames=390 transitions=16
dispatch accepted=40 deferred=8 rejected=0 commit_sequence=[shard-000#1 shard-001#2 shard-002#3 shard-003#4 shard-004#5 shard-005#6]
recovered_shards=[shard-000]
invalidation packages=[dlg-01/pkg/001] reused=39 sqlite_cache_hits=39
error_check E_CONFLICT   E_CONFLICT
error_check E_DECODE     E_DECODE
error_check E_DIFF       E_DIFF
error_check E_LIP        E_LIP
error_check E_ORDER      E_ORDER
error_check E_OVF        E_OVF
error_check E_PARA       E_PARA
error_check E_RATE       E_RATE
error_check E_SCHEMA     E_SCHEMA
error_check E_STAGE      E_STAGE
error_check E_STATE      E_STATE
```

## 运行

```bash
go version
go test -mod=vendor ./...
go vet -mod=vendor ./...

# 生成样本与真值
go run -mod=vendor ./cmd/lipsync generate -out artifacts -seed 20261126

# 三路径、真值、恢复、调度不变量和异常码的总验收
go run -mod=vendor ./cmd/lipsync demo -out artifacts -seed 20261126 \
  -workers 2 -worker-buffer 1 -queue-budget 1
```

`CGO_ENABLED=1` 是 SQLite 驱动的前提；当前机器已实测可用。所有生成物、SQLite 数据库和 WAL/SHM 文件均落在 `-out` 指定的工作目录内。

`make test`、`make demo`、`make generate` 是上述命令的简写。

## 目录

```text
cmd/lipsync/        本地诊断 CLI
pkg/domain/         工程模型、加载校验、稳定错误码
pkg/quant/          采样点->帧号、千分比<->万分比
pkg/merge/          音素->口型包确定性归并
pkg/lip/            逐帧口型与过渡桥帧
pkg/schedule/       逐帧预算、通道槽位和延后重试
pkg/dependency/     依赖 DAG、缓存键、精确失效、SQLite 包缓存
pkg/checkpoint/     六态分片状态机、单事务检查点、版本匹配恢复
pkg/workpool/       有界并行度、每线程缓冲和队列预算
pkg/pipeline/       三路径执行、提交排序、E_DIFF 比对
pkg/sample/         固定种子样本和解析真值
pkg/verify/         真值与调度不变量校验
artifacts/          生成样本、真值、SQLite 检查点、验收摘要
vendor/             离线构建依赖
```

## 工程契约

工程是严格 JSON。必填字段缺失会返回 `E_SCHEMA`，错误里包含具体路径和可定位时的音素 ID。

```json
{
  "version": 1,
  "seed": 20261126,
  "recipe": {
    "version": "recipe-v1",
    "frame_rate": 50,
    "quantize": "nearest",
    "affinity_threshold": 600,
    "transition_tolerance": 1,
    "max_phonemes_per_package": 3,
    "max_packages_per_frame": 2,
    "max_packages_per_channel_frame": 1,
    "max_defer_frames": 3,
    "algorithm_version": "lipsync-prototype-v1",
    "channels": [
      {"id": "speaker-a", "slots": 1},
      {"id": "speaker-b", "slots": 2},
      {"id": "speaker-c", "slots": 1}
    ]
  },
  "dialogues": [
    {
      "id": "dlg-01",
      "channel": "speaker-a",
      "pcm": {
        "format": "s16le",
        "sample_rate": 48000,
        "bits_per_sample": 16,
        "channels": 1,
        "sample_count": 115200
      },
      "phonemes": [
        {
          "id": "dlg-01/ph-00",
          "start_sample": 0,
          "end_sample": 4800,
          "category": "silence",
          "weight_permille": 100,
          "affinity_to_next": 0
        }
      ]
    }
  ]
}
```

音素加载校验：

- 区间必须 `start < end`、首尾在 PCM 内、按声明顺序严格递增且不重叠；违反返回 `E_ORDER` 并给出相邻音素 ID。
- 类别只允许 `vowel`、`consonant`、`silence`。
- `weight_permille` 必须为正。
- 帧率只允许 `24/25/30/48/50/60/120`，否则 `E_RATE`。
- 包上限、亲和度、容差、通道槽位等参数越界返回 `E_PARA`。
- PCM 只支持单声道 `s16le/16-bit` 描述；不支持的格式返回 `E_DECODE`。

## 确定性算法

### 量化

帧号计算全程使用整数：

```text
numerator   = sample * frame_rate
floor_frame = numerator / sample_rate
remainder   = numerator % sample_rate
```

`floor` 取 `floor_frame`；`ceil` 在余数非零时加一；`nearest` 在余数不小于半采样周期时加一。每个音素起止边界都会输出一条 `Truncation` 记录，包含原始采样点、分子、分母、余数、量化结果和是否调整。乘法溢出返回 `E_OVF`，并携带字段名与两个操作数。

持续权重由千分比转万分比时执行 `permille * 10`，向下换算使用整数除法。没有浮点累积。

### 归并与口型包

输入会先按 `start_frame/start_sample/phoneme_id` 归一化排序，再顺序分组。相邻音素合并需同时满足：

- 当前包音素数小于 `max_phonemes_per_package`。
- 前后音素都不是静音。
- 前音素的 `affinity_to_next >= affinity_threshold`。

包 ID 为 `<dialogue>/pkg/<三位序号>`，输出起止帧、包内音素 ID 和包内序号。低于阈值的亲和度、静音边界和包大小上限都会形成硬分界。

### 逐帧口型与过渡

基础映射为 `vowel -> open`、`consonant -> closed`、`silence -> rest`。类别族按 `rest(0) < closed(1) < open(2)` 排列：

- 相邻帧类别族距离不大于 1 时不插入桥帧。
- 距离大于 1 时由中间族生成桥帧，例如 `rest -> open` 在目标起始帧插入一帧 `closed`。
- 插入数量不得超过 `transition_tolerance`；边界空间不足、过渡冲突或容差不足返回 `E_LIP`，并带包 ID、帧号和两侧类别。
- 该原型不拉长时间轴；桥帧确定性地覆盖边界附近的帧。所有插入位置和依据记录在 `Transitions`。

### 通道调度

每个包在 `[start_frame, end_frame)` 内拥有下发窗口；最晚尝试帧同时受 `max_defer_frames` 和包结束帧约束。每个尝试帧消耗：

- 全局一个逐帧包预算；
- 对应通道一个槽位，且不超过 `max_packages_per_channel_frame` 和通道 `slots`。

容量不足时记录 `defer`，下一帧记录 `retry`，成功记录 `accept`；窗口耗尽记录 `reject`。拒绝包不进入任何逐帧下发集合、不占预算。未知通道、重复包 ID、非法帧区间等无法裁决的情况返回 `E_CONFLICT`。

### 依赖与失效

依赖图的节点包括对白根、音素边界、持续权重、过渡亲和度、量化结果、口型包和逐帧下发结果。缓存键由以下内容共同决定：

```text
对白静态内容哈希 + 节点身份/音素标识 + 配方参数版本 + 排序后的上游缓存键 + 节点 payload
```

编辑音素边界或权重只会改变对应量化节点、包含该音素的包和其下游下发节点；切换帧率会改变根参数并通过上游键传播到全部包。失效报告同时输出 `InvalidatedPackages`、`InvalidatedDispatchPackages` 和可复用的 `ReusedPackages`。包结果可写入 SQLite `cache_entries`，未失效包按新图缓存键复用。

### 状态机与检查点

分片合法状态迁移：

```text
pending -> quantizing -> merging -> dispatching -> committed
   |           |            |             |
   +-----------+------------+-------------+-> failed
```

非法迁移返回 `E_STATE`，状态与进度不变。每个提交分片在单一 SQLite 事务内写入：

- 包划分摘要；
- 逐帧下发 JSON；
- 内容哈希；
- 配方版本；
- 算法版本；
- 单调提交序号。

Store 只保留一个 SQL 连接并加提交互斥锁，写入串行。恢复查询只返回 `recipe_version`、`algorithm_version` 和内容哈希均匹配的已提交分片；不读取未提交的内存进度。

### 三路径和并发

- `full`：单线程顺序计算。
- `realtime`：有界 worker pool 计算，直接使用同一纯函数链路。
- `resume`：复用已提交且版本匹配的分片，重算未提交部分，提交顺序仍按 `shard-000...` 稳定排序。

worker pool 限制最大并行度、每线程缓冲和待处理队列预算。预算耗尽时提交方阻塞，并记录 `queue_wait`/`worker_wait` 的原因和等待时长。逐帧/逐包结果按索引和稳定排序汇总，不随并发完成顺序改变。

`CompareReports` 自动比较包 ID、包区间、逐帧类别、过渡位置、各通道逐帧下发集合、下发帧和提交序列；首个差异返回 `E_DIFF`，带对白 ID、帧号和字段名。

## 错误码

| 错误码 | 场景 |
|---|---|
| `E_SCHEMA` | 字段缺失、类别非法、权重非正等结构错误 |
| `E_ORDER` | 音素区间倒置/重叠、量化后顺序冲突 |
| `E_RATE` | 不支持的帧率 |
| `E_PARA` | 参数越界、亲和度越界 |
| `E_OVF` | 采样乘帧率或权重刻度换算溢出 |
| `E_LIP` | 过渡超容差、插入空间不足或口型包缺失 |
| `E_CONFLICT` | 通道/预算无法形成确定调度裁决 |
| `E_STATE` | 六态机非法迁移 |
| `E_DECODE` | JSON/对白数据/PCM 描述损坏 |
| `E_STAGE` | 单分片内部失败 |
| `E_DIFF` | 三路径或真值自动比对不一致 |

## 验收覆盖

`go test -mod=vendor ./...` 覆盖：

- 固定种子生成与加载校验、缺字段路径/音素 ID；
- 整数量化、截断证据、并发/乱序不变性、溢出诊断；
- 静音边界、亲和度阈值、包上限、归并乱序不变性；
- 桥帧插入、容差不足 `E_LIP`；
- 全局预算、通道槽位、延后/重试/拒绝事件、乱序调度不变性；
- 精确依赖失效、SQLite 持久化复用、帧率切换全失效；
- 六态机非法迁移、原子检查点、稳定提交序号和版本匹配恢复；
- 三路径真值一致、逐帧一致、提交序列一致；
- 单分片失败隔离，不写入已提交集合；
- worker pool 并行度/队列背压与等待证据；
- `E_SCHEMA/E_ORDER/E_RATE/E_DECODE/E_STAGE/E_DIFF` 等异常链路。

## 已知边界

- 样本只包含 PCM 描述，不生成实际波形；这是题面要求的单声道 PCM 描述与标注原型。
- 桥帧采用原时间轴上的确定性覆盖，不扩展包起止帧。
- 调度器的“通道槽位”是逐帧下发尝试槽位；包结束帧用于限定下发窗口。

# 架构设计

## 1. 分层

```text
PySide6 Workbench
        |
CLI / demo / selfcheck
        |
pipeline -> metadata -> model -> timebase
    |          |          |
    v          v          v
  raw      validator   package -> checkpoint -> segmented
```

核心层不依赖 Qt。`validator.py` 不导入 `package.py`、`model.py` 或 `pipeline.py`，只读取产物字节和真值文件，独立重算并比较。

## 2. 统一模型

`ContainerModel` 包含：

- `source_version`：保留来源版本标识，取值为 `v1/v2/v3`。
- `timebase` 与 `duration_ticks`：容器主轴时间和总时长。
- `tracks`：轨道 ID、类型、主语言/完整语言集合、时基、采样/帧数、时长 tick、codec、负载、编码、视频网格/音频格式、角色、HDR 和自定义字段。
- `chapters`：章节 ID、编号、起止 tick、标题、标签。
- `cover`、`cover_mime`、`tags`、`editorial`。

时间使用 `Timebase(num, den)` 表示“一个 tick 的秒数 = num/den”。所有换算经过 `Fraction`，再要求结果为整数；失败返回 `E_PREC`。

## 3. 三版本映射

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 总时长 | `duration_ms` | `timeline.duration_ticks` | `timeline.duration_us` |
| 轨时长 | `duration_ms` | `duration_ticks` | `duration_us` |
| 语言 | `language` | `language_tag` | `languages[]` + `primary_language` |
| 章节 | 数组 + 毫秒 | 字典 + tick | 帧表 `from_frame/to_frame` |
| 封面 | 顶层 `cover` | `assets.cover` | `resources.cover` |
| 自定义标签 | 顶层 `tags` | `metadata.tags` | `custom_tags[]` |

v1/v2/v3 均显式携带轨道时基，因此降级不会丢弃时基。章节时间在目标单位中不可整除时即 `E_PREC`。

## 4. 损失策略

目标版本能力矩阵：

- v1：核心轨道、封面、容器标签、章节标题；不支持附加轨道语言、轨道角色、HDR、轨道自定义字段、章节标签、编辑元数据。
- v2：v1 能力 + 轨道角色；仍不支持附加轨道语言、HDR、轨道自定义字段、章节标签、编辑元数据。
- v3：全部字段。

导出先计算 `E_LOSSY` 警告清单和 `mapping_trace`。警告不阻断导出；`project_model()` 给出目标版本可表达字段投影，用于加载回读逐项等价检查。

## 5. 裸流模型

视频 JSON 每帧包含 `pts_ticks`、网格尺寸、颜色单元和帧哈希。导入校验：

- 帧哈希；
- 时间戳严格递增；
- 相邻时间戳差为 `frame_interval_ticks` 的整数倍；
- 帧数、时基、语言与元数据及真值一致。

音频为 base64 编码的 `s16le` PCM，校验字节对齐、采样数、样本哈希、采样率/声道和时基。元数据与裸流语言/类型/属性冲突为 `E_CONFLICT`，缺轨或时基/计数不一致为 `E_TRACK`，时间顺序错误为 `E_ORDER`，负载哈希错误为 `E_CORRUPT`。

## 6. 封装格式

`.mfz` 二进制布局：

```text
MFZ1
uint64 little-endian header_length
canonical JSON header
track payloads in track_id order
SHA-256(all preceding bytes)
```

Header 包含：

- 容器/目标元数据/算法版本；
- 目标版本内嵌元数据，可被对应加载器重新读回；
- 扁平 `declaration`：轨道数、每轨计数、时基、时长、负载偏移/长度/哈希、章节、标签和封面哈希；
- `E_LOSSY` 与映射痕迹。

负载区必须无空洞、无重叠并完整覆盖。重复封装同一模型字节完全一致。

## 7. 独立校验

校验器从产物重算：

1. 文件尾部 SHA-256；
2. Header 与负载目录边界；
3. 每轨负载哈希；
4. 视频帧数/时间戳/帧哈希或音频采样数；
5. 每轨时长和容器总时长；
6. 从目标版本字段重新解析章节起止、标签集合与封面；
7. 与封装声明逐项比较；
8. 与真值清单的轨道数、每轨计数、时基、时长、章节、标签、封面和真值指纹比较。

首个差异以 `field_path`、`measured`、`declared` 返回。校验器不接收内部模型或编辑会话。

## 8. 分段检查点

每轨生成独立 `MFS1` 信封：

```text
MFS1
uint32 header_length
canonical JSON segment header
track payload
SHA-256(all preceding bytes)
```

Segment header 包含元数据版本、来源版本、参数版本、算法版本、轨道声明、负载哈希/长度。

快照目录：

```text
<checkpoint_root>/<snapshot_id>/
  .lock
  manifest.json
  segments/000_audio_main.mfs
  segments/001_video_main.mfs
```

打开快照时验证清单版本、请求签名、轨道集合、索引、文件哈希、段信封版本和负载。任一不匹配则删除全部段与清单并全量重封。已完成段可复用；失败段不写文件并记录错误；重试只重建失败/待处理段。

写入顺序为段文件原子替换，再清单原子替换；任一步崩溃留下的额外/缺失文件会在下次启动使快照整体作废。

## 9. 并发边界

- 硬上限 `MAX_ENGINE_WORKERS = 4`，请求值更大时截断为 4。
- 每个任务只持有一个轨道负载；序列化段超过 `per_thread_buffer_limit` 返回 `E_BUFFER`。
- 工作线程只做段序列化与校验；检查点写入在主线程串行执行，并受线程锁/文件锁保护。
- 请求签名包含输入模型、目标版本、参数版本、算法版本和缓冲上限，不包含线程数。
- 最终产物只由规范模型、排序后的段负载和元数据装配，因此串行/并行/恢复路径逐字节一致。

# 错误码

所有异常统一为 `WorkbenchError(code, field_path, message, measured, declared, details)`，CLI 输出 JSON，GUI 展示同结构。

| 错误码 | 触发条件 | 示例字段路径 |
|---|---|---|
| `E_META` | 未知来源元数据版本、必填字段缺失、字段类型错误、元数据 JSON 无效 | `$.metadata_version`、`$.asset.asset_id` |
| `E_TRACK` | 轨道缺失、轨道计数/时基不一致、未知轨道类型、裸流目录无轨道 | `$.tracks[1]`、`$.tracks[0].timebase` |
| `E_CHAP` | 章节起点小于 0、结束不大于开始、越界、排序后重叠 | `$.chapters[1].start_ticks` |
| `E_VER` | 未知目标版本或产物目标版本不受支持 | `$.target_version` |
| `E_LOSSY` | 低版本无法表达字段时的非致命警告代码 | `$.tracks[0].hdr` |
| `E_PREC` | 时间换算不能得到整数 tick/毫秒/微秒/帧，存在非零残差 | `$.tracks[0].duration_ms` |
| `E_CONFLICT` | 元数据与裸流/真值在语言、类型、时长、属性或标签上冲突 | `$.tracks[0].language` |
| `E_ORDER` | 视频时间戳非严格递增或间隔不对齐基准单位 | `$.tracks[0].frames[2].pts_ticks` |
| `E_CORRUPT` | 帧/样本/负载/段/产物哈希不匹配，分片损坏或结构损坏 | `$.tracks[1].payload_sha256`、`$.integrity.sha256` |
| `E_BUFFER` | 段大小超过每线程缓冲上限 | `$.segments.audio_main.buffer` |
| `E_PARAM` | CLI/GUI/注入参数非法 | `$.injection.metadata_conflict.track_id` |
| `E_IO` | 文件/目录创建、读取、写入或检查点清理失败 | `$.artifact`、`$.checkpoint` |
| `E_VALIDATE` | 独立校验发现声明、实测或真值不一致 | 首个差异路径 |

`E_LOSSY` 是非致命警告：导出继续，并在 `warnings` 与 `mapping_trace` 中留下记录。

章节重叠时错误路径指向后一个章节，`details.chapter_number` 给出章节编号，`details.previous_chapter_number` 给出冲突的前一章节编号。

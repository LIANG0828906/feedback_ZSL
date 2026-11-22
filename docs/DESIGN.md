# 设计与实现说明

## 1. 总体结构

系统由六个确定性边界组成：

1. `validation` 将事件流和能力清单转换为不可变模型，并在进入调度前返回结构化错误。
2. `scheduler` 对每个通道独立维护包窗口与合并状态，最后按 `(包起点, 通道, 通道内包序)` 统一分配全局包序号。
3. `codec` 使用固定大端协议和 CRC32，把调度包编码为设备指令包。
4. `transport` 在本机 `127.0.0.1` 上提供 TCP 模拟设备，使用确定性逻辑时钟和可配置故障计划。
5. `alignment` 将回显按包序号对齐，分离服务侧与内核侧异常，并支持单包隔离重试。
6. `persistence` 使用 SQLite `BEGIN IMMEDIATE` 逐窗口提交哈希链检查点。

`pipeline` 把上述边界组合成 `REALTIME`、`RESUME`、`PERSISTED_REPLAY` 三条路径；`workbench` 只通过这些公开边界取数，不直接修改调度状态。

## 2. 输入模型与校验

事件字段：

- `event_id`：唯一、非空字符串。
- `frame` / `frame_index`：帧号必须非递减；一旦帧号递增，帧内序号重新从任意非负值递增。
- `type`：`hit`、`shoot`、`explosion`、`footstep`、`ui`，且目标通道必须存在该类型包络模板。
- `channel`：必须存在于能力清单。
- `intensity`：有限浮点，范围 `[0, 1]`。
- `priority`：整数 `[0, 255]`。
- `no_interrupt`：布尔值。

能力字段：

- `frame_ms > 0`。
- 每通道 `min_event_interval_ms >= 0`。
- `max_events_per_packet >= 1`。
- `packet_window_ms = [min, max]`，且 `0 <= min < max`。
- 每个包络模板至少一个采样，采样有限且位于 `[0, 1]`，`sample_ms > 0`。

错误映射：

| 场景 | 代码 | 坐标 |
|---|---|---|
| 帧倒序/跳号、帧内序倒序、重复事件标识 | `E_SEQ` | `event_id`, `field` |
| 通道负值或越界 | `E_CHAN` | `event_id`, `channel`, `field=channel` |
| 强度、优先级、间隔、窗口、模板非法 | `E_PARA` | `event_id` 或 `<capability>`, `field` |

## 3. 逐通道调度

每个事件的时间按 `frame * frame_ms` 映射。每通道状态包含当前包、上一包、最后效果槽和下一包本地序号。

对每个事件按以下优先级规则处理：

1. 若当前包不存在，或事件距包起点超过 `packet_window_ms[1]`，或槽数已达 `max_events_per_packet`，关闭当前包并开新包。
2. 若最后槽仍在生效期且 `no_interrupt=True`，丢弃当前事件，决策 `DISCARD / NO_INTERRUPT_PROTECTED`。保护对后续低、同、高优先级事件均生效。
3. 若距最后事件的到达时间不超过 `min_event_interval_ms`，合并到同一槽。合并包络为逐采样最大值，优先级取最大，持续时间为二者较晚结束时间，`source_event_ids` 追加。
4. 若仍在生效期且新事件优先级不高于最后槽，丢弃，决策 `DISCARD / LOWER_OR_EQUAL_PRIORITY_ACTIVE`。
5. 若仍在生效期且优先级更高：
   - 同包内移除被覆盖的最后槽，写入新槽，决策 `INTERRUPT`；
   - 跨包时保留已关闭包，在新包接收新事件，决策 `INTERRUPT / CROSS_PACKET_PREEMPT`。
6. 否则新建槽，决策 `ACCEPT`。

合并后的效果槽数进入 `event_count`；被合并的原始事件数进入槽的 `source_count`。这同时满足“最小间隔内合成一个包络”和“包内事件数受能力上限约束”。

所有决策先按事件 `(frame, frame_index, channel, event_id)` 排序，再统一映射全局包序号。因此线程完成顺序、线程名和通道遍历顺序不会影响结果。

## 4. 设备指令包协议 HTP1

固定头部 21 字节，大端：

| 偏移 | 长度 | 字段 |
|---:|---:|---|
| 0 | 4 | `magic = "HTP1"` |
| 4 | 1 | `format_version = 1` |
| 5 | 2 | `capability_version` |
| 7 | 4 | `packet_seq` |
| 11 | 2 | `channel` |
| 13 | 2 | `timing_mark_ms` |
| 15 | 2 | `event_count` |
| 17 | 4 | `payload_length` |

每个效果槽 13 字节：`slot_index(u16)`、`offset_ms(u16)`、`type(u8)`、`priority(u8)`、`flags(u8)`、`intensity(u16)`、`source_count(u16)`、`envelope_len(u16)`，后接 8 位包络采样。`offset_ms` 从能力窗口下界起算，包分组跨度为 `window.max - window.min`。最后 4 字节是覆盖头部与载荷的 CRC32。

编码前检查：

- 槽数不超过 `max_events_per_packet`；
- `timing_mark_ms` 和相关事件偏移位于能力窗口；
- 事件类型有稳定线上编码；
- 字段宽度未溢出。

失败返回 `E_PACK`，带 `packet_seq`、`channel`、越界字段和实际值/上限。

## 5. 依赖图与增量失效

每个 `PacketDependency` 记录：

- 全局包序号、通道、通道内包序；
- 依赖的原始事件标识集合；
- 通道能力指纹和关键参数；
- 前一包序号与前一包内容哈希；
- 当前包字节哈希。

优先级编辑从该事件所属旧包开始；能力编辑从目标通道首包开始；新增事件从目标通道包含该时刻的包或下一个包开始。只有该锚点及后续同通道包标记失效。其他包从持久化检查点取回原始字节并标记 `reused=True`。失效清单包含包序号、通道、失效事件集合、原因和变更事件标识。

## 6. 状态机

合法迁移：

| 当前状态 | 允许的下一状态 |
|---|---|
| `PENDING` | `ENCODED`, `FAILED` |
| `ENCODED` | `DISPATCHED`, `FAILED` |
| `DISPATCHED` | `CONFIRMED`, `FAILED` |
| `FAILED` | `ENCODED`, `DISPATCHED`, `CONFIRMED` |
| `CONFIRMED` | 无 |

非法迁移返回 `E_STATE`，字段 `state`，详情带旧状态、请求状态和允许集合；状态保持不变。

## 7. 回环模拟设备与确定性故障

服务在本机 TCP 端口接收换行分隔 JSON，包体以十六进制传输。每个包先解析 HTP1 头部并校验 CRC，再返回：

- `received_seq`：设备接收的包序号；
- `processed_at_ms`：确定性逻辑处理时刻；
- `service_rx_index` / `delivered_index`：服务接收顺序与交付顺序；
- `attempt`：同一 run/包的尝试次数；
- `wall_time_ns`：仅用于排障，不参与自动比对。

故障计划按包序号注入：

- `drops`：首次尝试不返回回显；
- `duplicates`：首次尝试返回两次；
- `delays_ms`：延迟逻辑处理时刻，用于超时；
- `reorder_pairs`：交换两个回显的交付顺序。

重试请求走独立分支，绕过故障，且 `AlignmentReport.with_retry` 只替换目标包观测，重新聚合统计但不重算其他包。

对齐分类：

| 场景 | 代码 |
|---|---|
| 无回显 | `E_MISS` |
| 同包多个回显 | `E_DUP` |
| 逻辑处理时刻超过发送时刻 + 超时 | `E_TIMEOUT` |
| 服务交付序与本地包序不一致 | `E_REORDER` |
| 本地包序倒置/重复，或回显未知本地包 | `E_LOCAL` |

服务侧乱序不会误报 `E_LOCAL`；`E_LOCAL` 只在本地记录本身违规时产生。

## 8. 持久化检查点

每个调度包按全局包序作为窗口提交，SQLite 使用：

- `PRAGMA journal_mode=WAL`
- `PRAGMA synchronous=FULL`
- `BEGIN IMMEDIATE` 单事务
- 进程内 `RLock`

载荷包含包字节、包摘要、决策、已消费事件偏移、能力版本、能力指纹、事件流哈希和内核版本。哈希计算为：

```text
content_hash = SHA256(previous_hash || canonical_json(payload))
```

加载时逐行检查：

1. 窗口序号连续且从 0 开始；
2. 能力版本与内核版本匹配；
3. 内容哈希与哈希链前驱匹配；
4. 载荷是规范 JSON。

任一失败则整库删除，`LoadResult.invalidated=True`；恢复路径随后全量重放。

## 9. 三条路径与比对

- `REALTIME`：调度、编码、逐窗口检查点、下发、对齐。
- `RESUME`：加载有效前缀，前缀包复用持久化字节，缺失尾包重新调度编码，然后用全新模拟设备重放全序列以得到可比较的完整对齐结果。
- `PERSISTED_REPLAY`：不读取事件流，只加载检查点中的包字节与决策并下发。

`compare_results` 依次比较包存在性、通道、字节、时序标记、槽数、决策序列和确定性对齐结果。第一条差异产生 `E_DIVERGE`，记录包序号、通道、字段名及左右取值。

恢复前还会校验检查点的事件流哈希与能力清单指纹。若用户在增量编辑后直接重启，旧快照不再作为新前缀复用，而是整库重建并全量重放，避免旧包污染新事件流。

## 10. 并发边界

- 调度和编码使用 `ThreadPoolExecutor`。
- `max_parallelism` 限制工作线程数。
- `max_events_per_task` 限制每个调度任务缓冲事件数。
- `max_packets_per_task` 限制每个编码任务缓冲包数。
- 通道内状态机由单一任务顺序推进，跨通道结果先收集再规范排序。
- 包号分配在最终合并阶段完成。
- 回环记录顺序、检查点写入顺序和窗口序号保持串行稳定。

测试分别用 7 个事件/任务、3 个包/任务和 2 个工作线程，验证与单线程逐包字节、逐决策一致。


# 技术设计

## 1. 统一内部表示

`Score` 是唯一内部真值，时间坐标为四分音符拍 `Fraction`：

- `start_offset`：曲谱内容相对时间轴零点的偏移。
- `grid_resolution`：统一节拍网格，默认 `1/8` 拍。
- `time_signatures`：从 0 开始、严格递增的拍号变化。
- `tempo_segments`：连续速度段；最后一段开放结束。
- `parts`：声部、乐器、音域、音符和踏板。
- `expression`：全局或声部作用域的表情锚点。
- `provenance`：旧版升级时每个推导字段的来源标记。

外部格式按 `start_offset` 记录相对音符时间；内部表示存储绝对拍位置。解析时加偏移，导出时减偏移，因此 v1/v2/v3 往返不会重复偏移。

## 2. 解析与校验

支持单位：whole/half/quarter/eighth/sixteenth、quarter beats 和 tick/PPQ。拍号、音高、力度、连音标记、踏板区间、表情值、速度值均在解析边界校验。

音符时值与拍号矛盾的判定：

1. 音符起点与时长必须是 `grid_resolution` 的精确整数倍。
2. 拍号变化点也必须落在该网格。
3. 声部内音符按起点排序，音高与力度在业务范围。
4. 任一失败返回 `E_SCORE_INVALID`，定位 `note_id` 与 `field`。

## 3. 版本升降级

旧版升级规则写入 `provenance`：

| 来源 | 缺失字段 | 推导规则 |
|---|---|---|
| v1 | velocity | `80`，标记 `default_v1_velocity_80` |
| v1 | articulation | `normal`，标记 `default_v1_articulation_normal` |
| v1 | pedal_spans | 每个含音符小节从首个音起点到末音终点，标记 `derived_v1_bar_phrase_pedal` |
| v1/v2 | pitch_range | 取实际音高最小/最大值，标记 `derived_from_observed_*` |
| v2 | velocity | `80`，标记 `default_v2_velocity_80` |
| v2 | articulation | `normal`，标记 `default_v2_articulation_normal` |
| v2 | pedal_spans | 空踏板集合，逐音符标记 `v2_absent_pedal_spans_none` |

降级先计算损失集合：

- v3 -> v2：表情锚点、多速度段、声部分组语义、音域声明。
- v3/v2 -> v1：音高之外的表现字段、踏板、多拍号、多速度段、声部分组、音域。

默认拒绝丢失并抛出 `E_VERSION_LOSS`，`details[]` 含 `note_id`、`field`、`part_id`。显式 `allow_loss=True` 才生成降级文件，同时返回同一损失报告。

## 4. 演奏表达求解

### 时间

速度段为 `[start, next_start)`，速度单位 BPM。实际秒数使用逐段积分：

```text
seconds(beat) = sum((segment_end - segment_start) * 60 / bpm)
```

跨越速度边界的音符输出 `tempo_segments[]` 和 `note_crosses_tempo_boundary` 说明；结束时间覆盖全部段，不截断。输出同时包含 9 位小数和精确有理数字符串，便于回放与复算。

### 表情与力度

表情值在音符书写时值的中点采样，锚点间线性插值。作用域中的声部锚点覆盖同拍全局锚点。有效力度：

```text
scale = 1 - strength/2 + strength * expression
velocity = clamp(round_half_up(base_velocity * scale * velocity_scale), 1, 127)
```

`expression_strength=1` 时，表情 0.5 保持原力度，0.0 减半，1.0 增至 1.5 倍。

### 连奏与断奏

- `normal`：书写时值。
- `staccato`：书写时值 × 0.55。
- `legato`：若下一音符起点在延伸上限内，结束点对齐下一音符起点；可扩展或截短，并输出说明。无下一音符或超上限时保留书写时值。

### 踏板

踏板区间按绝对拍保留。跨小节时：

- 不切割、不丢失实际延音语义；
- `pedal_switches[]` 输出 down/up 两个切换点；
- 同时输出 `bar_segments[]` 与“跨小节连续保持”说明。

## 5. 逐音符校验

校验器不复用求解器输出作为真值，而是从 `Score` 的拍号网格、速度段、表情锚点和音域重新计算：

- 起始/结束时刻误差（毫秒）。
- 力度误差。
- 音高超出声明音域的距离。
- 序列内容哈希与乐谱哈希是否一致。

报告包含每个音符的 expected/actual/deviations/flags，不只给汇总；同时输出每个指标的极大值音符与综合最大偏差音符。容差按用户参数设置。

## 6. 确定性发布

表现序列使用规范 JSON：键排序、无空格分隔符、UTF-8、禁止 NaN/Infinity、浮点固定 9 位小数，另带精确有理数字段；不含时间戳、随机数或文件绝对路径。

内容哈希为去除顶层 `content_hash` 后规范 JSON 的 SHA-256。序列逐音符包含 `performance_hash`。

`VersionedPublisher` 在锁内把完整不可变字节快照替换为新版本；读查询在锁内取快照，因此不会看到跨版本混合音符。清单同时保存 `sequence_version`、`content_hash`、`score_hash`、参数和音符数。版本或哈希不匹配返回 `E_VERSION_STALE`。

## 7. 增量重解

支持编辑类型：

- `note_velocity`：仅目标音符。
- `expression_anchor`：逐音符比较旧/新插值值。
- `tempo_segment`：比较旧/新速度图分段积分后的实际起止秒数，仅时间改变的音符。
- `pedal_span`：仅旧/新区间覆盖的音符。
- `unknown`：无法证明依赖时全量保守重解，原因 `unknown_edit_conservative_full_resolve`。

未受影响音符直接复用缓存记录及其 `performance_hash`。最终序列重新组装后一次发布，轨迹记录 affected/reused ID 和原因。测试证明增量结果与同分全量重算逐字节一致。

## 8. 崩溃恢复

每个声部提交一个文件：

```text
payload = {
  score_hash, parameter_version, sequence_version, parameters,
  part_index, part_id, part_record
}
payload_hash = sha256_json(payload)
chain_hash = sha256_json({prior_chain_hash, payload_hash})
```

写入过程：同目录临时文件 -> flush + fsync -> `os.replace` 原子重命名 -> 目录 fsync。

恢复时：

1. 校验 schema、乐谱哈希、参数版本、序列版本、参数、声部索引与 ID。
2. 校验音符集合完整、顺序正确、每个 `performance_hash` 正确。
3. 重算 payload 哈希和链哈希。
4. 只从连续有效前缀恢复；中间缺失、断链、字段缺失返回 `E_RECOVERY_INVALID` 并定位首个断点。
5. 已提交声部不再求解/写入；未完成声部继续求解后提交。

## 9. GUI

PySide6 主线程维护乐谱与不可变版本；求解在 `QThread` 中执行，`CancellationToken` 被求解器逐音符检查。四个画布：

- 乐谱网格 + 逐音符偏差热力；
- 表情锚点曲线 + 速度段；
- 踏板区间与跨小节连续保持；
- 发布版本时间轴。

参数编辑包括表情强度、力度倍率、时间容差、力度容差。


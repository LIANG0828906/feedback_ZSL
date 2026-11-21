# 输入、二进制帧表与持久化格式

## 1. 字幕 JSON

相对路径均相对于字幕 JSON 所在目录解析。

```json
{
  "schema_version": 1,
  "material_id": "sample-mono_conflict-20261122",
  "sample_rate": 48000,
  "wav": "mono_conflict.wav",
  "frame_table": "mono_conflict.frames.bin",
  "params_version": "sample-params-v1",
  "min_readable_ratio": "1",
  "max_iterations": 192,
  "speech": [
    {
      "id": "sp001",
      "text": "The river carries the morning light.",
      "start": "00:00:00.200",
      "end": "00:00:01.600",
      "speaker": "SPEAKER_1",
      "priority": 40,
      "max_reading_rate_cps": 16.0,
      "estimated_reading_rate_cps": 11.52,
      "min_gap_ms": 0
    }
  ],
  "descriptions": [
    {
      "id": "ds001",
      "text": "A boat turns near the bridge.",
      "start": "00:00:02.080",
      "end": "00:00:03.320",
      "speaker": "AUDIO_DESCRIPTION",
      "priority": 85,
      "max_reading_rate_cps": 14.4,
      "estimated_reading_rate_cps": 8.8,
      "min_gap_ms": 120
    }
  ]
}
```

句子可以用 `start`/`end` 时间码，也可以用整数 `start_sample`/`end_sample`。同一句不能只给其中一个样本字段。所有时间统一换算到 `sample_rate` 的整数样本轴：

- 时间码：`round(seconds * sample_rate)`，使用有理数运算并四舍五入到最近样本。
- 可读下限：`ceil(text_length * sample_rate / max_reading_rate_cps)`。
- 帧区间：`frame_start = 最后一个不晚于 start 的帧起点`，`frame_end` 为半开区间终点。
- 覆盖上限：`min(WAV 样本数, frame_count * sample_rate * fps_den / fps_num)`。
- 首个错误项：字段按 `speech` 原顺序、再按 `descriptions` 原顺序校验，错误证据的 `first_conflict` 指向该句或 `frame_table`。

## 2. 自定义二进制帧时刻表

所有整数均为 little-endian。头部为 26 字节：

| 偏移 | 类型 | 字段 | 含义 |
|---:|---|---|---|
| 0 | 4 bytes | magic | 固定 `FTB1` |
| 4 | uint16 | version | 固定 `1` |
| 6 | uint32 | sample_rate | 必须与 WAV/JSON 一致 |
| 10 | uint32 | frame_count | 帧条目数量 |
| 14 | uint32 | fps_num | 帧率分子 |
| 18 | uint32 | fps_den | 帧率分母 |
| 22 | uint16 | entry_stride | 固定 `16` |
| 24 | uint16 | flags | 保留，当前为 0 |

主体为 `frame_count` 个 16 字节条目，打包格式 `<QII`：

| 类型 | 字段 | 含义 |
|---|---|---|
| uint64 | sample_index | 帧起点对应的整数样本 |
| uint32 | frame_index | 必须从 0 连续递增 |
| uint32 | flags | 帧标记，当前为 0 |

`sample_index` 必须等于 `round(frame_index * sample_rate * fps_den / fps_num)`（有理数四舍五入）。

若文件实际长度不等于 `26 + frame_count * 16`、帧索引不连续、样本起点不递增或首帧不为 0，返回 `E_TIMEBASE_INVALID` 且 `item_id="frame_table"`。

## 3. WAV 约束

- RIFF/WAVE，`fmt` 为 PCM 或 WAVE_FORMAT_EXTENSIBLE 的 PCM 子格式。
- 16-bit，单声道或 6 声道（5.1）。
- 48000 Hz。
- `data` 大小必须按 `channels * 2` 对齐。

## 4. 版本快照

每个物料只有一个快照提交文件，例如：

```text
conflict.session.json
conflict.session.trace.jsonl
conflict.session.speech.json
conflict.session.description.json
conflict.session.compliance.json
```

快照外层包含 `schema_version/version/prev_hash/payload_hash/chain_hash/history/payload`。`payload` 包含完整源句子、求解结果、描述轨、语音轨、说话人标记、合规报告、确定性轨迹日志和 `txn_id`。

- `payload_hash = SHA256(canonical_json(payload))`
- `chain_hash = SHA256(canonical_json({"previous": prev_hash, "payload": payload_hash}))`
- 每个历史项按版本严格连续，且上一项 `chain_hash` 必须等于下一项 `prev_hash`。
- 快照、轨道、报告和轨迹均通过同目录临时文件写入、`fsync`、`os.replace` 原子替换。

## 5. 编辑事务 journal

编辑先写 `<snapshot>.journal`，不覆盖已提交快照。journal 保存：

- 基版本与基链哈希；
- 编辑后的完整源句集合；
- 变更句、受影响集合和保守原因；
- 冲突消解检查点（迭代次数、链路、已做决策、每句当前解）；
- 目标版本和确定性事务 ID。

每个冲突消解迭代后原子刷新 journal。进程被终止后，`recover` 校验 journal 哈希、基快照哈希、源哈希和检查点字段，从保存的迭代继续，不重复执行已持久化的冲突消解；完成后只发布一个目标版本。

如果快照已先提交而 journal 尚未删除，恢复会根据 `txn_id` 识别“已提交”，删除 journal 并直接采用新快照。断链、缺字段、版本不符、源哈希不符或检查点损坏均返回 `E_RECOVERY_INVALID`。

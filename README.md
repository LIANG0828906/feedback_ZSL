# LCFT：本地视频多语种字幕与无障碍描述排版器

LCFT 是一个纯本地、确定性的 Python 工具包，用于把多语种语音字幕、
无障碍描述句、16-bit PCM WAV 样本索引和自定义二进制帧时刻表统一到同一条
整数时间轴，并在阅读速度、最短显示时长、句间间隔和描述优先级约束下发布
可恢复的排版版本。

实现约束：

- Python 3.10+，NumPy 仅用于确定性样本音频生成。
- 不使用 Docker、外部服务、系统时间或全局随机状态。
- 规范化时间单位是一个 48 kHz 音频样本，即 `1 tick = 1 sample`。
- 全部结论、预算降级阶段与轨迹均可序列化为规范 JSON，并以 SHA-256 哈希链发布。

## 快速开始

```bash
python3 -m pip install numpy
python3 -m pip install -e .

# 生成单声道、中英双语、48 句、带描述争抢的本地样本
python3 -m lcft generate artifacts/lcft-demo \
  --seed 20261122 --sentences 48 --languages zh,en \
  --channels 1 --duration 90 --description-density 180 --contention 400

# 校验时基并输出每句样本区间/帧区间
python3 -m lcft validate artifacts/lcft-demo/bundle.json

# 4 路并行预计算，批量排版并原子发布
python3 -m lcft run artifacts/lcft-demo/bundle.json artifacts/lcft-delivery \
  --workers 4

# 启动本地工作台
python3 -m lcft workbench artifacts/lcft-demo/bundle.json artifacts/lcft-workbench \
  --port 8765
```

`run` 会生成：

- `tracks/<language>.json` 与兼容的 `sentences-<language>.json`：逐句结论。
- `trace-log.json`：带刷新序号的调度、预算、并行提交与发布轨迹。
- `summary.json`：发布哈希、基线比对、预算账本和错误摘要。
- `snapshots/<language>/snapshot-vNNNNNN.json`：原子发布快照。

## 输入 Bundle

```json
{
  "audio": {
    "path": "audio.wav",
    "sample_rate": 48000,
    "channels": 6,
    "total_samples": 4320000
  },
  "frame_table": {
    "path": "frames.bin",
    "frame_rate": 30,
    "frame_count": 2700
  },
  "timebase": {
    "unit": "sample",
    "sample_rate": 48000,
    "ticks_per_second": 48000
  },
  "tracks": {
    "zh": {
      "sentences": [
        {
          "id": "zh-s0000",
          "text": "本地化 排版 快照",
          "start_timecode": "00:00:00.000",
          "end_timecode": "00:00:03.200",
          "speaker": "SPK_A",
          "max_read_speed": 12,
          "priority": 3
        }
      ]
    }
  },
  "descriptions": [
    {
      "id": "desc-d0000",
      "text": "画面切换",
      "start": 144000,
      "end": 192000,
      "priority": 5
    }
  ]
}
```

时间字段支持：

- 整数样本/ticks；
- `{"value": 1500, "unit": "ms"}`；
- `"00:00:01.500"`、`"00:00:01:15"`（30 fps 时等于 1.5 秒）；
- 同一句的 `start_timecode`/`end_timecode` 与整数时间字段同时出现时，两者必须一致。

WAV 必须为 48 kHz、16-bit PCM、单声道或 5.1。帧表格式：

- v1：`<4sHIIHH>` 头，后接 `frame_count` 个 little-endian `uint64` 样本索引。
- v2：`<4sHIIII>` 头，字段为 magic、version、sample_rate、fps_num、
  fps_den、frame_count，后接同样的 `uint64` 样本索引。

## 排版规则

所有语言轨与描述轨共用同一套整数规则：

- `R1_READ_SPEED`：显示时长不短于 `ceil(可读字符数 / 阅读速度上限)`。
- `R2_MIN_DISPLAY`：显示时长不短于 `min_display_seconds`。
- `R3_MIN_GAP_SHIFT`：前句结束与后句开始之间至少保留 `min_gap_seconds`，
  必要时顺延后句。
- `R4_MEDIA_BOUND`：素材末尾空间不足且仍满足可读下限时压缩。
- `D1_DESCRIPTION_FIT`：高优先级描述回收低优先级语音句的多余时长。
- `D2_PRIORITY_LOSER`：低优先级描述被拒绝。
- `D3_NATURAL_GAP`：描述自然放入句间空隙。
- `D4_PRIORITY_PLACEMENT`：描述通过优先级裁决获得位置。

逐句结论至少包含：

```json
{
  "id": "zh-s0000",
  "start_ticks": 0,
  "end_ticks": 153600,
  "start_timecode": "00:00:00.000",
  "end_timecode": "00:00:03.200",
  "frame_start": 0,
  "frame_end": 96,
  "ideal_ticks": 120000,
  "min_ticks": 120000,
  "verdict": "stretched",
  "effects": ["stretched"],
  "rules": ["R1_READ_SPEED"],
  "reasons": [
    {
      "rule": "R1_READ_SPEED",
      "original_ticks": 96000,
      "awarded_ticks": 153600,
      "ideal_ticks": 120000
    }
  ],
  "content_hash": "..."
}
```

`verdict` 为 `unchanged`、`stretched`、`compressed`、`deferred`、`placed`
或 `rejected`。描述冲突无法由优先级裁决时抛出 `E_DESC_CONFLICT`，其
`details.participants` 列出全部争抢句，完整版本不会发布。

稳定错误码：

- `E_TIMEBASE_INVALID`：时基、时间码、样本或帧表冲突；
- `E_DESC_CONFLICT`：描述争抢无法裁决；
- `E_TIMELINE_CONFLICT`：媒体末尾无法同时满足可读下限；
- `E_BUSY`：同一轨已有活动任务；
- `E_BUDGET_EXCEEDED`：全部降级阶段后仍超出配额；
- `E_RECOVERY_INVALID`：快照缺字段、版本/哈希链断链；
- `E_CANCELLED`：协作式取消，未发布部分版本。

## 并行与预算

`precompute_track` 并行计算文本时长和可读下限，但结果按源句序收集与提交。
`workers=1/2/8` 产生逐字节一致的预计算结果和最终轨道哈希。

同一 `track_id` 已有活动任务时，再次启动返回 `E_BUSY`。

预算按句数、文本缓冲和预计算中间表计量。降级序列固定为：

1. `full`
2. `compact_text`
3. `sparse_precompute`
4. `minimal`

降级只改变保留表示，不改变估算器、排序或排版规则，因此同一输入的轨道哈希
不因内存预算变化。四级均超限时返回 `E_BUDGET_EXCEEDED`，发布文件与内存中
当前版本均保持不变。累计占用超过全局配额的任务进入等待队列，账本记录
`queued` / `acquired` / `released`。

## 增量重排

```python
from lcft import LayoutParams, incremental_layout, load_bundle

loaded = load_bundle("artifacts/lcft-demo/bundle.json")
report, stats = incremental_layout(
    previous_report,
    edited_sentences,
    loaded.normalized.descriptions,
    loaded.normalized,
    LayoutParams.from_dict({"min_gap_seconds": "0.12"}),
    published_version=2,
    param_version=2,
)
```

从变更句出发，沿句间实际争抢/顺延关系传播受影响集合；未受影响句直接复用
缓存结论及 `content_hash`。描述长度或参数变化会恢复旧的描述裁剪并重跑描述
裁决，避免复用被污染的语音时长。无法证明安全时回退全量，并返回
`conservative=true` 与原因。

新版本通过 `VersionStore.publish` 一次整体替换。并发预览只会得到完整旧版本
或完整新版本，不会看到跨版本拼接。

## 快照与恢复

```python
from lcft import SnapshotStore

store = SnapshotStore("delivery/snapshots/zh", "speech-zh")
store.save_published(report)
state = store.load_latest()
```

写入使用同目录临时文件、`fsync` 和 `os.replace`。快照包含：

- 已发布逐句结论、描述句、说话人标记、争抢记录；
- 参数版本、`params_hash`、`track_hash`；
- 按句序建立的 SHA-256 哈希链；
- 可选 checkpoint 的已完成预计算行与 `pending_sentence_ids`。

恢复时校验 schema、版本、参数哈希、逐项内容哈希、链长度与 `prev/hash`。
断链或字段缺失返回 `E_RECOVERY_INVALID`，字段通常为
`snapshot.chain[N].hash`，并附带首个断链句 ID。

```bash
python3 -m lcft recover bundle.json delivery/snapshots/zh zh resumed \
  --workers 4
```

checkpoint 中已完成的预计算行会复用，只重新计算未完成句。进程被
`SIGKILL` 强制终止时，操作系统不会允许进程再写退出报告；恢复结论仅依据磁盘
上最近的有效快照/checkpoint。

## 确定性样本与工作台

生成器支持：

- 任意句数、语言集合、单声道/5.1；
- `--read-speed-min` / `--read-speed-max`；
- 描述密度与争抢强度；
- 固定种子；音频、帧表、bundle、基线和 manifest 均可逐字节复现。

若随机描述确实形成不可由优先级裁决的冲突，生成器会按稳定顺序移除冲突描述，
并在 `manifest.json` 中记录 `requested_description_count` 与
`removed_conflict_description_ids`；不会伪造一个成功基线。

工作台提供：

- 波形峰值、语音句、描述句、争抢区间和说话人标记；
- 参数/句子/描述编辑；
- 启动、后台执行与协作式取消；
- 每语言预算占用、刷新序号、受影响/复用句数、降级阶段、时间码与内容哈希。

```bash
python3 -m lcft workbench artifacts/lcft-demo/bundle.json artifacts/lcft-workbench
```

## 公共 Python API

```python
from lcft import (
    BatchOptions,
    LayoutParams,
    SnapshotStore,
    TaskRegistry,
    VersionStore,
    generate_sample,
    incremental_layout,
    layout_track,
    load_bundle,
    precompute_track,
    resume_from_snapshot,
    run_batch,
    unify_timebase,
)
```

## 验证

```bash
python3 -m pytest -q
python3 scripts/validate_demo.py
```

测试覆盖：

- 单声道/5.1 WAV、时间码边界、帧表截断和字段定位；
- 阅读速度延展、顺延、描述裁剪、低优先级拒绝、同优先级冲突；
- 任意并行度预计算一致性、同轨忙锁、预算降级和拒绝后旧版本不变；
- 增量复用与全量重排的轨道哈希一致性；
- 版本预览原子性、快照哈希链、checkpoint 恢复和 `SIGKILL` 恢复；
- 生成器逐字节确定性、工作台轨迹逐字节确定性。

最近一次完整执行结果：`29 passed`。

本地验收脚本的最近结果（`artifacts/lcft-validation/report.json`，`all_ok=true`）：

- 多轨基线：`en=a0676d9d...`、`zh=96d903f4...`，`baseline_match` 两轨均为 `true`；
- 并行一致性：`workers=1` 与 `workers=4` 的轨道哈希和调度轨迹逐字节一致；
- 预算降级：全局/单任务配额 `7104` 字节时两级均进入 `minimal`，轨道哈希不变；
- 增量：编辑末句后受影响 `7` 句、复用 `30` 句，与全量重排轨道哈希一致；
- 断链：返回 `E_RECOVERY_INVALID`，定位 `snapshot.chain[2].hash` 的 `en-s0002`；
- 强杀恢复：只复用 `9` 条已完成预计算记录，重算 `28` 条，未生成退出报告；
- 5.1 音频：6 声道、48 kHz、16-bit；
- 工作台：相同操作序列的轨迹逐字节一致，刷新序号共 `152`。



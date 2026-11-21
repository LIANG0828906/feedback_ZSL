# 本地字幕排版与无障碍描述工作台

本项目实现一条完全本地的字幕本地化交付流水线：读取 48 kHz/16-bit PCM WAV、二进制帧时刻表和 JSON 字幕源，在统一整数时基上完成排版、无障碍描述插入、说话人标记、阅读速度校正、增量重算、预览刷新、原子提交与强杀恢复。核心计算不访问网络、不依赖系统时间或随机状态。

## 快速运行

项目已经按任务要求在本地创建 Python 3.11 虚拟环境并安装 `PySide6`、`NumPy` 和 `pytest`。

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install --upgrade pip
.venv/bin/python -m pip install PySide6 numpy pytest
.venv/bin/python -m pip install -e .
```

生成并运行完整确定性演示：

```bash
.venv/bin/python -m subtitle_localizer demo --out artifacts/demo --seed 7
.venv/bin/python -m subtitle_localizer inspect --case artifacts/demo/case_01_s06_c10_d25_mono
.venv/bin/python -m subtitle_localizer compare --case artifacts/demo/case_01_s06_c10_d25_mono
```

运行测试和桌面窗口冒烟：

```bash
.venv/bin/python -m pytest -q
.venv/bin/python scripts/desktop_smoke.py
```

打开交互工作台：

```bash
.venv/bin/python -m subtitle_localizer gui --case artifacts/demo/case_01_s06_c10_d25_mono
```

Windows 桌面会话中把上面的 `.venv/bin/python` 换成 `.venv\Scripts\python.exe` 即可。`scripts/desktop_smoke.py` 会自动创建窗口、处理事件并在项目目录内写入临时标记文件；无显示服务时会自动使用 Qt `offscreen` 平台。

## 目录

```text
src/subtitle_localizer/
  timebase.py       WAV/FTBL 读取、时基换算、边界校验
  models.py         输入模型与参数模型
  layout.py         离线排版、描述裁决、哈希、增量重算、在线/离线比较
  persistence.py    状态校验、哈希链、原子写入
  lock.py           单素材单会话锁与死进程锁回收
  session.py        日志、检查点、取消、不可变快照发布、预览
  generator.py      确定性 WAV/FTBL/字幕样本生成器
  demo.py           4 个样本、4 个版本、恢复与异常码演示
  ui.py             PySide6 时间轴工作台
  cli.py            命令行入口
tests/              单元、集成、恢复、确定性与 Qt 测试
artifacts/demo/     当前实际运行产物
```

## 输入格式

### WAV

- 仅接受整数 PCM。
- 采样率必须为 `48000`。
- 位深必须为 `16`。
- 声道数必须为 `1`（mono）或 `6`（5.1）。
- 支持普通 PCM 与 `WAVE_FORMAT_EXTENSIBLE` 中的 PCM 子格式。

### 帧时刻表 `frames.ftbl`

固定 28 字节头，后接 `frame_count` 个小端 `uint64` 样本位置：

```text
offset  size  field
0       8     ASCII "FTBL0001"
8       4     version = 1
12      4     nominal frame rate, sample files use 24
16      4     timebase numerator, sample files use 1
20      4     frame_count
24      4     flags, currently 0
28      8*N   frame start sample positions, strictly increasing
```

帧区间使用包含端点的 `[frame_start, frame_end]`，两端均为“样本所在帧”：

```python
frame = bisect_right(frame_positions, sample) - 1
```

### 字幕 JSON

```json
{
  "schema": "subtitle-asset/v1",
  "asset_id": "asset-001",
  "parameters": {
    "default_cps": "10",
    "min_duration_ms": 800,
    "min_gap_ms": 200,
    "sentence_priority": 50,
    "default_channel": 0,
    "channel_map": {"SPEAKER_A": 1}
  },
  "sentences": [
    {
      "id": "S0001",
      "text": "字幕文本",
      "start": "00:00:01.000000",
      "end": "00:00:02.000000",
      "speaker": "SPEAKER_A",
      "cps": "15"
    }
  ],
  "descriptions": [
    {
      "id": "D0001",
      "after_sentence_id": "S0001",
      "text": "无障碍描述",
      "speaker": "NARRATOR",
      "max_cps": "10",
      "min_duration_ms": 400,
      "priority": 70
    }
  ]
}
```

`cps` 可省略，省略时使用 `default_cps`。CPS 使用十进制定点字符串参与哈希，避免平台浮点差异。

## 统一整数时基

公共时基为 `6_000_000` tick/秒：

- 1 微秒 = 6 tick。
- 1 个 48 kHz 音频样本 = 125 tick。
- 字幕时间码、样本索引、帧起点均可无余数换算到该整数时基。

`timebase.py` 会把每句换算成 `orig_start_sample`、`orig_end_sample`、`frame_start`、`frame_end`。以下情况返回 `E_TIMEBASE_INVALID`，并在 `context` 中带 `sentence_id` 与 `field`：

- WAV 不是 48 kHz、16-bit、mono/5.1。
- 时间码格式错误或超出音频长度。
- 句子结束不晚于开始。
- 帧表不单调、长度与计数不符、越过音频末尾。
- 声道映射越界。

## 排版规则

句子按原始开始样本和 ID 稳定排序。对每句有：

```text
desired = ceil(字符数 * 48000 / cps)
earliest = max(原始开始, 前一项结束 + min_gap)
duration = max(desired, min_duration, 1)
```

如果 `earliest + duration` 越过素材末尾，会压缩到剩余区域并记录 `media_end_clip`；完全无法容纳时返回 `E_TIMEBASE_INVALID`。每句报告包含：

- 原始/最终样本边界与时间码。
- 首尾帧。
- `desired_duration_samples`。
- `verdict.compressed`、`verdict.extended`、`verdict.deferred`、`verdict.reasons`、`verdict.status`。
- `entry_hash`：对句子条目除自身哈希外的规范 JSON 求 SHA-256。

描述轨按锚点句后顺序处理：

1. 优先占用“锚点结束 + 最小间隔”到“下一句原始开始 - 最小间隔”的间隙。
2. 空间不足时按 `description.priority` 与 `sentence_priority` 裁决。
3. 更高优先级描述可顺延下一句，并写入 `cut_sentence_ids`、`sentence_trims` 与 `cut_actions`。
4. 有可分配空间但不足以满足描述最短长度时，可压缩为 `inserted_compressed`。
5. 完全无空间且低优先级时记为 `dropped`。
6. 同级优先级发生部分重叠且无法裁决时返回 `E_DESC_CONFLICT`。

说话人标记在首句和相邻句说话人变化处生成。报告整体 `commit_hash` 对除自身外的完整规范 JSON 求 SHA-256，包含父版本哈希、参数、句子、描述、标记和判定。

## 增量重算与发布

提交时先比较已提交输入与暂存输入，计算初始受影响集合：

- 修改句子文本、起止、说话人或 CPS：该句及必要的后继。
- 修改默认 CPS：所有继承默认值的句子。
- 修改最短时长：实际最短时长约束发生变化的句子。
- 修改最小间隔：原本被顺延或间隔小于新旧阈值的相邻句。
- 修改说话人/声道映射：有效声道发生变化的句子。
- 修改描述轨：锚点句及其下一句。
- 全局句优先级变化：全部句子，因为描述裁决依赖该值。

随后做顺序依赖闭包：如果前一项的最终边界变化会改变下一项在离线整段排版中的最早位置，下一项加入受影响集合。未受影响条目直接从旧报告复制，`entry_hash` 和边界逐值不变；报告显式列出 `affected_sentence_ids`、`recomputed_sentence_ids`、`recovered_sentence_ids` 与 `reused_sentence_ids`。

计算期间旧报告对象保持不可变。新报告先写磁盘，再在短临界区内整体替换快照引用。预览线程在任意时刻只能看见完整旧版本或完整新版本，不会读到暂存参数或半成品条目。

## 预览与离线一致

`LayoutSession.refresh(position, window_samples=240000)`：

- 只读取当前已提交报告。
- 按 `[position, position + window)` 与句子边界相交筛选。
- 输出刷新序号、修订号、提交哈希、窗口时间码、句子边界和逐句哈希。
- 每次刷新写一条确定性 JSONL 轨迹。

`compare_with_offline()` 从当前已提交输入重新做一次完整离线排版，再按同步标记逐句比较：

- 句子 ID 集合。
- 起止样本。
- 首尾帧。
- `entry_hash`。

任一不同返回 `E_LAYOUT_MISMATCH`，上下文列出所有差异句子。

## 原子持久化与恢复

状态文件 `layout_state.json` 包含：

- 已提交输入与参数。
- 完整排版报告。
- 修订历史与父哈希链。
- `history_hash`。
- 覆盖整个状态文件的 `state_hash`。
- 待处理日志与 `pending_hash`。
- 已完成句检查点及未提交的计算条目。

保存使用同目录临时文件、`fsync`、`os.replace` 和目录 `fsync`。读取时校验：

- 报告内容哈希。
- 每个句子/描述/说话人标记的内容哈希。
- 修订号连续性与父哈希链。
- `history_hash`、`pending_hash`。
- `state_hash`。
- 必填字段与输入模型。

任一断裂返回 `E_RECOVERY_INVALID`。强杀后，状态文件中仍只有最后已提交报告；检查点条目可从待处理日志恢复，因此续算只处理未完成句。异常或协作式取消会回滚待处理日志，保留最后提交版本。强杀路径不生成退出报告。

同一素材使用 `layout_state.json.lock` 的 `O_EXCL` 会话锁。活跃持有者存在时重复打开返回 `E_BUSY`；持有进程已死亡时锁会被安全回收。

## 业务码

| 业务码 | 含义 |
| --- | --- |
| `E_TIMEBASE_INVALID` | 时基、时间码、帧表、声道映射或媒体边界不合法 |
| `E_DESC_CONFLICT` | 同级描述冲突无法确定裁决 |
| `E_LAYOUT_MISMATCH` | 在线已提交结果与离线整段结果逐句不一致 |
| `E_RECOVERY_INVALID` | JSON 损坏、字段缺失或哈希链断裂 |
| `E_BUSY` | 同一素材已有活跃会话或提交 |
| `E_CANCELLED` | 在句区间检查点协作式取消 |
| `E_INVALID_INPUT` | 其他输入/API 使用错误 |

## 确定性样本与轨迹

`generator.py` 使用自实现 xorshift64* 伪随机数和整数波形，避免 Python 版本、浮点库或系统时间影响样本字节。默认生成：

- 6 句、默认 CPS 10、25% 描述密度、mono。
- 12 句、默认 CPS 14、50% 描述密度、5.1。
- 20 句、默认 CPS 12、无描述、5.1。
- 8 句、默认 CPS 10、25% 描述密度、mono，用于强杀恢复。

每个目录包含 `subtitles.json`、`program.wav`、`frames.ftbl`、`baseline.json`、`final_layout.json` 和 `layout_state.json`。全局 `trace.jsonl` 记录：

- `journal`：待处理句。
- `checkpoint`：句区间检查点。
- `commit`：受影响/重算/复用数量和提交哈希。
- `refresh`：刷新序号、编辑位置、窗口、句子 ID、时间码和哈希。
- `recovery`：恢复的已完成/未完成句。
- `offline_equal` / `mismatch`：在线与离线对齐结果。

## 实际验证结果

本环境为 Ubuntu 26.04，使用项目内 `.venv`（Python 3.11）。

```text
.venv/bin/python -m pytest -q
26 passed

.venv/bin/python scripts/desktop_smoke.py
desktop smoke ok
exit 0

同一 seed=7 的演示连续运行两次：
27 个输出文件逐字节一致

demo report:
refresh_count = 27
trace_rows = 181
trace_sha256 = 3dc3d9c76e95180f887eb8f27f02cc769294f9e19b7f0971a8b78034125c1162
```

最终提交哈希：

```text
case_01 mono  8cfca1346097768dfa02066e  -> 433f5ef8e7c0722c979132d75fcde64af6dd7d8fc1298398a65d723f3c363262
case_02 5.1   0985dbb7271b1d0b81bf4728  -> 63c39a29bf23132fffc94a30c85dc1aef3af86d2df40ccc8568a1f24ea24eb73
case_03 5.1   763b9df508229ffda1d07cae  -> 563318398656ad7090b6644435379e519aa9505e6e877d558d4526d12395f408
case_04 mono  3c4eb2a4a4d9f918578cbfb6  -> fadb42296458e45c0fee26782cf52cfedaa98324de056d39d06bd0809c824d5c
```

恢复用例中，子进程在写完第 2 个句检查点后以退出码 70 强制退出；父进程只依据磁盘状态恢复：

```text
before revision=2  hash=18a019b4470e76bdbfa40247439176234d497def3b238a8eb5d5855524dee52a
unfinished           S0004..S0008
recomputed           S0004..S0008
reused/recovered     S0001..S0003
after revision=3   hash=fadb42296458e45c0fee26782cf52cfedaa98324de056d39d06bd0809c824d5c
exit report generated = false
offline per-sentence equal = true
```

异常码检查均返回预期业务码：

```text
E_BUSY, E_DESC_CONFLICT, E_LAYOUT_MISMATCH, E_RECOVERY_INVALID, E_TIMEBASE_INVALID
```

并发运行 `inspect` 与 `compare` 时，第二个进程实际返回了 `E_BUSY`，证明单素材会话锁生效。当前容器没有 Windows 图形会话，因此未声称在 Windows 上实机验证；代码与冒烟脚本使用跨平台 Qt API，Windows 运行命令见“快速运行”。

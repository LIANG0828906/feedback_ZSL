# StreamForge

面向本地电竞赛事直播转码实验的确定性多档切片服务。项目只读取工作区内生成的 16-bit PCM WAV、JSON 视频帧描述和带宽时变配置，不调用网络、Docker、FFmpeg、GStreamer 或真实编解码服务。

## 功能

- 16-bit PCM WAV（单/双声道，16–48 kHz）与 JSON 帧描述的时间轴校验。
- 固定时长共享切片边界，音频采样位置和视频帧 PTS 严格连续。
- 多档确定性音频重采样、增益、合成视频表示帧和层级内容哈希链。
- 档位缓存：配置变化时只失效并重算指纹变化的档位。
- 批屏障并行调度：全局并行度、稳定提交序、单切片/总内存预算、缓冲降级、超限拒绝和饥饿告警。
- 原子切片写入和失败隔离：失败档位只保留从 0 开始的连续前缀。
- 自适应播放模拟：只在切片边界切换并校验音视频起点与解码缓冲。
- 原子快照、版本/哈希链校验、配置变化感知和断点恢复。
- PySide6 桌面界面：档位进度、内存曲线、实时调度等待/饥饿事件、播放切换轨迹，以及启动、暂停、协作式取消和快照恢复。

## 环境

推荐 Python 3.11。当前验证环境为 Python 3.11.18、NumPy 2.4.6、PySide6 6.12.0、pytest 9.2.0。

Windows PowerShell:

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
python -m pip install -e .
python -m streamforge gui
```

Linux/macOS:

```bash
python3.11 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements.txt
python -m pip install -e .
python -m streamforge gui
```

`requirements.txt` 锁定直接依赖；`python -m pip install -e .` 安装本地包和 `streamforge` 命令。

## 快速演示

```bash
python -m streamforge generate --samples samples --seed 20261121
python -m streamforge demo --samples samples --output artifacts/demo --seed 20261121
python -m streamforge simulate \
  --config samples/track_config.json \
  --budgets samples/budgets.json \
  --output artifacts/demo \
  --bandwidth samples/bandwidth.json \
  --playback samples/playback.json
```

`demo` 生成样本、执行三档切片、运行播放模拟，并输出：

- `artifacts/demo/<tier>/slice-NNNNN.fslice`：切片文件。
- `artifacts/demo/scheduler_trace.jsonl`：调度、内存与切片哈希轨迹。
- `artifacts/demo/playback_trace.jsonl`：播放切换与拒绝轨迹。
- `artifacts/demo/demo_trace.jsonl`：调度与播放合并轨迹。
- `artifacts/demo/snapshot.json`：原子状态快照。
- `artifacts/demo/demo_summary.json`：摘要与各档哈希。

从快照恢复：

```bash
python -m streamforge run \
  --config samples/track_config.json \
  --budgets samples/budgets.json \
  --output artifacts/demo \
  --restore artifacts/demo/snapshot.json
```

失败注入（在提交前抛出，不修改切片模块源码）：

```bash
python -m streamforge run \
  --config samples/track_config.json \
  --budgets samples/budgets.json \
  --output artifacts/failure \
  --inject-failure low:1
```

## 样本格式

`samples/track_config.json`：

```json
{
  "config_version": "demo-v1-seed20261121",
  "source": {"audio": "audio_stereo_48k.wav", "video": "video_frames.json"},
  "timeline": {"slice_duration_ms": 400, "tolerance_ms": 2},
  "tiers": [
    {
      "id": "high",
      "target_bitrate_kbps": 1800,
      "audio_sample_rate": 48000,
      "gain_db": 0.0,
      "video_qp": 24
    }
  ]
}
```

视频帧支持 `pts_ms`/`duration_ms`、`pts_us`/`duration_us`、`pts_ns`/`duration_ns`，也可使用 `pts`/`duration` 加 `pts_unit`/`duration_unit`。每帧必须有唯一 `display_index`。第一帧从 0 开始，帧区间必须无空洞无重叠，且每个切片边界必须与某帧 PTS 重合。

`samples/budgets.json` 定义 `global_parallelism`、`total_memory_bytes`、`single_slice_memory_bytes`、`initial_block_bytes`、`min_block_bytes`、`decode_buffer_bytes`、缓冲水位和饥饿阈值。

## 核心设计

时间轴统一使用纳秒整数，切片边界同时记录源采样位置。每档重采样后的采样位置从共享边界计算，避免累计浮点漂移。音频使用整数线性插值；视频表示帧由 SHA-256 确定性展开，不依赖真实编码器。

切片文件格式为 `FSLICE01 + 4-byte header length + canonical JSON header + payload`。头部包含档位、序号、时间范围、音视频起点、帧描述、预估内存、前序哈希和 `content_hash`。写入先进入同目录临时文件，`fsync` 后原子替换。

调度器采用批屏障：每轮将各档当前待运行切片按提交序号稳定排序，在并行度和总内存允许时并行执行；本轮全部结束后再按提交序号处理结果。因此调度顺序不依赖线程实际完成时刻，同时同档位最多只有一个任务。无法准入的任务进入等待队列；高码率档等待达到阈值时写入 `starvation_warning`，包含逻辑等待毫秒和让出次数。单切片预估超出预算时先缩小内部块；缩小到下限仍超出则返回 `E_BUDGET_EXCEEDED`，该档不再越过失败点提交。

快照使用临时文件原子替换，包含边界表、档位哈希链、内存峰值/采样、等待队列和配置版本。载入时校验快照自身哈希、切片内容哈希和逐片前序哈希；档位参数或边界表变化会被标记为待重算，断链或字段缺失返回 `E_RECOVERY_INVALID`。

## 业务码

- `E_TIMEBASE_INVALID`：WAV/帧描述/容差/边界冲突；详情包含首个冲突帧、PTS 或采样位置。
- `E_BUDGET_EXCEEDED`：单切片或总内存不允许启动；详情包含档位和切片序号。
- `E_SWITCH_MISALIGN`：切换时间戳不对齐或解码缓冲越界；保留原档位。
- `E_RECOVERY_INVALID`：快照版本、哈希链、文件或字段损坏。
- `E_BUSY`：运行中的管线或档位被重复启动。
- `E_CANCELLED`：协作式取消。
- `E_INJECTED_FAILURE`：入口参数注入的提交前故障。

## 验证

```bash
python -m pytest -q
python scripts/acceptance.py
```

测试覆盖时间轴违规、三档边界与采样连续性、内存约束和降级、饥饿告警、预算拒绝、失败前缀隔离、缓存复用、切换对齐、解码缓冲越界、快照恢复/篡改、CLI 字节确定性和 Qt offscreen 启动。`scripts/acceptance.py` 会生成 `artifacts/acceptance_report.json` 及成功、失败、恢复轨迹。

实际验证结果见 `artifacts/acceptance_report.json`。在无桌面显示的环境中，Qt 窗口通过 `QT_QPA_PLATFORM=offscreen` 启动验证；Windows 桌面可直接运行 `python -m streamforge gui`。

本次实际结果：

- `pytest`：31 项通过。
- 三档各 8 个切片，时间轴和共享边界检查全部通过。
- 调度内存峰值 1,919,684 B，低于配置上限 3,145,728 B。
- 播放模拟 8 次切换，全部落在切片边界，错误列表为空。
- `low:1` 注入失败后仅保留 `slice-00000.fslice`，`mid`/`high` 各完成 8 片，无临时文件残留。
- 相同种子和操作序列下，切片文件、调度轨迹和快照逐字节一致。
- 快照恢复复用 `low`/`mid`/`high` 全部切片，重算列表为空，恢复前后快照逐字节一致。

# Live Transcode Slice Lab

本地确定性多档转码切片与自适应播放模拟器。项目同时提供脚本化入口和 PySide6 图形界面，不使用 Docker、真实编解码器、外部服务或账号。

核心实现覆盖：

- 16-bit PCM WAV + JSON 帧描述读取，统一整数时基与绝对切片边界。
- 多档音频重采样、增益、确定性视频帧表示。
- 稳定提交序调度、全局并行度/内存预算、逐级降级和单档失败隔离。
- 原子切片发布、JSON 哈希链索引、原子快照和硬终止恢复。
- 仅允许切片边界的 ABR 切换、音视频时间戳校验、解码缓冲水位/字节上限校验。
- 相同种子和操作序列下，切片、切换轨迹、调度日志逐字节一致。

## 环境

- Windows 10/11（PowerShell）或可运行 Python 的等价本地环境。
- Python 3.11/3.12。
- NumPy；GUI 另需 PySide6。按官方 wheel 安装即可，无需 GPU。

PowerShell:

```powershell
py -3.12 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
```

bash:

```bash
python3.12 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
```

如果只做脚本验证，可不安装 PySide6；安装 `numpy` 后使用 `generate/run/resume/simulate/verify` 即可。

## 快速验收

```bash
# 1. 生成 4 组本地样本与输入基线哈希
python -m livetranscode generate --out samples --seed 1234

# 2. 多档切片 + 自适应切换模拟
python -m livetranscode run \
  --workspace runs/standard \
  --sample samples/esports_48k_stereo \
  --clean

# 3. 校验边界、WAV 样本数、音视频时间戳、索引哈希链
python -m livetranscode verify --workspace runs/standard

# 4. 预算降级与拒绝隔离（该命令故意有失败档位，退出码 7）
python -m livetranscode run \
  --workspace runs/budget \
  --sample samples/budget_degrade \
  --clean

# 5. 强制终止：4 次原子提交后直接硬退出，不写 exit_report.json
python -m livetranscode run \
  --workspace runs/crash \
  --sample samples/esports_48k_stereo \
  --clean \
  --crash-after-commits 4

# 6. 仅依据磁盘索引/切片和最近快照恢复
python -m livetranscode resume --workspace runs/crash

# 7. GUI
python -m livetranscode ui \
  --workspace runs/ui \
  --sample samples/esports_48k_stereo
```

无桌面时可使用全部脚本命令；GUI 可用 `QT_QPA_PLATFORM=offscreen` 做构造烟雾测试。

## 输入格式

### WAV

- RIFF/WAVE PCM，`sampwidth=2`，无压缩。
- 单声道或双声道。
- 采样率 16000–48000 Hz。
- 非 16-bit、非法声道数、非法采样率、压缩 WAV 均返回 `E_TIMEBASE_INVALID`。

### 视频帧 JSON

推荐使用整数 PTS 和显式 `timescale`：

```json
{
  "timescale": 90000,
  "duration": 540000,
  "frames": [
    {"index": 0, "pts": 0, "duration": 3000},
    {"index": 1, "pts": 3000, "duration": 3000}
  ]
}
```

- `index` 为显示序号，必须严格递增。
- `pts` 必须从 0 开始、单调递增、与前一帧结束时刻连续。
- 帧时长可省略，此时由下一帧 PTS 或根级 `duration` 推导。
- 也支持 `pts_seconds` / `duration_seconds`；换算到声明 timescale 时必须无小数余数。
- 视频描述必须覆盖完整音频时间轴，否则返回 `E_TIMEBASE_INVALID`。

### 配置 JSON

```json
{
  "schema_version": 1,
  "slice_duration_ms": 500,
  "global_parallelism": 2,
  "memory_budget_bytes": 1500000,
  "slice_memory_budget_bytes": 1400000,
  "decode_buffer_capacity_ms": 5000,
  "target_buffer_ms": 1600,
  "low_buffer_ms": 500,
  "max_decode_buffer_bytes": 4000000,
  "tracks": [
    {
      "id": "high",
      "target_bitrate_kbps": 2400,
      "audio": {"resample_rate": 48000, "gain_db": 0.0},
      "video": {"quantization": 6},
      "memory_limit_bytes": 900000,
      "degrade_stages": [
        {"name": "full", "block_size": 65536, "temp_factor": 1.0},
        {"name": "shrink_block", "block_size": 16384, "temp_factor": 0.75},
        {"name": "minimal_buffers", "block_size": 4096, "temp_factor": 0.5}
      ]
    }
  ],
  "failure_injection": {}
}
```

约束：

- `global_parallelism`、总内存和单切片内存预算只能来自配置，运行时不会提高。
- `slice_memory_budget_bytes <= memory_budget_bytes`。
- 每档 `memory_limit_bytes <= memory_budget_bytes`。
- `degrade_stages` 的 `block_size` 与 `temp_factor` 必须逐级不增。
- `low_buffer_ms < target_buffer_ms <= decode_buffer_capacity_ms`。
- `failure_injection` 仅用于确定性故障测试，键为 `<track>:<slice>`。

### 带宽序列

```json
{
  "samples": [
    {"boundary_index": 0, "bandwidth_kbps": 4200},
    {"boundary_index": 2, "bandwidth_kbps": 1600}
  ]
}
```

也支持 `time_ticks`、`time_ms`，或直接的数值数组（按切片序号解释）。每个决策只发生在切片边界。

## 输出布局

```text
workspace/
  manifest.json                 # 实现版本、配置版本、源哈希
  config.json                   # 规范化副本
  boundaries.json               # 所有档位共享的绝对边界表
  snapshot.json                 # 原子快照（含边界表、索引、哈希链、水位、等待队列）
  exit_report.json              # 仅正常结束/协作取消时生成
  tracks/<track>/index.json     # 已完成切片索引与链头
  tracks/<track>/slices/000000/
      audio.wav
      video.json
  logs/scheduler_trace.jsonl
  logs/run.log
  switches.json
  switches.jsonl
```

每个索引项记录源/输出起止样本、统一时基起止时刻、音视频起止时间戳、预算阶段、内容哈希和链哈希。`audio.wav` 与 `video.json` 先写入同一 staging 目录，再通过目录原子重命名发布；随后原子写入该档索引。

## 设计说明

### 统一时基

时基频率取以下整数的最小公倍数：

```text
LCM(源采样率, 视频 timescale, 1000, 所有档位重采样率)
```

因此：

- 每个音频样本索引可精确映射为整数 tick。
- 每个视频整数 PTS 可精确映射为整数 tick。
- 每个切片边界同时对齐源样本、各档输出样本和毫秒切片长度。
- 无法整除的 PTS/秒值或边界直接返回 `E_TIMEBASE_INVALID`，不做静默舍入。

### 多档一致边界

所有档位引用同一 `boundaries.json`。每档只改变重采样率、增益、视频量化参数和码率，不改变源时间范围。因此同一切片序号在所有档位中对应完全相同的 `[start_ticks, end_ticks)`。

### 调度顺序

任务按“切片序号优先、档位声明顺序次之”分配全局提交序号。调度器同时满足两个约束：

1. 每个档位最多只有一个准备中任务；同档下一片只有在前一片发布后才可启动/发布，保证同档索引严格有序。
2. 不同档位在同一并行批次内可同时占用 worker。批次完成后，按全局提交序号串行发布，再进入下一批，因此“调度决策顺序 = 发布顺序”且不受线程完成先后影响，轨迹可逐字节重放。

若较早任务因内存等待，不会越过它启动更晚的可运行任务；因同档前序未完成而不可运行的任务会被跳过，允许其他档位利用空闲 worker。

### 内存预算与降级

估算包含：

- 源切片工作块。
- 重采样插值工作块。
- 临时缓冲（受 `temp_factor` 控制）。
- 目标 PCM。
- 视频帧记录。
- 固定开销。

先尝试 `full`，超限后严格按声明顺序缩小块并降低临时缓冲，直至选中第一个可容纳阶段。输出样本数只由边界和重采样率决定，与降级阶段无关。所有阶段都超限时返回 `E_BUDGET_EXCEEDED`；该档保留已提交前缀，后续不再落盘，其他档继续。

### 切换与缓冲

模拟器在每个切片边界：

1. 找出该边界有已提交且时间戳/样本数合法的档位。
2. 按带宽、缓冲水位和码率选择档位。
3. 校验 `start_ticks`、`audio_start_ticks`、`video_start_ticks` 均等于边界时刻。
4. 校验 WAV 帧数、视频帧窗口连续性、解码缓冲字节上限。

缓冲变化的简化公式为：

```text
新增播放时长 = 切片时长 × 带宽 / 档位码率
缓冲变化     = 新增播放时长 - 切片时长
```

同一带宽和缓冲水位下，高码率档位消耗/排空缓冲更快，低码率档位回填更快。下载达到缓冲上限时暂停下载并记录 `download_throttled_at_capacity`。若目标档位在当前水位下字节数超过 `max_decode_buffer_bytes`，尝试保留可容纳的当前档位；仍不合法则返回 `E_SWITCH_INVALID`。缺口边界、时间戳不一致、缓冲越界都会写出 `valid=false` 轨迹并定位档位、边界和冲突字段。

### 原子提交与恢复

- 切片先写同目录 staging，再原子重命名。
- 索引只在音频和视频都发布后更新。
- 每次索引更新后原子刷新快照。
- 恢复时磁盘索引是“已提交”的唯一依据；staging 和未入索引的 final 目录均视为未提交。
- 恢复校验 schema、配置版本、档位参数版本、边界表、索引内容哈希、链哈希和实际文件哈希。
- 快照可落后于磁盘索引（例如索引已写而快照未刷新）；此时使用磁盘索引继续。快照声称存在但磁盘索引没有的切片会被拒绝。
- `--crash-after-commits` 使用 `os._exit(137)` 模拟强制终止；此时不会产生 `exit_report.json`。

## 业务码

| 业务码 | 含义 |
|---|---|
| `E_TIMEBASE_INVALID` | WAV/帧描述非法，或整数时基换算有余数 |
| `E_BUSY` | 引擎运行中重复启动，或同一请求重复档位 |
| `E_BUDGET_EXCEEDED` | 所有降级阶段仍超过单切片内存预算 |
| `E_SWITCH_INVALID` | 切换边界无合法切片、时间戳/样本数不符或缓冲越界 |
| `E_SNAPSHOT_MISMATCH` | 配置、档位参数、边界表、快照或哈希链不一致 |
| `E_CONFIG_INVALID` | 配置结构或取值范围非法 |
| `E_IO` | 本地文件读写失败 |
| `E_CANCELLED` | 在切片边界检查点协作取消 |

CLI 对 `E_BUSY/E_SNAPSHOT_MISMATCH/E_SWITCH_INVALID/E_TIMEBASE_INVALID/E_BUDGET_EXCEEDED/E_CANCELLED` 分别返回退出码 `3/4/5/6/7/130`。

## 测试

```bash
python -m unittest discover -v
```

测试覆盖：

- 16k/44.1k/48k、单/双声道、多档重采样边界连续性。
- 非法采样率、非法声道、非单调/有空洞帧、分数时基余数。
- 预算降级、预算拒绝、失败档位隔离。
- 音视频切换时间戳和缓冲字节校验。
- 缺口区间禁止合法切换结论。
- 并发重复启动 `E_BUSY`。
- 配置/边界表/快照不匹配。
- 4 次提交后硬终止、无退出报告、恢复后与未中断运行逐字节一致。
- 两次相同运行的全部切片、切换轨迹和日志逐字节一致。

实际运行结果见 [VALIDATION.md](VALIDATION.md)。

## 说明与边界

- 视频“帧表示”为确定性的量化元数据负载，用于切片/切换/哈希模拟，不调用 FFmpeg 或真实编码器。
- 重采样为确定性的线性插值 + 声道保持 + dB 增益，输出样本数由整数时基严格约束。
- 强制终止测试使用项目内硬退出钩子；外部任务管理器/SIGKILL 同样只会留下已提交索引和快照。
- GUI 需要桌面会话；无桌面时所有验收能力均可通过 CLI 和库 API 完成。


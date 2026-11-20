# 声迹修复（Voice & Music Repair）

面向语音与音乐修复的本地桌面应用。输入为 16-bit PCM WAV（单声道/双声道，8000–48000 Hz），
在时间轴上划定修复区间，按固定块顺序叠加：

1. 去直流偏移
2. 噪声门
3. 谱减降噪
4. 区间增益包络 + 左右联动轻量限幅

应用提供 PySide6 图形界面、脚本化 CLI、确定性的本地样本生成器，以及会话保存/载入/
强制终止恢复/受影响块增量重算能力。

## 快速开始

Linux / WSL：

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install -U pip
pip install -r requirements-dev.txt
pip install -e .
```

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -U pip
pip install -r requirements-dev.txt
pip install -e .
```

启动桌面界面：

```bash
voice-repair gui
# 或直接打开已有会话
voice-repair gui --session artifacts/demo/demo.repair.json
```

无界面完整演示：

```bash
voice-repair demo --workdir artifacts/demo --seed 20261120
```

该命令会自动生成单声道与双声道样本、创建会话、启动子进程处理并在块提交后强制 kill，
随后恢复、续算、修改区间并只重算受影响块，最后用相同种子和操作序列做逐字节重放比较。
结果写入 `artifacts/demo/demo_report.json`，逐块轨迹写入 `artifacts/demo/demo_trace.jsonl`。

## CLI

```bash
voice-repair generate --out input.wav --rate 48000 --channels 2 --seconds 4 --seed 1234
voice-repair new --input input.wav --session session.json --output repaired.wav \
  --block-size 4096 --interval 0.5:1.5:3:20:20
voice-repair process --session session.json --trace trace.jsonl
voice-repair process --session session.json --max-new-blocks 10
voice-repair recover --session session.json --trace trace.jsonl
voice-repair status --session session.json
voice-repair update --session session.json --set noise_gate.threshold_db=-48
voice-repair update --session session.json --intervals-file intervals.json
```

强制终止脚本可在子进程上使用 `process --post-commit-delay-ms 40`，观察到目标块提交后
直接 `kill()`/`TerminateProcess`，再执行 `recover` 与 `process`。所有命令输出 JSON，
业务失败返回非零退出码及稳定错误码。

区间参数格式为 `开始秒:结束秒[:增益dB[:淡入ms[:淡出ms]]]`。会话内区间不允许重叠；
如需“多处修复”，可并列放置多个区间。

## 会话结构

会话为带版本号和自完整性摘要的 JSON（`schema_version = 1`），主要字段：

```json
{
  "schema_version": 1,
  "session_format": "voice-music-repair",
  "paths": {"input": "input.wav", "output": "repaired.wav", "block_cache": "session.json.blocks"},
  "audio": {
    "sample_rate": 48000,
    "channels": 2,
    "frames": 115200,
    "sample_width": 2,
    "input_file_sha256": "..."
  },
  "block_size": 4096,
  "processing_chain": {
    "dc_removal": {"enabled": true, "per_channel": true},
    "noise_gate": {"enabled": true, "threshold_db": -42.0, "attack_ms": 4.0, "release_ms": 80.0, "attenuation_db": -60.0},
    "spectral_subtraction": {"enabled": true, "n_fft": 512, "hop_length": 128, "noise_percentile": 20.0, "oversubtraction": 1.5, "reduction_db": 12.0, "floor_db": -45.0},
    "limiter": {"enabled": true, "threshold_db": -1.0}
  },
  "intervals": [
    {"id": 1, "start_frame": 24000, "end_frame": 60000, "gain_db": 3.0, "fade_in_ms": 20.0, "fade_out_ms": 20.0}
  ],
  "completed_blocks": {
    "0": {
      "index": 0,
      "start_frame": 0,
      "frames": 4096,
      "input_sha256": "...",
      "config_sha256": "...",
      "output_sha256": "...",
      "output_bytes": 16384,
      "status": "committed"
    }
  },
  "complete": false,
  "output_sha256": null,
  "integrity_sha256": "..."
}
```

摘要全部是 SHA-256 内容哈希。文件大小、修改时间、时间戳均不参与“块是否有效”的判定。

## 原子性与恢复

### 块提交协议

1. 从原始输入读取固定帧数块；不足一块的尾块也按实际帧数形成独立完整块。
2. 在当前进程内完成全部处理并量化为完整 int16 块字节。
3. 写 `session.json.blocks/00000000.pcm` 的同目录临时文件，`fsync` 后
   `os.replace` 原子改名。
4. 写包含输入摘要、配置摘要、输出内容摘要的会话临时文件，`fsync` 后原子改名。
5. 停止/完成/恢复时，遍历已提交块缓存构造临时 WAV，`fsync` 后用整文件
   `os.replace` 发布最终输出。

因此输出 WAV 从不做“半块局部写入”。强制终止只会留下：

- 旧输出 WAV（完整文件）或初始零填充 WAV；
- 若干原子块缓存；
- 最近一次完整有效的会话 JSON；
- 可忽略的 `.tmp` 文件。

恢复时只信任会话声明为 committed 且内容哈希匹配的块。未声明完成的块会重新处理，
不会把残留临时内容当作有效音频。最终 WAV 从完整块缓存原子重建。

若块缓存缺失，但已有输出 WAV 中该块内容与摘要一致，会从输出恢复缓存；两者都不满足时
以 `E_RECOVERY_INVALID` 中止。

### 保存与校验

会话保存使用“同目录临时文件 + `fsync` + 原子重命名”。载入时依次校验：

- `schema_version` 精确匹配；
- 会话自完整性 `integrity_sha256`；
- 输入 WAV 整文件摘要及参数；
- 输出 WAV 头、声道数、采样率、帧数和精确文件长度；
- 每个已完成块的输入/配置/输出内容摘要。

版本或摘要不匹配返回 `E_SESSION_MISMATCH`；会话缺失、输出截断、块缓存缺失等不可恢复状态
返回 `E_RECOVERY_INVALID`。

## 增量重算

每个块的 `config_sha256` 来自该块实际生效的完整配置清单：

- 采样率、声道数、块起始帧与帧数；
- 四类处理参数；
- 与该块相交的区间片段（绝对边界、区间增益、淡入/淡出）。

不落在任何区间内的块使用确定的直通清单。参数或区间变化后，只删除配置摘要发生变化的块摘要；
其他块保留原缓存和摘要。再次处理时直接复用，不会重新调用处理器或重写其块缓存。

## 双声道与确定性

- 去直流按声道分别计算均值并分别扣除。
- 噪声门包络按声道独立维护。
- 谱减在左右声道上使用相同的块长度、FFT 长度、帧移和帧数，输出严格等长。
- 限幅检测时取双声道联合峰值，并按同一系数缩放，避免左右声道电平关系被破坏。
- 样本生成、处理、量化均不读取墙钟时间；相同 Python/NumPy/SciPy 版本、种子与操作序列
  产生逐字节一致的 WAV 与块摘要。

## 并发与资源

每个会话对应一个跨进程锁文件：POSIX 使用 `flock`，Windows 使用 `msvcrt.locking`。
第二个处理任务立即返回 `E_BUSY`，不阻塞。处理、恢复、更新和显式保存均在锁内完成；
取消或异常通过上下文管理器释放锁、文件描述符与输出句柄。

## 界面

- 时间轴按秒缩放，滚轮缩放，Shift+滚轮水平移动，中键拖拽平移。
- 显示双声道波形、区间叠加、所选区间和当前处理块指示线。
- 双击空白添加 0.5 秒区间，拖动区间内部移动，拖动左右边缘调整边界，Delete 删除。
- 参数修改采用防抖自动保存；受影响块立即失效并在状态栏显示数量。
- “开始/继续处理”在 QThread 中执行；“协作式中止”只设置停止事件，当前块完整提交后退出。
- 底部持续显示当前块、总块数、已提交块数和进度条。

## 已验证

`python -m pytest`：24 项全部通过。实际环境与完整结果见 `VERIFICATION.md`。

当前执行环境未暴露 Windows `cmd.exe`/`python.exe` 互操作入口，因此 Windows 原生窗口未在该
会话内目视确认；PySide6 GUI 已通过 `QT_QPA_PLATFORM=offscreen` 完成窗口、会话载入和波形
渲染冒烟。Windows 侧使用同一份跨平台代码启动 `voice-repair gui`。


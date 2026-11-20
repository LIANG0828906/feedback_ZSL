# 实际验证记录

验证日期：2026-11-20（Asia/Shanghai）  
工作区：`/home/liang/feedback/uid190c93df-default-2`  
环境：WSL2 Ubuntu 26.04、Python 3.14.4、NumPy 2.5.5、SciPy 1.18.1、
PySide6 6.12.0、pytest 9.2.0。

## 自动化测试

命令：

```bash
.venv/bin/python -m pytest
```

结果：

```text
........................                                                 [100%]
24 passed in 6.51s
```

覆盖内容：

- 生成器在 8k/16k/44.1k/48k、单/双声道下同种子逐字节一致；
- WAV 头、数据长度和 int16 读取；
- 去直流/噪声门按声道独立，谱减双声道等长，限幅左右联动；
- 参数变化只使相交块失效，未受影响块摘要与缓存原样复用；
- 区间移动同时失效旧区间与新区间的相交块，其余块复用；
- 两个独立会话相同输入/参数输出 WAV 与块摘要逐字节一致；
- 部分处理、恢复、续算与已提交块复用；
- 会话缺失、输出截断返回 `E_RECOVERY_INVALID`；
- 会话完整性/块缓存摘要篡改返回 `E_SESSION_MISMATCH`；
- 同一会话锁被占用时返回 `E_BUSY`；
- 块缓存缺失时可从已物化输出恢复；
- 真实子进程处理中被 `kill()` 后，恢复续算且已完成块摘要不变化；
- CLI 生成/新建/处理/状态/更新链路；
- CLI 完整 demo 链路；
- PySide6 offscreen 窗口创建、会话恢复载入、波形与区间显示。

## 强制终止 demo

命令：

```bash
.venv/bin/python -m repair_app demo --workdir artifacts/demo --seed 20261120
```

实际关键结果：

```json
{
  "status": "demo_completed",
  "forced_crash": {
    "child_progress_events_observed": 4,
    "committed_blocks_at_kill": 4,
    "output_size_ok": true,
    "only_complete_block_regions_written": true,
    "non_zero_uncommitted_blocks": []
  },
  "recovery": {
    "recovered_blocks": 4,
    "remaining_blocks_before_resume": 109,
    "resume_reused_blocks": 4,
    "resume_processed_blocks": 109,
    "resumed_status": "completed"
  },
  "incremental_recompute": {
    "reused_blocks": 39,
    "processed_blocks": 74,
    "status": "completed"
  },
  "determinism": {
    "generator_byte_equal": true,
    "output_byte_equal": true,
    "block_summaries_equal": true,
    "output_sha256": "f0bb88a01bbc06bc348578427b3e6daa1becd65665d41816068a96ab0fbecd0d",
    "replay_output_sha256": "f0bb88a01bbc06bc348578427b3e6daa1becd65665d41816068a96ab0fbecd0d"
  },
  "final": {
    "committed_blocks": 113,
    "total_blocks": 113,
    "complete": true
  }
}
```

完整报告：`artifacts/demo/demo_report.json`  
逐块轨迹：`artifacts/demo/demo_trace.jsonl`  
最终 WAV：`artifacts/demo/demo_repaired.wav`  
重放 WAV：`artifacts/demo/deterministic_replay.wav`

## GUI 冒烟

命令：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python - <<'PY'
from PySide6.QtWidgets import QApplication
from repair_app.gui import MainWindow
app = QApplication([])
window = MainWindow()
window.open_session_path("artifacts/demo/demo.repair.json")
window.show()
app.processEvents()
window.grab().save("artifacts/gui_screenshot.png")
PY
```

结果：窗口尺寸 1360×820，成功载入 48 kHz 双声道波形、两个区间、参数面板和
113/113 已提交块状态。截图见 `artifacts/gui_screenshot.png`。

当前 WSL 环境没有暴露 `cmd.exe`、`powershell.exe` 或 Windows `python.exe`，因此无法在本
会话内启动 Windows 原生窗口。GUI 使用纯 PySide6 Widgets，无 Linux 专属 UI 依赖；
Windows 安装依赖后可直接运行 `voice-repair gui`。

## CLI 安装检查

```bash
.venv/bin/voice-repair --help
```

成功列出 `generate/new/process/recover/status/update/demo/gui` 八个子命令。


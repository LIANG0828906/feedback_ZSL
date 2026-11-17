# 多轨掌声事件检测与立体声确定性母带管线

纯本地、确定性的 Python 工程：由固定种子合成六支观众席话筒的掌声/喝彩/
脚步素材，自动统计独立掌声事件、按频段计算话筒能量配比、分块渲染立体声
母带，并按 ITU-R BS.1770 做响度与真峰值合规检查，支持快照断点续算。

## 环境要求

- Windows 10/11 或 Linux/macOS，Python 3.11（仅用 NumPy / SciPy / SoundFile）
- 无网络、无 GPU、无外部媒体依赖；测试素材全部由代码按种子确定性生成

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate
source .venv/bin/activate
pip install -e .[test]
```

## 一键演示

```bash
# 生成六话筒工程 -> 合成 WAV -> 检测 -> 配比 -> 渲染 -> 合规报告
applause-master demo ./demo_work --duration 90
```

产物：

- `demo_work/project.json`：工程声明（话筒、门限、频段、响度/TP 目标）
- `demo_work/mics/mic_*.wav`：确定性合成的六支话筒 16-bit WAV
- `demo_work/ground_truth.json`：种子可复算的事件真值与每话筒声学参数
- `demo_work/out/master_final.wav`：合规立体声母带（32-bit float）
- `demo_work/out/report.json`：事件清单、频段配比、限幅统计、母带哈希
- `demo_work/out/snapshot.json`：分块渲染快照

## 分步命令

```bash
# 1. 按工程 JSON 合成话筒 WAV 与真值
applause-master generate ./demo_work/project.json
# 2. 渲染母带（Ctrl+C 会完成当前块并写恢复报告，退出码 0）
applause-master render ./demo_work/project.json --output ./demo_work/out
```

## 工程 JSON 关键字段

```json
{
  "seed": 20261118,
  "sample_rate": 48000,
  "duration_seconds": 90.0,
  "microphones": [
    {"name": "mic_1", "file": "mics/mic_1.wav", "channels": 1,
     "position": [-9.0, 6.0, 1.6]}
  ],
  "detection_threshold": 0.012,
  "freq_bands": [
    {"name": "low",  "low_hz": 20.0,  "high_hz": 250.0},
    {"name": "mid",  "low_hz": 250.0, "high_hz": 2000.0},
    {"name": "high", "low_hz": 2000.0, "high_hz": 8000.0},
    {"name": "air",  "low_hz": 8000.0, "high_hz": 20000.0}
  ],
  "target_loudness_lufs": -23.0,
  "true_peak_limit_dbtp": -1.0,
  "energy_gate": 0.05,
  "noise_gain_db": -6.0,
  "block_seconds": 0.5
}
```

校验失败时 CLI 输出含业务码（`FILE_MISSING`、`SAMPLE_RATE_MISMATCH`、
`CHANNEL_COUNT_MISMATCH`、`DUPLICATE_MIC_NAME`、`INVALID_FREQ_BAND`、
`E_RESUME_MISMATCH` 等）的结构化 JSON，以非零退出码结束且不产生部分母带。

## 测试

```bash
pytest
```

26 个用例覆盖：合成逐字节一致性、种子真值复算、事件计数与边界 RMSE
（<= 20 ms）、脚步抗虚警、频段配比求和与顺序无关、能量门限、响度/真峰值
合规、限幅无阶跃、SIGINT 与 SIGKILL 恢复逐字节一致、快照失配拒绝重启、
全部结构化错误拒绝与非零退出码。

## 代码结构

```
src/applause_master/
  project.py   工程加载与结构化校验
  synth.py     确定性多话筒合成与真值复算
  detect.py    稀疏掌声事件检测与跨话筒合并
  bands.py     分频段话筒能量配比
  render.py    分块流式渲染、声像、快照与恢复
  loudness.py  BS.1770 响度、真峰值与平滑限幅
  pipeline.py  端到端编排与稳定报告
  wavio.py     剔除 PEAK 块的确定性 WAV 写入
  cli.py       命令行入口与中断语义
tests/         pytest 用例（含子进程 SIGKILL/SIGINT 恢复）
docs/TECHNICAL.md  算法与验收口径说明
```

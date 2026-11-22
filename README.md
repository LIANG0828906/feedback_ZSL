# 实时语音抖动缓冲工作台 (Jitter Buffer Workbench)

面向多人游戏实时语音的流媒体接收与自适应抖动缓冲桌面工作台，供语音
工程师调试网络抖动下的播放质量。核心保证：**同一语音样本在实时接收、
断点恢复、离线全量重放三条路径下，出队采样序列、目标延迟决策序列与
逐帧能量统计逐位一致**。

## 环境准备

```bash
python3 -m venv .venv          # Python 3.10+（本仓库在 3.14 验证）
.venv/bin/pip install -r requirements.txt   # numpy / PyQt5 / pytest
```

离线环境可用 `.venv/bin/pip install --no-index --find-links <本地包目录> -r requirements.txt`。

## 快速开始

```bash
# 1. 生成确定性样本（固定种子，可重复）
.venv/bin/python -m jitter_workbench.sample_gen samples/voice_seed42

# 2. 运行验收（自动清理旧检查点，产出 exports/ 报告）
.venv/bin/python scripts/run_acceptance.py

# 3. 启动桌面工作台
.venv/bin/python main.py

# 4. 运行测试
.venv/bin/python -m pytest tests/ -q
```

## 目录结构

```
jitter_workbench/
  sample_gen.py   # 确定性样本生成器（种子固定，保留脚本）
  loader.py       # 清单/帧流加载与校验（E_META/E_JITTER/E_DECODE/E_ORDER/E_DUP）
  controller.py   # 自适应目标延迟控制（水位/迟到率/时钟偏差 + 滞回，E_PARA）
  engine.py       # 抖动缓冲核心：排序去重、按目标延迟出队、OLA、gap 静音
  receiver.py     # 离线/多线程接收 ingress（有界队列 + 水位线，背压可解释）
  checkpoint.py   # 原子串行检查点（tmp+rename，哈希/版本门控）
  paths.py        # 三条路径：实时接收 / 断点恢复 / 离线重放
  compare.py      # 一致性比对，首个不一致点定位（E_DIFF + 上下文窗口）
  gui.py          # PyQt5 桌面工作台
samples/          # 生成的样本（manifest.json + frames.bin + 真值）
checkpoints/      # 检查点存储（运行前由验收脚本清空）
exports/          # 验收报告与导出产物
tests/            # 37 项测试
scripts/run_acceptance.py
```

## 样本格式

`manifest.json` 声明 `sample_rate / frame_len / hop / num_frames /
duration_samples`、逐帧 `media_ts`（单调）与接收侧 `arrivals`（含
`arrival_ts`、`dup` 标记）。抖动模式标记集合：`delay / reorder / drop /
duplicate / surge`，未知标记整份样本拒绝加载（E_JITTER）。真值：
`truth_wave.npy`（逐样本原始波形）、`truth_energy.npy`（逐帧能量）。

## 确定性设计（三路径一致性的根因）

- 引擎由**整数毫秒虚拟时钟**驱动；每个 tick 的到达集合完全由
  `(arrival_ts, seq, dup)` 排序决定，与线程调度无关。
- 控制器只消费**已观测**的到达信息（水位、滑动窗口迟到率、播放时钟
  偏差、到达延迟 std 突变），不读取未到达帧。
- 目标延迟调整通过平移播放原点实现，**已提交窗口的输出永不改变**。
- 检查点在每次窗口提交后原子落盘（`state.json.tmp` → `os.replace`），
  恢复时校验样本内容哈希与策略参数版本，不匹配则整体作废从头重放；
  恢复点 tick 的控制步已执行标记保证与连续运行逐 tick 对齐。
- 多线程接收通过水位线（watermark）告知引擎“tick t 的到达集合已完整”，
  队列满默认阻塞（背压），因此并行与串行产出相同序列。

## 错误码

| 代码 | 含义 |
|------|------|
| E_META | 字段缺失 / 采样率不一致 / 媒体时间戳非单调 / 帧长不符 / 到达时间为负 |
| E_JITTER | 未知抖动模式标记，整份样本拒绝加载 |
| E_PARA | 控制器参数越界 |
| E_DECODE | 帧流损坏（字节数不符、无法解码） |
| E_ORDER | 到达流媒体时间戳倒置且未声明 reorder |
| E_DUP | 重复帧超出容忍度 |
| E_DIFF | 三路径比对不一致（附首个不一致位置与前后窗口上下文） |

## 强制终止边界

进程被强制终止后，重启只能依据**已持久化且版本匹配**的窗口恢复（原子
改名保证检查点不会出现半写状态）；不承诺强制终止后仍生成完整播放报告
与延迟统计。单窗口内部异常将该窗口置为失败并允许重试，不污染已完成窗口。

## 实际验证结果

见 `exports/acceptance_report.md`（由 `scripts/run_acceptance.py` 生成）：
三路径逐样本一致、1/2/4/8 线程与串行一致、4 个断点恢复点一致、
6 类错误码链路全部按预期触发；`pytest` 37 项全部通过。

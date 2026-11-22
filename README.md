# 空间声学与声场重建桌面工作台

这是一个从零实现的、可离线复现的 Python 工作台。它从固定的多通道脉冲响应
样本出发，完成分帧到达时刻/早期反射能量估计、传播时差与 DRR 重建、干声卷积
渲染、误差上界报告、四态帧状态机、SQLite 原子检查点，以及实时、断点恢复、
持久化参数全量复算三条路径的逐样本校验。

项目不依赖 Docker、网络服务、真实录音或音频设备。样本、真值、报告和异常注入
全部在本地确定性地生成。

## 环境与离线安装

目标环境为 Windows + Python 3.10 及以上。代码只依赖 NumPy；桌面面板额外使用
PyQt5。仓库不内置二进制 wheel，准备一个本地 wheelhouse 后执行：

```powershell
py -3.10 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --no-index --find-links .\wheelhouse numpy PyQt5
```

也可以使用仓库提供的脚本：

```powershell
.\scripts\setup_offline.ps1 -Wheelhouse .\wheelhouse
.\scripts\run_tests.ps1
.\scripts\run_verify.ps1
```

不安装包时，在源码根目录直接执行 `python -m space_audio ...` 也可以。若依赖已
解包到本地 `.deps/`，可运行：

```bash
PYTHONPATH=.deps:. python run_checks.py
```

## 快速验证

```bash
# 生成固定种子样本
python -m space_audio generate --workdir artifacts/demo --seed 20261123 --frames 8

# 生成两条不同布局的样本并写入 dataset_manifest.json
python -m space_audio generate --workdir artifacts/demo --count 2

# 跑完整验收：三路径、检查点失效、五类异常与首个分歧定位
python -m space_audio verify --workdir artifacts/verification --frames 8

# 启动 PyQt5 面板
python -m space_audio gui
```

`verify` 会清空目标工作目录中的旧结果，只保留本次生成的样本、报告、CSV、WAV
与 NPZ。当前仓库中的实际结果位于：

- `artifacts/verification/verification.json`
- `artifacts/verification/exports/studio_quad_report.json`
- `artifacts/verification/exports/studio_quad_frames.csv`
- `artifacts/verification/exports/studio_quad_render.wav`
- `artifacts/verification/exports/studio_quad_render.npz`

## 项目结构

```text
space_audio/
  generator.py   固定种子多通道 IR、干声与解析构造真值
  model.py       样本/构造/真值模型及从构造计算真值
  loader.py      manifest 与 NPY/NPZ 加载、元数据/声道路由校验
  analysis.py    分帧、检峰、能量积分、TDOA/DRR、误差记录
  bounds.py      仅由观测参数推导的四项误差上界
  render.py      重建 IR、限并发通道渲染、OLA FFT 卷积
  state.py       待估计/已估计/已提交/失败四态帧状态机
  checkpoint.py  SQLite 单写者、原子提交、全快照失效
  pipeline.py    实时/续跑/全量复算、进度、比对与 E_DIVERGE
  faults.py      由输入副本触发五类可复现异常
  gui.py         PyQt5 桌面面板
  cli.py         命令行入口
tests/           unittest 验收测试
scripts/         Windows 离线安装与运行脚本
```

## 样本与解析真值

生成器使用 `numpy.random.default_rng(seed)` 生成干声，并以解析公式构造每个通道：

- 高斯直达脉冲，中心可位于小数样本位置；
- 若干净反射冲激，具有确定的时延与幅度；
- 指数衰减双频尾音；
- 每帧真值直接从上述构造参数计算，不从分析结果或渲染结果反推。

manifest 同时保存 `construction` 和 `truth`。`truth` 提供逐通道摘要和逐帧直接
路径延迟、早期反射能量、尾音能量、DRR、TDOA 与反射位置。分析时可由
`construction` 针对当前帧长、帧移、估计窗长重新计算真值，因此修改参数时不需要
复用旧真值。

默认样本为 48 kHz、quad 布局、6/8/12 帧配置；每个通道有独立的小数样本延迟，
因此 TDOA 与检峰量化误差均可见但不为零。

## 分帧与重建定义

第 `k` 帧分析起点为 `k * hop`：

1. 检峰窗口：`[start, start + window_len)`。
2. 直接能量：以插值峰值中心为圆心，在 `±direct_half_width` 个样本内积分。
3. 早期反射能量：直接能量区结束到估计窗结束之间积分。
4. 晚期/混响能量：`[start + window_len, start + min(frame_len, hop))`。
5. `DRR = direct_energy / reverb_energy`。
6. `TDOA_c = delay_c - delay_reference`。

`window_len < hop` 是配置约束，这样早期窗和晚期窗不会包含下一帧的直达事件。
帧内检峰使用 `interpolation_order` 阶局部多项式；默认二阶得到小数峰值位置。

坏样点、NaN、超出 `clip_ceiling`、峰值低于能量阈值、解析构造中的直达事件时序
倒置都会在对应分析帧内定位并返回带通道/帧号的错误。当帧移小于样本事件间隔时，
没有能量的空分析帧会按坏帧返回 `E_FRAME`，不会伪造到达时刻。

## 误差上界

每个已估计帧同时保存单帧界和累计界。所有界只使用已观测参数，不使用真值：

```text
quantum = 1 / sample_rate
peak_bound = 0.5 * quantum
energy_bound = min(1,
  2*(direct_half_width+1)/window_len
  + (frame_len-window_len)/(frame_len*interpolation_order))
tdoa_bound = 2 * peak_bound
conv_bound = min(2,
  0.5/(interpolation_order+1)
  + block_size/conv_window_len
  + quantum)
```

第 `k` 帧累计界为单帧界乘 `k+1`；累计观测误差按帧序相加（能量和卷积项封顶）。
单帧报告包含 `bounds`，帧序报告包含 `cumulative_bounds` 与
`cumulative_errors`。

误差记录包括：

- 检峰量化误差：估计延迟与解析延迟之差；
- 能量截断误差：直接、早期、晚期积分相对解析积分的最大相对误差；
- 时差估计误差：估计 TDOA 与解析 TDOA 之差；
- 卷积插值误差：配置阶数 Lagrange 频响相对解析复指数频响的残差。

任一项超过对应推导界时返回 `E_BOUND`，并附通道、帧号、越界量和观测/界值。

## 渲染

对每个已提交帧和通道，用持久化参数重建局部 IR：

- 直达能量按小数延迟 Lagrange 核放置；
- 早期反射能量按确定性指数权重分布；
- 晚期能量按确定性权重分布在持久化窗口内；
- 对干声做分块、分卷积窗的 OLA FFT 卷积；
- 最终只保留 `dry_length` 个样本，不向起止边界外补零或延长。

`channel_map` 为每个声明输出声道选择干声源声道，长度必须等于布局声道数；
索引非法返回 `E_PARA`，长度不匹配或 IR 声道数与布局不一致返回 `E_CHAN`。
帧估计按通道并行，渲染也按通道并行；最大并发度受 `max_workers` 限制，每通道
缓冲受 `per_thread_buffer_samples` 限制，检查点写入由单锁串行化。

## 四态帧状态机

合法迁移：

```text
PENDING   -> ESTIMATED | FAILED
ESTIMATED -> COMMITTED | PENDING | FAILED
FAILED    -> PENDING | ESTIMATED
COMMITTED -> (terminal)
```

已提交结果使用 frozen dataclass，并同时保存在状态机和检查点中。重复提交、从
待估计直接提交、修改已提交帧等非法操作返回 `E_STATE`，且不改变原状态或参数。
重配置只重建未提交状态；已提交的重建参数、时差和能量统计不被改写。

## 检查点与恢复

每帧提交时在 SQLite 的一个 `BEGIN IMMEDIATE` 事务中写入：

- 帧号与连续序列号；
- 重建参数、直达延迟、早期/晚期/总能量、DRR、TDOA；
- 四项观测误差、单帧与累计误差界；
- 输入内容哈希与算法版本。

输入哈希覆盖 manifest 原始字节、布局、IR、干声与解析构造。加载时只要输入哈希、
算法版本、样本 ID、帧序列或任一帧哈希不一致，整个快照被清空并从第 0 帧全量
重算，不会混合旧帧。暂停、取消或进程终止后，只有已提交帧可恢复；未完成运行
的 `report_complete` 为 `false`，导出会被拒绝。

## 三条路径一致性

- `run_realtime`：逐帧估计并立即提交；
- `run_resume`：只用已持久化前缀恢复，再继续未提交帧；
- `run_full_recompute`：不重新估计，使用全部持久化参数重建 IR 并渲染。

`compare_results` 先比较渲染数组的每个样本，再比较提交序列与逐帧数值。第一处
不一致会返回 `E_DIVERGE`，并给出样本位置、所属帧、两路径值和通道。

## 错误码

| 错误码 | 触发条件 |
| --- | --- |
| `E_META` | 采样率、布局、文件引用、真值或构造元数据缺失/非法 |
| `E_PARA` | 帧参数、窗长、插值阶数、卷积窗、块大小、映射索引等非法 |
| `E_FRAME` | 截断帧、NaN/Inf、静音或坏帧 |
| `E_CHAN` | IR/输出声道数与声明布局不一致、声道缺失 |
| `E_ORDER` | 解析构造中的直达事件时序倒置 |
| `E_CLIP` | 帧增益超过 `clip_ceiling` |
| `E_BOUND` | 观测误差超出仅由参数推导的上界 |
| `E_STATE` | 非法帧状态迁移或已提交帧被修改 |
| `E_DIVERGE` | 两路径首个样本/帧结果不一致 |

## 异常注入

`space_audio.faults.inject_fault` 只复制并修改输入对象，不修改源码：

```python
from space_audio.faults import FaultKind, inject_fault

bad_order = inject_fault(sample, FaultKind.TIMING_INVERSION, channel="R", frame_index=2)
bad_clip = inject_fault(sample, FaultKind.GAIN_OVERFLOW, channel="R", frame_index=2)
bad_frame = inject_fault(sample, FaultKind.BAD_FRAME, channel="L", frame_index=1)
bad_chan = inject_fault(sample, FaultKind.CHANNEL_MISSING, channel="Ls")
bad_bound = inject_fault(sample, FaultKind.BOUND_VIOLATION, channel="R", frame_index=2)
```

## 实际验证结果

本机环境：Linux/WSL2，Python 3.14.4，NumPy 2.5.5，PyQt5 5.15.11（均为本地
wheel 解包）。实现使用 Python 3.10+ 语法并通过源码运行。

`python3 run_checks.py` 实际结果：

- 19 项测试全部通过；
- 覆盖固定种子复现、解析真值、四项界检验、三路径逐样本一致；
- 覆盖并发/串行渲染一致、检查点输入哈希与算法版本整体失效；
- 覆盖四态机非法迁移、失败帧重试、已提交帧冻结；
- 覆盖 `E_CHAN`、`E_ORDER`、`E_CLIP`、`E_FRAME`、`E_BOUND` 和首个
  `E_DIVERGE` 定位；
- PyQt5 面板在 `QT_QPA_PLATFORM=offscreen` 下构建通过。

`python -m space_audio verify --workdir artifacts/verification --frames 8` 实际结果：

- `passed: true`；
- 4 声道、48 kHz、8 个已提交帧；
- 渲染输出 `4 x 16800`，与 16,800 样本干声严格等长；
- 实时 vs 断点恢复：67,200 个样本逐样本相等；
- 实时 vs 持久化参数全量复算：67,200 个样本逐样本相等；
- 检查点输入哈希变化触发整体作废并从 0 全量重算；
- 异常码依次为 `E_CHAN`、`E_ORDER`、`E_CLIP`、`E_FRAME`、`E_BOUND`；
- 人为扰动一个渲染样本后，`E_DIVERGE` 正确报告样本 0、帧 0 和两路径值。

完整数值、上界和导出路径见 `artifacts/verification/verification.json`。

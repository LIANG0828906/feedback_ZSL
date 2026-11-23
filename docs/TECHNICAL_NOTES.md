# LoudBench 技术说明

面向游戏与互动媒体音频素材的广播响度与动态范围处理工作台。全部处理在
本地完成：NumPy/SciPy 做 DSP，PySide6 做界面，WAV 做导出/试听，无任何
外部音频服务或网络账号依赖。

## 1. 环境与任务环境偏差

| 项 | 任务要求 | 实际交付 |
|---|---|---|
| Python | 3.11 + pip | uv 安装 CPython 3.11.18，`.venv` 内带 pip |
| 工作目录 | Windows 工作目录 | 实际执行环境为 Ubuntu 26.04（无 Windows 主机/sudo）；代码与路径均跨平台，Windows 下等价命令见下 |
| 依赖 | NumPy/SciPy/PySide6 锁定 | `numpy==2.1.3`、`scipy==1.14.1`、`PySide6==6.8.1`（见 `requirements.txt`/`requirements.lock`） |
| 容器 | 禁用 Docker | 未使用任何容器 |

Windows 等价初始化：

```powershell
py -3.11 -m venv .venv
.venv\Scripts\python -m pip install -r requirements.txt
.venv\Scripts\python samples\generate_samples.py --out samples\data
```

## 2. 项目结构

```
src/loudbench/
  core/        错误码、参数配置与版本指纹、状态机、节目段模型
  dsp/         K 加权、门限响度、压缩器、限幅器
  audio/       本地 WAV IO、内容哈希
  pipeline/    单段处理、全局归一、检查点、编排调度、比对、报告、应用门面
  gui/         PySide6 主窗口与自绘控件
  cli/         命令行入口
samples/      确定性样本生成器（含逐段真值清单）
tests/        44 个单元/集成/GUI 测试
```

## 3. 八项业务要求落点

1. **K 加权与门限响度**（`dsp/kweighting.py`、`dsp/loudness.py`）
   - 两级滤波按 BS.1770-4 附录精确双线性变换，按实际采样率设计；
     48 kHz 高通级与附录规范系数误差 < 1e-5。
   - 0.4s 帧长 / 0.1s 帧移、绝对门限 -70 LKFS、相对门限 -10 LU；
     立体声通道权重 +1.5 dB；真峰值 4 倍上采样。
   - 采样率白名单、帧参数越界返回 `E_PARA` 并带字段名。
2. **压缩与限幅**（`dsp/compressor.py`、`dsp/limiter.py`）
   - 压缩器一阶启动/释放（`alpha=exp(-1/(fs*tau))`），增益逐样本平滑无阶跃。
   - 限幅器前瞻窗口 + 平滑增益 + 数值安全二次修正，不做硬裁剪；
     输出长度/通道数严格不变，限幅后真峰 ≤ 上限，越界 `E_PARA`。
3. **全局归一**（`pipeline/normalizer.py`）
   - 段必须按 0..N 顺序提交，乱序 `E_STATE`；一次增益达目标响度，
     真峰超限触发受控二次衰减并记录 `secondary_correction_db`，
     每段主增益/修正/前后真峰/输出响度全链路可追溯。
4. **状态机**（`core/states.py`）
   - `PENDING -> PROCESSING -> COMMITTED`，`PROCESSING -> FAILED`，
     `FAILED -> PROCESSING` 重试；非法迁移 `E_STATE` 且原状态不变。
5. **并行与资源预算**（`pipeline/orchestrator.py`）
   - 线程池并行做单段 DSP（受 `max_workers` 与每线程缓冲样本上限约束），
     归一在顺序屏障内串行；检查点串行原子写，导出按时间轴拼接。
     三路径不一致由 `pipeline/compare.py` 抛 `E_DIFF`，定位段号与首个采样。
6. **检查点与恢复**（`pipeline/checkpoint.py`）
   - 每段一个 `.npz`（临时文件 + `os.replace` 原子提交）+ manifest 记录
     内容哈希、参数指纹、算法版本、累计统计。任一不匹配则**整体快照作废**，
     禁止部分混用；恢复路径与并行路径逐样本一致。
7. **可视化**（`gui/`）
   - 时间轴/状态、波形、FFT 频谱、逐段响度/真峰曲线、压缩增益曲线、
     差异热力图；参数面板可改目标响度/压缩/限幅并触发重算；
     本地 WAV 试听与导出。已提交段为终态，参数变化不静默改写内存结果，
     旧检查点因指纹变化整体作废后按新参数全量重算。
8. **异常与强制终止**
   - 损坏 WAV/清单 -> `E_DECODE`；单段异常置 FAILED 并隔离，可重试；
     取消抛 `E_CANCEL`；`is_complete()` 仅在全部段落盘时才允许出完整报告，
     强制终止后只依据版本匹配的检查点恢复，不谎报完整报告。

## 4. 三路径逐样本一致性的确定性来源

- DSP 全部为无状态纯函数，float64 计算，无随机量、无全局可变缓存；
- 并行只作用于彼此独立的单段 DSP，全局归一带顺序约束且只在主线程提交；
- 检查点保存归一后 float64 样本，恢复直接载入，不重新滤波；
- 拼接阶段的重采样同样是确定性多相 FIR，三条路径输入完全相同。

## 5. 实际验证结果

执行 `bash run_checks.sh`（或 CLI `verify` + `unittest`）：

- 确定性样本：6 段（48k/44.1k，立体声/单声道，含突发瞬态与静音间隙）。
- 门限生效：每段均有帧被门限扣除（如段 0：33/37 帧通过）。
- 三路径（并行 / 串行全量 / 断点恢复）逐样本严格一致，报告一致。
- 归一后每段输出响度 = -23.0000 LUFS，整体 -23.000 LUFS，落在 0.5 LU 容差内。
- 限幅后所有段真峰 ≤ -1.0 dBTP；二次修正路径有独立测试覆盖。
- 真值复算最大响度误差约 2.7e-4 LKFS（来自 16-bit PCM 落盘量化，上界内）。
- 测试：`Ran 44 tests ... OK`，覆盖 K 加权规范系数、门限、压缩/限幅、
  状态机、归一追溯、三路径一致性、检查点作废/恢复、`E_PARA/E_STATE/
  E_DECODE/E_DIFF/E_CANCEL`、缓冲预算与 GUI 离屏渲染。

## 6. 常用命令

```bash
source .venv/bin/activate
python samples/generate_samples.py --out samples/data   # 生成样本
python -m loudbench.cli.main verify                       # 三路径验收
python -m loudbench.cli.main run --serial                 # 串行全量
python -m loudbench.cli.main run --restore                # 断点恢复
python -m loudbench.gui.app                               # 图形界面
python -m unittest discover -s tests                      # 全量测试
```

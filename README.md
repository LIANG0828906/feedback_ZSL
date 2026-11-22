# Resona Restore

面向游戏与多媒体素材的本地语音/音乐修复桌面工具。处理管线以固定采样边界分段，逐段执行 STFT、噪声谱估计、谱减、掩膜平滑与 ISTFT/重叠相加；提交采用带版本标识的原子检查点，可从已提交段恢复。

## 已实现能力

- 单声道/立体声 WAV，帧长、帧移、窗函数、谱减强度、增益下限、噪声帧数与噪声模式可配置。
- 每段独立噪声轮廓：前若干帧或低能量帧检测；频率平滑与时间掩膜平滑。
- `PENDING -> PROCESSING -> COMMITTED/FAILED`，失败段可 `FAILED -> PROCESSING` 重试；非法迁移返回 `E_STATE` 且状态不变。
- 固定采样点边界分段，尾段允许非整段长度；导出严格按时间轴顺序拼接。
- 原子 NPZ 分段提交 + 原子 manifest；快照及每段都带 schema、pipeline、音频哈希、边界和配置指纹。
- 版本/身份不匹配时整体快照移至 `invalidated-*`，不会混用部分旧段。
- 暂停停在已提交边界；停止/强杀不生成完成报告；重启只恢复持久化提交。
- 有界并行处理，检查点写入在工作线程之外串行、升序提交。
- 实时处理、断点恢复、全量重算三条路径共用同一个纯分段 DSP 函数，输出逐样本一致。
- PySide6 桌面界面：分段状态表、提交进度、波形对比、前后频谱、逐段试听、未提交预览、导出、审计日志。
- 本地确定性样本生成，无网络、无外部账号、无在线音频服务。

## 实际环境

本次交付实际运行环境：

| 组件 | 实测版本 |
| --- | --- |
| OS | Ubuntu 26.04 / WSL2 Linux |
| Python | 3.12.16 |
| NumPy | 2.5.5 |
| SciPy | 1.18.1 |
| PySide6 | 6.12.0 |

项目要求 Python `>=3.11,<3.13`，依赖已固定于 `requirements.txt` 和 `pyproject.toml`。代码使用跨平台 NumPy/SciPy/PySide6 API，Windows 路径、临时文件和 `os.replace` 均可工作；但当前执行容器不是原生 Windows，因此没有把本次 Linux/WSL 结果声称为 Windows 实机结果。Windows 上的等价准备命令见下文。

## Windows 启动

```powershell
py -3.12 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
python -m pip install -r requirements.txt
python -m pip install -e . --no-deps

python scripts\generate_samples.py --output artifacts\samples
python -m audio_repair.cli verify
python -m audio_repair.cli ui
```

若系统未注册 `py` 启动器，可直接使用已安装 Python 3.11/3.12 的 `python.exe`。

## Linux / WSL 启动

```bash
python3.12 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
python -m pip install -e . --no-deps

python scripts/generate_samples.py --output artifacts/samples
python -m audio_repair.cli verify
python -m audio_repair.cli ui
```

也可使用 `uv`：

```bash
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -r requirements.txt
uv pip install --python .venv/bin/python -e . --no-deps
```

## 工作区路径

所有运行数据默认位于工作区，不需要管理员权限：

- 样本：`artifacts/samples/`
- 验收检查点与日志：`artifacts/verify/`
- 验收导出：`artifacts/exports/`
- UI 检查点：`artifacts/ui_checkpoints/`
- UI 导出：`artifacts/ui_exports/`
- 试听临时 WAV：`artifacts/audition/`
- 验收报告：`artifacts/acceptance_report.json`
- 摘要：`artifacts/acceptance_summary.md`
- UI 截图：`artifacts/ui_smoke.png`

## 确定性样本

`scripts/generate_samples.py` 使用固定随机种子生成：

| 文件 | 采样率 | 声道 | 采样点数 | 内容 |
| --- | ---: | ---: | ---: | --- |
| `music_noisy_stereo.wav` | 44100 | 2 | 327663 | 和弦/低音/打击 + 粉红噪声、50/150 Hz 嗡声、稀疏爆点 |
| `speech_noisy_mono.wav` | 32000 | 1 | 185920 | 共振峰式旁白音节 + 粉红噪声、50 Hz 嗡声 |
| `boundary_mono_1000.wav` | 16000 | 1 | 1000 | 短于一个 STFT 帧 |
| `boundary_stereo_tail1.wav` | 22050 | 2 | 8193 | `4096*2+1`，单采样尾段 |

`artifacts/samples/manifest.json` 保存每个 WAV 的 SHA-256。重新生成会得到逐字节相同文件。

## 操作桌面界面

1. 启动 `python -m audio_repair.cli ui`，默认加载歌曲样例；也可打开任意本地 WAV。
2. 左侧查看分段边界、状态、配置指纹和 `committed/total` 进度。
3. 中间修改强度或 DSP 参数；已提交段不会被改写，新参数只进入后续批次。
4. `Preview` 计算当前段但不提交，显示原始/修复波形和前后频谱；`Original`、`Repaired` 可逐段试听。
5. `Start / Resume` 启动或继续；`Pause` 停在已提交边界；`Stop` 只保留已提交段。
6. 全部段提交后 `Export WAV` 按时间轴导出；未完成时导出会返回 `E_STATE`。
7. `Reset full` 清理当前检查点目录后从头重算；版本不匹配则由加载器整体移至 `invalidated-*`。
8. 运行中关闭窗口会提示：停止不会生成完整退出报告。外部强杀同样只能从已提交段恢复。

## 命令行

```bash
# 生成固定样本
python -m audio_repair.cli samples --output artifacts/samples

# 修复 / 断点继续（默认读取同一声明快照）
python -m audio_repair.cli run \
  --input artifacts/samples/speech_noisy_mono.wav \
  --output artifacts/exports/cli_speech.wav \
  --checkpoint artifacts/checkpoints/cli_speech \
  --profile speech --segment-samples 16384 --workers 4

# 清空快照后从头运行
python -m audio_repair.cli run ... --clear

# 不读/写检查点的一次性全量重算
python -m audio_repair.cli run ... --full-recompute

# 业务错误以 JSON 输出，例如 E_PARAM
python -m audio_repair.cli run ... --strength 9

# 一键验收
python -m audio_repair.cli verify
```

## 业务错误码

| 代码 | 含义 |
| --- | --- |
| `E_PARAM` | 帧长/帧移/强度/声道/内存预算等参数越界或组合不可覆盖时间轴 |
| `E_STATE` | 非法状态迁移、未完成时间轴导出、无运行中的提交 |
| `E_DECODE` | 文件不存在、损坏、非 WAV、无可解码样本、非法声道或非有限值 |
| `E_IO` | 检查点/清单/导出无法原子持久化 |
| `E_INTERNAL` | 单段处理内部异常，段被置为 `FAILED`，可重试 |
| `E_CANCELLED` | CLI 被用户中断 |

## 实测结果

最终验收命令：

```bash
python scripts/generate_samples.py --output artifacts/samples
python scripts/run_acceptance.py
python -m unittest discover -s tests -v
QT_QPA_PLATFORM=offscreen python scripts/ui_smoke.py
```

本次结果：

- 自动验收：**50/50 检查通过**。
- 单元测试：**12/12 通过**。
- UI 冒烟：20/20 分段提交，截图 `artifacts/ui_smoke.png`。
- 歌曲三路径完全一致：327663 采样点、2 声道、20 段、尾段 16367 点；首差为空；输出 SHA-256 `60d72c319b5054bd60734b65b52d6f5e0b46d3af0ea6bcc9808880a5068dc80c`。
- 歌曲统计：峰值 `0.5469283875681209`（`-5.241390691613628 dBFS`）、RMS `0.04740559084782712`（`-26.483408722984294 dBFS`）、集成响度 `-24.991154544183686`。
- 旁白三路径完全一致：185920 采样点、1 声道、12 段、尾段 5696 点；首差为空；输出 SHA-256 `d43d458a27ebeaf1e0f2ba4d4032e7e39486a0b39ffa291dbfed66472b1814a5`。
- 旁白统计：峰值 `0.6662335322536714`（`-3.527470251682592 dBFS`）、RMS `0.09541133354108819`（`-20.40800068159834 dBFS`）、集成响度 `-20.991881269569905`。
- 硬退出测试：子进程在 4 个已提交段后 `os._exit(27)`；重启只加载 4 段，再计算 8 段完成，结果与全量重算逐样本相同。
- 版本测试：把 manifest `schema_version` 改为 999 后，整个快照被移动，加载 0 段，所有段回到 `PENDING`。
- 暂停测试：暂停时 `committed=2`、处理中段集合为空；继续后完成且输出与全量重算一致。
- 失败重试：注入第 1 段故障后 2/3 提交、1 段 `FAILED`，导出返回 `E_STATE`；重试后 3/3 完成。
- 并发提交：4 个处理线程时，检查点最大同时写入数为 1，提交顺序 `[0, 1, ...]` 稳定升序。
- 参数隔离：修改强度后，段 0 的输出 SHA-256、配置强度和配置指纹均未变化。

完整数字与检查明细见 `artifacts/acceptance_report.json`。

## 设计说明

更完整的算法、检查点协议、并发模型与测试方法见 `docs/TECHNICAL.md`。

## 已知边界

- 本次未在原生 Windows 实机执行；Windows 命令与跨平台实现已提供。
- 集成响度使用确定性的 BS.1770 风格门限统计，用于三路径一致性比较，不作为广播合规认证仪表。
- 单进程内多工作线程的检查点写入已串行化；同一检查点不应同时被多个应用进程驱动。
- 检查点中的修复段是完整时域重建结果，另含起止采样边界、STFT 参数与配置指纹；恢复时无需重算已提交段。

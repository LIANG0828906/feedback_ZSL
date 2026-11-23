# MediaForge 多媒体容器工作台

面向游戏与互动媒体素材的确定性容器封装、元数据版本映射、跨版本导出、检查点恢复与独立校验桌面工作台。项目从空工作区构建，核心算法只依赖 Python 标准库；桌面层优先使用 PySide6，并提供 PyQt5 兼容入口用于无 PySide6 环境的离屏烟测。

## 交付内容

- `mediaforge/`：统一模型、v1/v2/v3 元数据编解码、裸流导入、确定性封装、独立校验器、分段检查点、异常注入、CLI 与样本生成器。
- `mediaforge/gui/`：可编辑轨道属性与章节表的 PySide6 桌面工作台。
- `samples/`：固定种子 `20261123` 生成的 3 组音视频素材、每组 3 版元数据、真值指纹、异常参数和损坏分片样本。
- `tests/`：49 项自动化测试，覆盖正常链路、精度边界、跨版本往返、异常码、并发、恢复与 Qt 烟测。
- `scripts/`：Windows/Linux 环境准备、自检、端到端演示和测试入口。
- `artifacts/demo/validation_report.json`：一次完整实测的机器可读报告。

## 快速开始

本地离线验证（本工作区已具备解释器与依赖，无需联网、无需 pip）：

```bash
export PYTHONPATH="$PWD/.deps${PYTHONPATH:+:$PYTHONPATH}"
python3 run_workbench.py
python3 -m mediaforge selfcheck --samples samples --checkpoint artifacts/checkpoints
python3 -m unittest discover -v
python3 scripts/run_demo.py
```

`python3` 解析为本机已安装的离线解释器（当前 WSL 主机为 `/usr/bin/python3`，Python 3.14，预装 numpy 与 PySide6）；核心算法只依赖标准库，GUI/样本依赖已在该解释器中就绪。如需独立虚拟环境，可从本地 `wheelhouse/` 离线安装，见 `docs/WINDOWS_SETUP.md` 与 `scripts/setup_linux.sh`，安装命令统一使用 `--no-index --find-links wheelhouse`。

Windows 10/11 + Python 3.11 的目标流程：

```powershell
.\scripts\setup_windows.ps1
.\.venv\Scripts\python.exe run_workbench.py
```

本地 wheel 放入 `wheelhouse/`，安装命令使用 `--no-index --find-links wheelhouse`，样本生成与运行不下载任何素材或依赖。详见 `docs/WINDOWS_SETUP.md`。

## 常用 CLI

```bash
python3 -m mediaforge generate-samples --out samples --seed 20261123
python3 -m mediaforge inspect --sample samples/g01_neon_city --source-version v3
python3 -m mediaforge pack --sample samples/g01_neon_city --source-version v3 --target v1 --out artifacts/g01_v1.mfz
python3 -m mediaforge pack --sample samples/g01_neon_city --source-version v3 --target v3 --out artifacts/g01_seg.mfz --segmented --workers 4 --checkpoint artifacts/checkpoints
python3 -m mediaforge validate --artifact artifacts/g01_v1.mfz --truth samples/g01_neon_city/truth.json
python3 -m mediaforge roundtrip --sample samples/g01_neon_city --source-version v3 --target v1
python3 -m mediaforge readback --artifact artifacts/g01_v1.mfz
```

异常注入参数位于 `samples/injections/`：

```bash
python3 -m mediaforge pack --sample samples/g01_neon_city --target v3 --out artifacts/injected/conflict.mfz --injection samples/injections/conflict.json
python3 -m mediaforge pack --sample samples/g01_neon_city --target v3 --out artifacts/injected/corrupt.mfz --segmented --checkpoint artifacts/injected/checkpoints --injection samples/injections/corrupt_segment.json
```

组合注入（例如分段损坏与元数据冲突/时间戳倒置/缺失轨道同时存在）时，每一项请求的故障都会被校验并报告，不会因为存在分段 `corrupt_segment` 而被静默丢弃。

## 确定性不变量

- 规范 JSON 使用固定键排序、紧凑分隔符、UTF-8 和禁用 NaN；封装产物不含时间戳、绝对路径、线程调度信息或随机值。
- 时间换算全部使用 `fractions.Fraction` 与整数 tick，不可整除时返回 `E_PREC`，绝不静默舍入。
- 章节先按起止时刻规范化排序，再检查非重叠与容器边界；错误包含章节编号及字段路径。
- 串行与并行使用同一分段信封、同一排序和同一最终装配函数；并发度不进入请求签名，因此不改变产物字节。
- 检查点的段文件与清单均通过临时文件 + `fsync` + `os.replace` 原子写入；进程内互斥锁与跨进程文件锁保证写入不交错。

## 样本布局

```text
samples/
  index.json
  g01_neon_city/
    metadata/meta_v1.json
    metadata/meta_v2.json
    metadata/meta_v3.json
    raw/video_main.json
    raw/audio_main.json
    truth.json
  g02_clockwork/...
  g03_tidal/...
  injections/{conflict,order,missing_track,corrupt_segment}.json
  anomalies/corrupt_audio_fragment.json
```

三组样本分别为 1.5s、1.2s、1.8s；视频为逐帧网格图案，音频为整数三角波合成 PCM；三版元数据分别使用毫秒、分数时基 tick、微秒与章节帧表表达时长/章节。

## 文档

- `docs/ARCHITECTURE.md`：模型、版本映射、封装格式、检查点与并发设计。
- `docs/ERROR_CODES.md`：错误码、触发条件与字段路径约定。
- `docs/VALIDATION_RESULTS.md`：实际环境、测试、哈希和异常链路结果。
- `docs/REQUIREMENTS_TRACEABILITY.md`：逐条业务要求到实现与测试的追踪。
- `docs/WINDOWS_SETUP.md`：Windows 10/11 + Python 3.11 本地 wheel 部署。

## 当前验证环境

交付容器为 WSL2 Linux，无法在本机执行 Windows 安装器；实际验证使用本地离线解释器 `/usr/bin/python3`（Python 3.14.4，预装 PySide6 6.12.0、numpy 2.5.5）完成，核心链路仅依赖标准库。Windows 目标环境的 Python 3.11 脚本与本地 `wheelhouse/` 离线安装流程已提供，但未在当前 WSL 主机执行。

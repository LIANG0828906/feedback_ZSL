# 实测结果

## 环境

- 主机：WSL2 Linux，`/home/liang/feedback/uid42a8691e-default-132`
- 本地离线解释器：`/usr/bin/python3`（Python 3.14.4，核心链路仅用标准库）
- 目标离线解释器：Windows + Python 3.11，依赖从仓库内 `wheelhouse/` 以 `--no-index --find-links wheelhouse` 安装
- GUI：PySide6 6.12.0（Qt 离屏烟测）
- 数值依赖：numpy 2.5.5（本地解释器预装；离线 wheel 安装路径见 `docs/WINDOWS_SETUP.md`）
- 核心算法：仅 Python 标准库，无需联网或 pip
- 样本种子：`20261123`

Windows 10/11 安装脚本、Python 3.11 venv 和本地 wheel 离线安装命令已交付；当前 WSL2 主机无法执行 Windows 安装器本身。

## 自动化测试

命令：

```bash
QT_QPA_PLATFORM=offscreen python3 -m unittest discover -v
```

结果：

```text
Ran 57 tests in ~1.1s
OK
```

覆盖：

- 三组样本和两套独立生成目录逐文件哈希一致；
- 三版本映射同核心模型、未知版本/缺字段 `E_META`；
- 四类异常注入分别进入 `E_CONFLICT`、`E_ORDER`、`E_TRACK`、`E_CORRUPT`；
- 非 PNG 封面在 `validate_model` 与 CLI `pack` 链路返回 `E_CORRUPT`（`$.cover`），不再泄漏 `NameError`；
- 分段 `corrupt_segment` 与元数据冲突/时间戳倒置/缺失轨道组合注入时，每一项故障都被校验并报告，不会被静默丢弃；
- 章节排序、重叠、越界、`E_VER`、`E_PREC`；
- 三目标版本独立校验；
- 负载/声明/真值差异定位到首个字段；
- 跨版本往返采样数、时基、章节起止严格不变；
- 串行/4 线程产物相同哈希；
- 检查点复用、单段失败重试、外部分片损坏全量失效、版本篡改全量失效、缓冲上限失败无部分产物；
- PySide6 窗口加载样本并刷新轨道/章节/元数据页。

## 前置自检

命令：

```bash
python3 -m mediaforge selfcheck --samples samples --checkpoint artifacts/checkpoints
```

结果：`ok=true`，40 项检查全部通过，`failure_count=0`。包括 3 个样本组、3 个元数据版本、所有 truth 指纹和文件哈希、三版本导入、独立校验、检查点写入/清理、注入损坏与单段重试。

机器可读输出：`artifacts/selfcheck.json`；完整测试日志：`artifacts/test-results.txt`。

样本 truth 指纹：

| 样本 | truth fingerprint |
|---|---|
| `g01_neon_city` | `6fc7231cbb77156a3997cf805f5b7955b3d370c3570da68db5621b94c1766756` |
| `g02_clockwork` | `1a0abb38faf516f1b21062d5cb3ca6d4e7d9c4d29b0d83caf6b683c40de03d95` |
| `g03_tidal` | `e14834853d068ebeea0d44bd69e0f5528f9b5c92d3f5dc98e64d1cc74b1d85e1` |

## 端到端实验

命令：

```bash
python3 scripts/run_demo.py
```

`g01_neon_city` 从 v3 内部模型导出：

| 目标 | 产物 SHA-256 | 重复导出一致 | 独立校验 | E_LOSSY |
|---|---|---|---|---|
| v1 | `911951c9a1dc854de04fb681a600acede40811b7f4b191c751ab76f62a62357b` | 是 | 通过 | 17 |
| v2 | `b86f58b8719515fa9f3b9894fc6c751533eec6f722525a6cc7ef614947e4c6cf` | 是 | 通过 | 15 |
| v3 | `c73cf5755151f375cc347ef8c8e332e1b9592dfa6c5f4f6a7beb5f10508ffeea` | 是 | 通过 | 0 |

分段路径：

- 1 线程与 4 线程最终产物均为 `c73cf5755151f375cc347ef8c8e332e1b9592dfa6c5f4f6a7beb5f10508ffeea`，逐字节相同。
- 对 `audio_main` 注入分段损坏后，只该段进入失败，`video_main` 分段保持完成。
- 重试只重建 `audio_main`，复用 `video_main`，恢复产物仍为同一 SHA-256。
- 失败尝试不保留目标 `.mfz`，检查点状态为 `failed`，不会生成完整报告。

异常入口：

| 注入 | 错误码 | 字段路径 |
|---|---|---|
| 元数据语言冲突 | `E_CONFLICT` | `$.tracks[0].language` |
| 视频帧时间倒置 | `E_ORDER` | `$.tracks[0].frames[2].pts_ticks` |
| 缺失音频轨 | `E_TRACK` | `$.tracks[1]` |
| 音频样本损坏 | `E_CORRUPT` | `$.tracks[1].payload_sha256` |
| 分段损坏 | `E_CORRUPT` | `$.segment.sha256` |

完整机器可读报告：`artifacts/demo/validation_report.json`。

该报告自指纹：`5d8c3210530aa7404db0b1dcd2b48375c83e4022a620192b5a350587ef64d85c`。

## GUI 烟测

Qt 离屏启动 `MainWindow`，加载 `g01_neon_city`：

- 样本组发现 3 个；
- 轨道表 2 行；
- 章节表 3 行；
- 目标 v1 元数据预览成功并显示 17 条 `E_LOSSY`；
- 直接导出后独立校验 `ok=true`。

## 已知环境差异

当前 WSL2 主机没有可用于 Windows 的 wheel，验证直接使用本地离线解释器 `/usr/bin/python3`（预装 PySide6/numpy）；仓库内 `scripts/setup_windows.ps1`、`scripts/setup_linux.sh` 和 `wheelhouse/README.md` 给出目标环境的离线安装路径，安装统一使用 `--no-index --find-links wheelhouse`。

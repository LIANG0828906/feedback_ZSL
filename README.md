# 透明介质与体积光渲染管线

本项目从零实现一套本地、确定性、可断点续跑的游戏场景透明介质与体积光处理管线，并附带解析真值验证、增量失效、并发调度、检查点恢复和浏览器诊断面板。

核心实现使用 Python 3.11 + NumPy 软件光栅，不依赖 GPU 扩展；PyOpenGL 仅做环境探测。Windows、Linux/WSL 均使用同一套代码和确定性样本。

## 已实现能力

- 场景加载与结构校验：`E_SCHEMA`、`E_GEOM`、`E_PARA`、`E_DECODE` 均携带字段名。
- 球体、闭合 AABB、闭合三角网格边界；界面折射、全反射分支、吸收与固定点量化。
- 逐波长折射率、吸收系数、散射系数、体积光源强度与 Henyey-Greenstein 各向异性。
- 可配置步长/采样策略的体积透射率累积和单次散射积分。
- 固定分块的五态状态机、稳定提交顺序、逐块检查点。
- 介质/光源版本、依赖图、增量失效清单和未受影响分块缓冲复用。
- 原子快照、整快照兼容性校验、崩溃后恢复、禁止部分混用。
- 线程池、最大并行度、每线程采样缓冲预算、求交缓存预算和阻塞式资源租约。
- 全量、预览、崩溃恢复三条路径的逐像素一致性验证与 `E_DIVERGE` 定位。
- 本地诊断面板：边界叠加、透射率热力图、分块状态矩阵、失效传播、差异热力图、参数编辑与导出。

详细设计、公式和验收数据见 [docs/TECHNICAL.md](docs/TECHNICAL.md)。

## 环境准备

### Windows

```powershell
powershell -ExecutionPolicy Bypass -File scripts/setup_windows.ps1
```

脚本使用 `py -3.11` 创建 `.venv`，安装 NumPy、PyOpenGL、pytest 与项目本身，然后生成固定种子样本并运行 `doctor`。

### Linux / WSL

```bash
./scripts/setup_posix.sh
```

本工作区实际验证环境为 WSL2 + Python 3.11.18 + NumPy 2.4.6 + PyOpenGL 3.1.10。默认进入软件渲染路径；`doctor` 会明确报告是否存在可用窗口上下文。

## 快速开始

```bash
# 1. 固定种子生成样本与解析真值
.venv/bin/python -m transparent_pipeline.cli generate --out samples --seed 20261123

# 2. 检查运行环境
.venv/bin/python -m transparent_pipeline.cli doctor

# 3. 解析真值 + 全量/预览/断点恢复三路径验收
.venv/bin/python -m transparent_pipeline.cli verify \
  --work runtime/verification \
  --report runtime/verification/report.json

# 4. 运行回归测试
.venv/bin/python -m pytest -q

# 5. 启动本地诊断面板
.venv/bin/python -m transparent_pipeline.cli serve --port 8765
# http://127.0.0.1:8765/

# 6. 导出静态面板、结果 JSON 与 NPZ 缓冲
.venv/bin/python -m transparent_pipeline.cli export-ui --out runtime/exports

# 7. 可选：复跑完整验收并刷新 artifacts/validation_report.json
.venv/bin/python scripts/run_validation.py
```

## 样本

`samples/scene.json` 包含：

- 3 个透明介质：玻璃球、雾腔、水面层。
- 3 个逐波长体积方向光：主光、补光、轮廓光。
- 3 个波段：450/550/650 nm。
- 5 个严格递增时刻的相机轨迹与视锥参数。

`samples/truth.json` 包含与场景哈希绑定的解析参考：

- 轨迹首尾两个时刻共 18 条介质边界出射点真值。
- 18 条路径透射率真值。
- 54 条逐光源散射积分真值。

固定种子 `20261123` 下场景哈希为：

```text
30c8678dcd26e1201d1c77588a3990f5bd992b47ec196d0cd8d3f5e3978c35c2
```

## 主要命令

| 命令 | 用途 |
| --- | --- |
| `generate` | 生成确定性场景、真值与 manifest |
| `doctor` | 报告 Python、NumPy、PyOpenGL、显示环境 |
| `render --mode full` | 全量渲染并逐块提交 |
| `render --mode preview` | 预览路径；可设置更低步长并记录误差上界 |
| `render --mode resume` | 从兼容检查点继续 |
| `render --mode incremental` | 只重算待求交或失效分块 |
| `verify` | 验证解析真值、收敛性和三路径一致性 |
| `serve` | 启动本地浏览器诊断面板 |
| `export-ui` | 导出静态面板、结果和缓冲 |

## 状态与错误码

分块状态：

```text
pending_intersect -> transmission_accumulating -> scattering_accumulating -> committed
                                      \-----------------> failed -> pending_intersect (retry)
committed -> pending_intersect (仅显式失效)
```

| 错误码 | 含义 |
| --- | --- |
| `E_SCHEMA` | 必需字段/对象/数组结构缺失或类型错误 |
| `E_GEOM` | 边界不闭合、退化或非法几何 |
| `E_PARA` | 折射率、系数、波长长度、步长、各向异性等越界 |
| `E_STATE` | 非法分块迁移或未完成进程状态 |
| `E_DIVERGE` | 三路径/全量比对出现逐像素差异 |
| `E_DECODE` | 场景或持久化数据无法解码 |

所有 CLI/API 错误都返回 JSON，并包含 `code`、`field`、`message`，需要时附 `details`、实测值与期望值。

## 检查点

默认目录：

```text
runtime/checkpoints/
  checkpoint.npz       # 单个原子快照，包含缓冲、版本、哈希、记录与提交序列
  commit_log.jsonl     # 每个成功提交分块的串行日志
  resume_decision.json # 最近一次恢复决定
  decision_log.jsonl   # 恢复/作废历史
  rejected/            # 不兼容或损坏的旧快照
```

每提交一个分块会写入透射率缓冲、散射缓冲、固定点量、介质/光源版本、场景哈希、算法版本、配置哈希和逐块校验和。写入由进程内锁和文件锁串行化，并通过临时文件 `fsync + os.replace` 原子替换。

恢复时只要场景哈希、介质版本、光源版本、算法版本、配置哈希、布局或任一块校验和不一致，就整体作废快照并全量重算。

## 验收结果

`artifacts/validation_report.json` 记录了实际运行结果：

- 解析出射点最大误差：`4.0962e-15`。
- 路径长度固定点最大误差：`4.7985e-10`。
- 透射率最大误差：`4.4220e-13`。
- 单次散射积分最大误差：`2.3493e-09`（容差 `2.0e-3`）。
- 步长从 `0.2` 收紧到 `0.0125` 时误差单调下降。
- 全量 vs 预览、全量 vs 崩溃后恢复：透射率、散射、固定点量、逐波长统计、提交序列全部逐位一致。
- 强制在第 4/9 块后崩溃，恢复后完成全部 9 块。
- 编辑玻璃球后仅失效 2/9 个分块，增量结果与新场景全量重算逐位一致。
- 降低预览步长至 `0.025` 时记录实际步长与误差上界 `3.1258e-07`；该模式明确标记为非精确等价。
- 回归测试：`42` 项全部通过。

## 目录结构

```text
src/transparent_pipeline/
  scene.py              # 场景/真值加载与校验
  sample_generator.py   # 固定种子样本与解析真值
  geometry.py           # 求交、折射、全反射、路径追踪
  fixedpoint.py         # 路径/消光固定点量化
  scattering.py         # HG 相位函数与单次散射积分
  renderer.py           # 分块软件渲染
  state.py              # 五态分块状态机
  dependencies.py       # 资源依赖与失效传播
  checkpoint.py         # 原子快照与兼容性判定
  resources.py          # 并发/采样/求交缓存预算
  engine.py             # 调度、提交、恢复、编辑
  comparison.py         # 首个差异定位
  verify.py             # 真值与三路径验证
  ui.py                 # 本地诊断面板与导出
  cli.py                # 命令行入口
tests/                  # 42 项回归测试
scripts/                # Windows/POSIX 环境与验收脚本
samples/                # 已生成的确定性样本与真值
artifacts/              # 验收报告
```

## 设计边界

- 介质是均匀、互不嵌套的闭合体；不实现多层嵌套介质和递归全局光照。
- 体积光采用单次散射近似，支持方向体积光和 HG 相位函数。
- 全反射会在同一介质内继续反射追踪，最多 `max_segments` 段。
- 软件光栅是权威结果路径；OpenGL 未用于结果生成，避免驱动差异破坏逐位一致性。
- 进程被操作系统强制终止时不承诺完整结果，只保证最后一个已提交快照可恢复。


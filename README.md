# NPR Workbench

面向游戏美术与互动媒体的确定性非真实感绘制桌面工作台。项目从零实现了三阶段 NPR 管线、16ms 级交互预览、分块并行精确渲染、原子检查点、断点恢复和三条渲染路径逐像素校验。

![Workbench](docs/workbench.png)

## 交付内容

- `npr_workbench/`：核心渲染、版本快照、分块并行、检查点、一致性比对与 Tk 桌面 UI。
- `data/scenes/`：3 个固定种子场景，每个包含球体、平面、盒体和 5 个相机位姿。
- `data/truth/`：每个相机位姿 22 个解析采样点，共 330 个真值样本。
- `data/cases/`：损坏场景、非法参数、故障块注入配置。
- `tests/`：20 项自动化测试。
- `docs/TECHNICAL.md`：管线、确定性、检查点与算法说明。
- `docs/VERIFICATION.md`：本机实际测量与验收结果。

## Windows 3.11 离线启动

前置条件：Python 3.11、pip、可创建窗口的桌面环境。python.org 的 Windows 安装包已包含 Tkinter。

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install --no-index --find-links=wheelhouse -r requirements.txt

.\.venv\Scripts\python.exe -m npr_workbench.cli generate-samples --out data
.\.venv\Scripts\python.exe -m npr_workbench.cli verify-truth --data data --out artifacts\truth_verification.json
.\.venv\Scripts\python.exe -m npr_workbench.cli benchmark --data data --out artifacts\benchmark
.\.venv\Scripts\python.exe -m npr_workbench.cli verify --data data --scene orb_and_crate --out artifacts\verification
.\.venv\Scripts\python.exe -m npr_workbench.cli ui --data data --persist persist
```

将对应 CPython 3.11 的 NumPy 离线轮子放入 `wheelhouse/`，或将 `--find-links` 指向已有本地轮子目录。核心渲染是 NumPy CPU 软件光栅器，不依赖 OpenGL 驱动；PyOpenGL 可作为可选环境项安装，但不参与确定性关键路径。

## Linux / WSL 启动

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install --no-index --find-links=wheelhouse -r requirements.txt
.venv/bin/python -m npr_workbench.cli ui --data data --persist persist
```

无显示环境时，核心渲染、样本生成和测试仍可运行；UI 冒烟测试会自动跳过。

## 工作台功能

- 分页查看基础着色、分级图、分级层映射、轮廓掩码、笔触层、差异热力图。
- 拖动画面轨道旋转相机；滑杆调整分级层数、笔触密度、笔触长度。
- 交互中只读取最新参数版本并输出低分辨率预览；停止拖动 260ms 后锁定当时版本执行精确渲染。
- 状态栏明确标注 `PREVIEW`、`EXACT RENDERING`、`PAUSED`、`CANCELED`、`TILE FAILED` 或错误码。
- 暂停、取消、强制精确重算、失败块重试。
- 本地故障块注入入口：先作废当前会话检查点，再对指定块注入异常；其他块仍正常提交，可清除故障后重试。
- 参数/相机/版本同步写入 `persist/ui_state.json`；重启后按版本恢复，并可复用已提交块。

## 命令行

| 命令 | 作用 |
| --- | --- |
| `python -m npr_workbench.cli generate-samples` | 重新生成场景、真值和异常样例 |
| `python -m npr_workbench.cli verify-truth` | 用解析采样校验三阶段输出 |
| `python -m npr_workbench.cli benchmark` | 测量预热后预览延迟与精确渲染耗时 |
| `python -m npr_workbench.cli verify` | 自动验证交互精确、断点恢复、一次性全量三路径 |
| `python -m npr_workbench.cli render` | 输出单帧 PPM/NPZ 可视化与诊断张量 |
| `python -m npr_workbench.cli ui` | 启动桌面工作台 |

## 确定性契约

1. 同一路径内三阶段分辨率必须完全一致；`level`、`outline`、`brush` 与颜色数组形状不一致即失败。
2. 每条笔触只由 `scene_content_hash + global_stroke_index` 经 SHA-512 派生；不使用线程 ID、时钟、进程 ID 或全局随机状态。
3. 精确渲染启动时锁定不可变参数/相机/裁剪面快照；预览只允许读取最新版本。版本不一致返回 `E_VER`。
4. 精确分块固定尺寸、按行优先块号提交；并行与单线程使用同一块函数，提交顺序由协调线程统一串行化。
5. 每块先写 NPZ 临时文件并 `fsync`，再 `os.replace`，随后原子更新清单；清单校验 SHA-256。
6. 恢复时若内容哈希、相机哈希或参数版本任一不同，整份会话检查点作废并全量重算。
7. 三路径校验要求全分辨率风格像素、分级层数分布、轮廓掩码与笔触覆盖逐项相等；不一致返回 `E_PATH` 和首个差异像素/块号。

跨分辨率预览/精确比对在预览网格执行，默认容差：

- 分级直方图总变差 ≤ `0.035`
- 轮廓像素覆盖率差 ≤ `0.040`
- 笔触 alpha 覆盖密度差 ≤ `0.120`
- 1 像素膨胀后轮廓 IoU ≥ `0.42`

任一超差返回 `E_DIVERGE`，保留双方张量、PPM 图像、差异热力图与 JSON 报告，UI 不提交精确帧。

## 错误码

| 错误码 | 含义 |
| --- | --- |
| `E_PARA` | 参数越界，`details.field` 指向字段 |
| `E_VER` | 参数版本冲突，附双方版本 |
| `E_SEED` | 笔触种子派生分叉，附笔触序号 |
| `E_DIVERGE` | 预览/精确指标超差，附报告和诊断路径 |
| `E_PATH` | 三路径不一致，附首差异像素与块号 |
| `E_DECODE` | 场景 JSON/模式/几何损坏 |
| `E_TILE` | 单块渲染失败，可重试 |
| `E_RESOURCE` | 像素缓冲或笔触缓存配置不可满足 |
| `E_CHECKPOINT` | 检查点 I/O 或块校验失败 |

## 测试

```bash
.venv/bin/python -m unittest discover -s tests -t . -v
```

测试覆盖参数错误、种子线程确定性、`E_SEED`、预览/精确容差、`E_VER`、`E_DIVERGE` 诊断留存、并行/串行逐像素一致、暂停恢复、版本失效、块故障重试、`E_PATH`、损坏场景、真值样本、16ms 延迟和 Tk 启动。

## 实际结果

详细记录见 `docs/VERIFICATION.md`。关键结果：

- 3 个场景、330 个真值样本全部通过。
- 预览 `120×90`，精确 `360×270`；预热后 60 次预览中位数 `12.07ms`、p95 `13.60ms`、最大 `15.74ms`。
- 精确帧约 `0.93–1.0s`，63 个块按 `0..62` 稳定提交。
- 交互精确、暂停恢复、一次性全量三路径风格像素 SHA-256 完全一致。
- 20 项自动化测试全部通过。

冷启动首次计划构建约 `110–140ms`，Tk UI 会在可交互前完成一次预热，因此拖动阶段的预算针对预热后路径。

## 目录

```text
npr_workbench/
  engine.py        预览/精确编排、恢复、路径验证入口
  raster.py        光线投射与精确/预览着色
  npr.py           色调分级、轮廓、风格合成
  strokes.py       笔触计划、种子消费、分块栅格化
  tiles.py         固定分块、资源预算、块故障
  checkpoint.py    原子块提交与清单
  consistency.py   指标比对、诊断与三路径校验
  parameters.py    版本化不可变快照
  ui.py            Tk 桌面工作台
data/              固定种子样本与真值
docs/              技术说明、验证记录、界面截图
tests/             unittest 测试套件
```

强制终止进程不承诺完整图像或报告；可恢复范围仅限已完成原子提交并出现在校验清单中的块。

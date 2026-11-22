# Vector Path Compositing Diagnostics Workbench

面向游戏 UI 与互动媒体的本地矢量路径合成、分阶段诊断与可复现性工作台。项目从固定种子生成量化定点三次贝塞尔场景，完成自适应扁平化、子采样扫描线覆盖、抗锯齿、描边连接和多路径合成，并提供 PySide6 桌面界面、版本化检查点、异常注入和逐像素差异定位。

![Workbench screenshot](artifacts/workbench_ui.png)

## 核心能力

- 确定性样本：3 份正常场景 + 4 份异常场景，固定种子可逐字节重建。
- 严格校验：量化整数坐标、闭合轮廓、段首尾连续、样式契约与真值哈希；错误返回 `E_DECODE` 或 `E_GEOM`。
- 自适应扁平化：按曲率与容差递归细分；容差收紧时折线段数单调不减，结果不含重复闭合端点。
- 覆盖与合成：任意 `1..16` 子采样网格归一化到 `[0,1]`，支持 round/bevel/miter 描边连接和 normal/add/multiply 固定路径顺序合成。
- 三阶段状态机：每条路径 `flatten -> coverage -> compose`，单元状态为 `pending/processing/committed/failed`，非法迁移返回 `E_STATE`。
- 异常链路：输入或参数注入自交、零面积、描边覆盖溢出和单阶段失败，分别返回 `E_SELF/E_DEGEN/E_CLIP/E_STAGE`；失败单元可重试。
- 版本化快照：每次阶段提交原子串行落盘，保存内容哈希、参数版本、算法版本和阶段缓存；任一版本不匹配即整体作废。
- 三路径一致性：一次性全量、增量续算、强制终止后恢复的位图尺寸、逐像素值和阶段提交序列一致。
- 诊断界面：路径状态表、折线叠加、覆盖灰度、合成结果、差异热图、暂停/恢复、可恢复取消、硬停止、重试和 PNG 导出。

## 快速开始

Windows PowerShell：

```powershell
py -3.12 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
python -m pip install -e .

python -m vector_workbench generate --out data --seed 20261123
python -m vector_workbench gui --data data --checkpoints var/checkpoints/gui
```

Linux / WSL：

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install --upgrade pip
python -m pip install -e .

python -m vector_workbench generate --out data --seed 20261123
python -m vector_workbench gui --data data --checkpoints var/checkpoints/gui
```

无显示器环境可加 `--offscreen`。完整安装说明见 [docs/INSTALL.md](docs/INSTALL.md)。

## 常用命令

```bash
# 校验场景结构、样式契约与扁平化真值
python -m vector_workbench validate --data data --scene-id hud_orbit \
  --scale 1 --tolerance 0.25 --subsamples 4 --workers 2

# 渲染单个场景
python -m vector_workbench render --data data --scene-id media_cards \
  --mode full --out artifacts/media_cards_cli.png \
  --checkpoints var/checkpoints/render

# 运行完整验收，生成 JSON 报告和校验位图
python -m vector_workbench verify --data data --artifacts artifacts \
  --checkpoints var/checkpoints/acceptance --seed 20261123

# 测试
python -m unittest discover -v
```

## 数据与持久化

- `data/scenes/`：7 份场景描述，坐标为量化整数。
- `data/truth/truth_manifest.json`：面积、包围盒、闭合检查点、曲线采样点和样式声明。
- `data/samples.json`：样本、类别、期望异常码索引。
- `var/checkpoints/`：本地持久化快照目录，运行内容默认不纳入版本控制。
- `artifacts/validation_report.json`：最近一次实际验收报告。

## 项目结构

```text
src/vector_workbench/
  generator.py   固定种子场景与真值生成
  validation.py  场景解码、几何与样式契约校验
  geometry.py    三次贝塞尔、自适应扁平化、面积与自交检测
  raster.py      扫描线覆盖、描边、抗锯齿与合成公式
  state.py       三阶段路径状态机
  checkpoint.py  版本化、原子、串行检查点快照
  pipeline.py    并行预计算、稳定提交顺序、暂停/取消/恢复
  diff.py        首个差异像素、路径与阶段定位
  ui.py          PySide6 诊断工作台
  operations.py  三路径、异常、版本失效和差异检测验收
tests/           18 项自动化测试
docs/            安装、技术和验证说明
```

## 可复现性

同一场景、缩放、容差、子采样网格与算法版本下，所有阶段计算只依赖确定性的数值路径；阶段按路径顺序提交，检查点使用已验证的 `float64` 折线和 `float32` 覆盖/合成缓存。并行只改变计算调度，不改变提交顺序或累加顺序。

实际验证结果：3 份正常场景的三条执行路径均逐像素一致，4 类异常码与阶段重试、参数/场景/算法版本失效、`E_DIFF` 定位全部通过。详见 [docs/VALIDATION.md](docs/VALIDATION.md)。

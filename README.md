# ArtMask Workbench

面向游戏美术素材的本地图像分割、掩码几何诊断与规模自适应工作台。项目不访问网络、不依赖外部素材；样本、真值、检查点与导出均保存在本地工作区。

## 已落地能力

- 固定种子生成 5 组样本：色块、渐变背景、低对比度目标、嵌入式噪声、多目标重叠，分辨率覆盖 `384×256` 到 `2048×1536`。
- 每组样本包含像素级真值掩码、区域数量、重心、面积、外接框及解析几何元数据。
- 颜色距离 + 梯度阈值的确定性区域生长，自动/手动种子，置信度阈值，形态学开闭清理。
- 掩码几何统计、闭合轮廓、匈牙利最优区域匹配、真值偏差报告。
- 基于像素数、区域数、时间/内存预算的稳定规模判定；记录模式、倍率、块尺寸、依据与降级影响。
- 精细与降采样路径的双路径实验入口，强制检查重心、面积、外接框 IoU 容差。
- 块 × 阶段四态状态机：`pending / processing / committed / failed`，冻结已提交结果，失败块可重试。
- 每块每阶段原子写入版本化检查点；重启时任一身份字段不匹配即整体作废并全量重算。
- PyQt5 诊断工作台：原图、生长掩码、最终叠加、偏差热力图、块阶段矩阵、模式依据、暂停/取消/重试/导出。

## 环境

本机实际使用环境：

| 项目 | 值 |
| --- | --- |
| Python | 3.12.16 |
| 虚拟环境 | `/home/liang/feedback/uid42a8691e-default-163/.venv` |
| NumPy | 2.5.5 |
| PyQt5 | 5.15.11 / Qt 5.15.14 |
| 持久化工作区 | `/home/liang/feedback/uid42a8691e-default-163/workspace_data` |
| 检查点目录 | `/home/liang/feedback/uid42a8691e-default-163/workspace_data/checkpoints` |
| 导出目录 | `/home/liang/feedback/uid42a8691e-default-163/workspace_data/exports` |

从零重建环境：

```bash
cd /home/liang/feedback/uid42a8691e-default-163
uv python install 3.12
uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python -r requirements.txt
uv pip install --python .venv/bin/python -e .
mkdir -p workspace_data/checkpoints workspace_data/exports workspace_data/samples
```

所有命令均可在不激活虚拟环境时直接使用 `.venv/bin/artmask`，或先执行：

```bash
source .venv/bin/activate
```

## 快速运行

生成固定种子样本：

```bash
artmask generate --workspace workspace_data
```

校验样本、清单、尺寸、通道与像素级真值：

```bash
artmask validate --workspace workspace_data
```

按默认预算运行全部分块流水线并导出：

```bash
artmask run --workspace workspace_data --export
```

运行双路径规模一致性实验：

```bash
artmask experiment --workspace workspace_data
```

启动 PyQt5 工作台：

```bash
artmask gui --workspace workspace_data
# 或
artmask-gui --workspace workspace_data
```

无显示器环境可做窗口冒烟验证：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -c \
  "from PyQt5.QtWidgets import QApplication; \
   from pathlib import Path; \
   from artmask.gui.app import WorkbenchWindow; \
   app=QApplication([]); w=WorkbenchWindow(Path('workspace_data')); print(w.windowTitle())"
```

运行测试：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -v
```

## 目录结构

```text
src/artmask/
  samplegen.py       确定性样本与像素级真值
  validation.py      加载与元数据校验
  imageops.py        NumPy 图像/形态学算子
  segmentation.py    自动种子、区域生长、后处理、降采样路径
  geometry.py        区域统计、轮廓、最优匹配、偏差报告
  budget.py          可解释的规模取舍策略
  state.py           块/阶段四态状态机
  checkpoint.py      原子检查点、版本一致性与恢复
  pipeline.py        并行块任务、串行提交、恢复与导出
  experiments.py     精细/降采样双路径可复现实验
  cli.py             命令行入口
  gui/app.py         PyQt5 诊断工作台
tests/               29 项自动化测试
workspace_data/      本地样本、检查点、导出与验收记录
TECHNICAL.md         架构、算法与状态/持久化约束
VERIFICATION.md      实际环境、指标、恢复与业务码验证结果
```

## 业务码

| 业务码 | 触发条件 |
| --- | --- |
| `E_DECODE` | 清单缺失/损坏、图像或真值文件缺失/损坏/哈希不符；`details.file` 给出文件 |
| `E_META` | 尺寸、通道、类型、区域数量、区域列表或面积与清单/掩码不一致；`field` 给出字段名 |
| `E_PARA` | 参数、预算或单块缓冲越界；`field` 给出字段名 |
| `E_REGION` | 最终区域数量与真值不一致；列出 `extra_regions` / `missing_regions` 及面积 |
| `E_SCALE` | 降采样路径超过重心、面积或外接框 IoU 容差；列出区域编号与实测指标 |
| `E_STATE` | 非法状态迁移、阶段逆序、越界块、检查点阶段链断裂 |
| `E_BUDGET` | 最大 16× 降采样仍无法满足预算 |

## 默认预算

默认时间预算 `20s`、内存预算 `512MB`、4 个工作线程、`256×256` 分块、单块阶段缓冲 `4MB`。

实际判定中，`2048×1536`、5 区域样本的精细路径预测约 `26.99s`，超过 `20s`，因此稳定选择 `2×` 降采样；其余四组选择逐像素精细路径。该判定是纯函数，同图同预算下稳定，并保留完整预测表与降级影响范围。

## 说明

- 掩码统计与轮廓只使用最终全分辨率标签掩码；降采样中间结果从不用于反推几何。
- 强制终止后只承诺恢复已原子持久化且版本完全匹配的阶段；不承诺生成完整报告。
- 检查点写入使用线程锁 + `fcntl` 文件锁 + 同目录临时文件 + `os.replace` + 目录 `fsync`。
- 未提交阶段受参数变化影响；已提交阶段冻结。块状态快照会标记失效块。

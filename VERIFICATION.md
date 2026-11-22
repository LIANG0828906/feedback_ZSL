# 实际验证记录

验证时间：2026-11-23 07:14（Asia/Shanghai）

## 1. 环境与持久化

| 项目 | 实际值 |
| --- | --- |
| Python | 3.12.16 |
| 虚拟环境 | `/home/liang/feedback/uid42a8691e-default-163/.venv` |
| NumPy | 2.5.5 |
| PyQt5 / Qt | 5.15.11 / 5.15.14 |
| 持久化根目录 | `/home/liang/feedback/uid42a8691e-default-163/workspace_data` |
| 样本目录 | `/home/liang/feedback/uid42a8691e-default-163/workspace_data/samples` |
| 检查点目录 | `/home/liang/feedback/uid42a8691e-default-163/workspace_data/checkpoints` |
| 导出目录 | `/home/liang/feedback/uid42a8691e-default-163/workspace_data/exports` |

本地读写、样本生成、检查点写入与导出均已实际执行。未使用 Docker，未访问外部素材。

## 2. 自动化测试

执行命令：

```bash
cd /home/liang/feedback/uid42a8691e-default-163
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -v
```

最终结果：

```text
Ran 29 tests in 35.687s
OK
```

覆盖内容：

- 样本生成确定性、清单字段与像素真值一致性。
- `E_DECODE`：清单缺失、图像损坏、真值缺失。
- `E_META`：通道数、宽度、区域数量等元数据不一致。
- `E_PARA`：颜色容差、梯度阈值、置信度、结构元素、最小面积、种子、预算、块缓冲。
- 同图同参数多线程重复运行掩码完全一致。
- 精细掩码对真值的重心、面积、外接框、Mask IoU。
- 最优匹配在标签重排后仍得到零偏差对应关系。
- 区域数量不一致的 `E_REGION` 多发/缺失区域编号与面积。
- 超严容差下的 `E_SCALE` 区域级实测指标。
- 状态机合法/非法迁移、阶段顺序、失败重试、冻结结果、失效块清单。
- 完整快照恢复、失败块隔离后断点续跑、参数版本整体作废、图像哈希整体作废。
- 检查点原子载荷哈希、序列号与索引顺序。
- `E_BUDGET` 与预算收紧原因/影响范围记录。
- 掩码与 JSON 报告导出。
- PyQt5 无显示器窗口创建、样本加载与模式预览。

## 3. 全样本默认预算验收

命令：

```bash
.venv/bin/artmask validate --workspace workspace_data
.venv/bin/artmask run --workspace workspace_data --export
```

默认预算：`20s / 512MB / 4 workers / 256×256 blocks / 4MB per-block buffer`。

| 样本 | 分辨率 | 区域 | 自动模式 | 提交进度 | 重心误差(px) | 面积相对误差 | 外接框 IoU | Mask IoU |
| --- | --- | ---: | --- | ---: | ---: | ---: | ---: | ---: |
| s01-color-blocks | 384×256 | 4 | fine | 6/6 | 0.0243 | 0.0162 | 0.9790 | 0.9838 |
| s02-gradient | 640×480 | 3 | fine | 18/18 | 0.1154 | 0.0060 | 0.9808 | 0.9940 |
| s03-low-contrast | 960×720 | 2 | fine | 36/36 | 0.0032 | 0.0001 | 0.9896 | 0.9999 |
| s04-embedded-noise | 1280×960 | 3 | fine | 60/60 | 0.0197 | 0.0001 | 0.9931 | 0.9999 |
| s05-multi-overlap | 2048×1536 | 5 | downsampled ×2 | 144/144 | 0.5027 | 0.0057 | 0.9828 | 0.9941 |

大图判定依据：

```text
精细模式预测耗时 26.99s 超过 20.00s；
选择 2 倍降采样，预测耗时 12.13s、内存 61.5MB。
```

降级影响范围明确记录为：区域生长在 1/2 分辨率进行、掩码最近邻上采样、几何统计/轮廓基于最终全分辨率掩码、分块提交粒度不变。

原始结果：

- `workspace_data/exports/validate.json`
- `workspace_data/exports/acceptance_run.json`
- 每样本 `*_mask.npy`、`*_initial.npy`、`*_overlay.npy`、`*_heatmap.npy`、`*_report.json`

## 4. 双路径一致性实验

命令：

```bash
.venv/bin/artmask experiment --workspace workspace_data
```

每个样本均实际运行精细与 2× 降采样两条路径：

| 样本 | 像素数 | 最大重心偏差(px) | 最大面积相对误差 | 最小外接框 IoU | 结果 |
| --- | ---: | ---: | ---: | ---: | --- |
| s01-color-blocks | 98,304 | 0.665 | 0.0621 | 0.9366 | PASS |
| s02-gradient | 307,200 | 0.146 | 0.0127 | 0.9756 | PASS |
| s03-low-contrast | 691,200 | 0.205 | 0.0036 | 0.9927 | PASS |
| s04-embedded-noise | 1,228,800 | 0.158 | 0.0039 | 0.9943 | PASS |
| s05-multi-overlap | 3,145,728 | 0.600 | 0.0048 | 0.9960 | PASS |

声明容差：

```text
centroid_px <= 2.0
area_relative <= 0.12
bbox_iou >= 0.80
```

组合偏差分数按分辨率递增顺序：

```text
[1.1667, 0.3012, 0.1693, 0.1400, 0.3598]
```

组合偏差未单调恶化；单项指标也不存在连续单调恶化。完整记录：

```text
workspace_data/exports/scale_experiment.json
```

## 5. 检查点恢复验收

同一命令再次运行五组样本：

| 样本 | 模式 | 恢复阶段 | 快照复用 | 耗时 |
| --- | --- | ---: | --- | ---: |
| s01-color-blocks | fine | 6/6 | 是 | 0.0129s |
| s02-gradient | fine | 18/18 | 是 | 0.0188s |
| s03-low-contrast | fine | 36/36 | 是 | 0.0405s |
| s04-embedded-noise | fine | 60/60 | 是 | 0.0667s |
| s05-multi-overlap | downsampled ×2 | 144/144 | 是 | 0.2149s |

第二次运行的最终掩码与首次逐像素一致。结果：

```text
workspace_data/exports/acceptance_rerun.json
```

自动化测试还验证了：

- 块 0 `cleanup` 注入失败时，块 1 仍正常提交；重试后块 0 从已冻结的 `growth` 继续并完成。
- 参数版本改变后，整个快照作废，恢复阶段数为 0，全量重算。
- 图像内容哈希改变后，整个快照作废。
- 任一阶段载荷哈希错误或阶段链不连续会触发整快照作废。
- 取消/强杀不承诺完整报告；只会依据已持久化阶段恢复。

## 6. 业务码核对

| 业务码 | 实际触发与断言 |
| --- | --- |
| `E_DECODE` | 清单缺失、图像损坏、真值缺失均返回业务码，并包含具体绝对文件路径 |
| `E_META` | 4 通道图像返回 `field=channels`；声明宽度错误返回 `field=width` |
| `E_PARA` | 参数或预算越界返回精确字段名；块载荷超缓冲返回 `budget.block_buffer_mb` |
| `E_REGION` | 区域数量不一致列出多发/缺失区域 ID 与面积 |
| `E_SCALE` | 超严容差列出区域编号、重心偏差、面积误差、外接框 IoU |
| `E_STATE` | 非法迁移和逆序阶段返回 `E_STATE`，原状态保持不变 |
| `E_BUDGET` | 0.01s / 1MB 预算下 2048×1536 样本返回 `E_BUDGET`，无静默超预算 |

## 7. 可视化验收

无显示器冒烟测试实际输出：

```text
samples 5 title ArtMask Workbench · 游戏美术掩码诊断
sample s01-color-blocks
mode 判定：fine · 倍率 1 · 块 256px
GUI_SMOKE_OK
```

工作台包含原图、生长初始掩码、最终叠加、逐区域偏差热力图、块 × 阶段状态矩阵、模式依据、暂停/取消/重试、故障注入与导出。

实际从 UI 触发一次 `s01-color-blocks` 运行，QThread 与核心流水线联调结果：

```text
complete True progress 6/6 rows 2
views True
UI_PIPELINE_OK
```

## 8. 已知边界

- 强制终止发生在块阶段原子替换与索引更新之间时，恢复扫描仍以各阶段文件的实际内容与内嵌哈希为准；索引不是唯一事实来源。
- 分块是处理、提交与恢复边界；全局区域生长先完成以消除块缝，块阶段任务及检查点写入仍按资源边界并行/串行执行。
- 自动种子依赖区域数量提示（来自样本清单）与颜色簇稳定性；手动种子入口可用于无提示场景。

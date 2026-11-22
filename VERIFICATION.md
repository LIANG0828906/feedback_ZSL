# 实际验证记录

验证日期：2026-11-23

## 环境

| 项目 | 实测值 |
|---|---|
| 平台 | WSL2 Ubuntu，Linux 6.18 |
| Python | 3.11.18 |
| pip | 26.3 |
| NumPy | 2.3.4 |
| PySide6 | 6.9.3 |
| 工作区 | `/home/liang/feedback/uid42a8691e-default-135` |

## 自动化测试

命令：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -v
```

结果：最新一轮 `Ran 19 tests in 8.611s`，`OK`。

覆盖项：

- PNG 灰度/RGB 往返与损坏输入 `E_DECODE`
- `E_PARA` 字段名、`E_BAND`、`E_STATE` 状态保持
- 重建尺寸/dtype/通道契约
- 右下边块像素计数与块级 SSE 精确核对
- 量化步长从 1 到 16 时 MSE 单调增、PSNR 单调降
- 分块 SSIM/边缘合并与整图计算一致
- 实时、暂停续跑、全量重算三路径逐像素一致
- `E_DIFF` 定位首像素、块号和阶段
- 未完成快照拒绝导出报告
- 量化变更时已提交块参数冻结
- 图像哈希变化时整体快照失效
- 算法版本变化时整体快照失效
- 块 payload 被篡改时整体快照失效
- 已提交后 `resume` 更换分块尺寸时返回 `E_PARA`
- 单块失败隔离与指定块重试
- 子进程强制退出后仅恢复已提交事务
- PySide6 离屏界面实例化

## CLI 验收矩阵

命令：

```bash
.venv/bin/python -m imq_workbench verify --output artifacts/verification_cli.json
```

结果：17 项检查全部通过，最新一轮耗时 8.8559 秒。

样本：`quick_mixed_rgb`，224×320×3。

参数：`block_size=16`、`quant_step=8`、`evaluation_window=7`、`workers=4`。

关键结果：

| 检查 | 结果 |
|---|---|
| 三路径 pixels_compared | 215,040 |
| 三路径 blocks_compared | 280 |
| 分块/整图最大指标差 | `1.7763568394002505e-15` |
| 暂停后已提交块 | 24 / 280，未完成 |
| 续跑恢复块数 | 24 |
| 续跑续处理块数 | 256 |
| PSNR | 43.2316868113589 dB |
| MSE | 3.089667038690478 |
| SSIM | 0.9897990782601551 |
| 边缘保真度 | 0.9952314114228997 |
| 真值边界像素 | 720 / 720 |

量化单调性：

| quant_step | MSE | PSNR |
|---:|---:|---:|
| 1 | 0.05366908482142859 | 60.833561712151635 |
| 2 | 0.25964936755952384 | 53.98693091767544 |
| 4 | 0.8554501488095235 | 48.80885654679577 |
| 8 | 3.089667038690478 | 43.2316868113589 |

异常码实测：

- `E_PARA`（`field=quant_step`）
- `E_PARA`（`field=block_size`）
- `E_BAND`（window=33，block=8，需要边带 16，最大 8）
- `E_STATE`（pending -> committed，状态保持 pending）
- `E_DECODE`（损坏 PNG）
- `E_DIFF`（首像素 `{y:1,x:1}`，块号 0，阶段 `reconstruction`）

## 数千像素样本实测

样本：`mixed_rgb_2048`，2048×1152×3，图元边界像素 5,944。

参数：`block_size=32`、`quant_step=8`、`evaluation_window=7`、`workers=4`、模式 `realtime`。

命令：

```bash
.venv/bin/python -m imq_workbench run \
  --samples-dir artifacts/samples \
  --sample mixed_rgb_2048 \
  --block-size 32 --quant-step 8 --eval-window 7 --workers 4 \
  --checkpoint artifacts/state/full.sqlite \
  --mode realtime \
  --output-dir artifacts/runs/large_2048 \
  --json
```

结果：

- 完成块：2304 / 2304
- 耗时：18.3362 秒
- PSNR：44.254190218905904 dB
- MSE：2.4415259184660747
- SSIM：0.9954099329371766
- 边缘保真度：0.9967045536015561
- 真值边界像素：5944 / 5944
- 邻接误差传播记录：4508 条
- 质量报告：`artifacts/runs/large_2048/quality_report.json`

随后以 `--mode resume` 重新加载同一快照：恢复 2304 块、处理 0 块、耗时 2.58 秒，四项指标与全量重算完全一致，且未触发快照失效。结果见 `artifacts/large_resume.json`。

## 强制终止边界

自动化测试使用子进程和 `--crash-after 5` 直接 `os._exit(137)`：

1. 退出后 SQLite 中只存在 0 < committed < total 的完整提交块。
2. 没有生成 `quality_report.json`。
3. 使用 `--mode resume` 后从已提交块恢复并完成。
4. 返回结果中 `resumed_blocks` 等于强制终止前提交数。

因此可复核边界为：已提交块可恢复；未提交块必须重算；未完成快照不得声称已生成最终质量报告。

## 生成样本

`python -m imq_workbench generate --output-dir artifacts/samples --preset full` 实测 1.02 秒生成：

| 样本 | 形状 (H×W×C) | 边界像素 |
|---|---:|---:|
| gradient_gray_512 | 384×512×1 | 428 |
| texture_rgb_768 | 512×768×3 | 1,356 |
| shapes_gray_1280 | 720×1280×1 | 3,158 |
| mixed_rgb_2048 | 1152×2048×3 | 5,944 |

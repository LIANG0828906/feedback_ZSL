# 实际验证记录

验证日期：2026-11-23  
样本种子：`20261123`  
算法版本：`vector-workbench.algorithm.v1`

实际环境：

```text
Python 3.14.4
NumPy 2.5.5
PySide6 6.12.0
WSL2 Ubuntu 26.04
```

## 自动化测试

执行：

```bash
PYTHONPATH=src .venv/bin/python -m unittest discover -v
```

结果：

```text
Ran 18 tests in 20.078s
OK
```

覆盖内容：

- 固定种子样本逐字节一致。
- `E_DECODE`、`E_GEOM` 路径/段上下文、样式范围。
- 扁平化 `E_PARA` 字段、闭合、无重复端点、容差单调性和真值面积/采样点。
- 任意子采样覆盖范围归一化、round/bevel/miter 与缓冲上限。
- 状态机非法迁移、前置阶段和前一路径 compose 门控、失败重试。
- 并行度 1 与 4 的逐像素和提交序列一致。
- 输入注入与参数注入、四类异常、阶段重试隔离。
- 快照参数/场景/算法版本失效、payload 损坏整体作废。
- 检查点越序写入拒绝。
- 三条执行路径逐像素一致。
- `E_DIFF` 首像素/路径/阶段定位。
- PySide6 离屏界面加载与状态表。
- 一次完整验收报告与 PNG 输出。

## 完整验收命令

```bash
PYTHONPATH=src .venv/bin/python -m vector_workbench verify \
  --data data \
  --artifacts artifacts \
  --checkpoints var/checkpoints/acceptance \
  --seed 20261123
```

结果：`passed=true`。报告位于 `artifacts/validation_report.json`。

## 三路径逐像素一致

| 场景 | 路径数 | 阶段数 | 位图尺寸 | SHA-256（float32 RGBA） |
|---|---:|---:|---|---|
| `hud_orbit` | 6 | 18 | 512×320 | `8cb003239c785d1b8821838b160e4e2b251cb09a6edb6b9b6ac6cc5a44c0d54e` |
| `badge_combo` | 5 | 15 | 480×340 | `db0ab56545af8de311ad43a96c6cbaafd52e46050c8a896dbbdd04d5b7fec04b` |
| `media_cards` | 7 | 21 | 560×340 | `e3e88d245c86cbf7acf0d3bd2b1ea8dd055223aeb024dafffbb20ab902925305` |

三份场景均满足：

```text
full status        = completed
incremental status = completed after recoverable cancel prefix
resume status      = completed after non-recoverable hard-stop prefix
commit sequences   = identical
exact_pixel_equal  = true
diff               = equal
```

取消前缀状态为 `cancelled/recoverable=true`；硬停止前缀状态为 `terminated/recoverable=false`。恢复后最终提交序列仍与全量一致。

## 异常注入

| 场景 | 注入路径 | 实际错误码 | 失败阶段 | 其他路径 | 重试 |
|---|---|---|---|---|---|
| `anomaly_self_intersection` | `self_star` | `E_SELF` | flatten | 全提交 | 几何仍自交，保持失败 |
| `anomaly_degenerate` | `zero_area_loop` | `E_DEGEN` | coverage | 全提交 | 几何仍零面积，保持失败 |
| `anomaly_stroke_clip` | `oversize_stroke` | `E_CLIP` | coverage | 全提交 | 描边仍超限，保持失败 |
| `anomaly_stage_failure` | `injected_stage` | `E_STAGE` | coverage | 全提交 | 禁用注入后完成 |

关闭注入字段后，前三类仍分别由真实几何/栅格检测得到相同错误码，说明异常链路可经输入或参数触发，不依赖源码改坏。

`anomaly_stage_failure` 重试结果：

```text
status = completed
flatten/coverage/compose for all paths = committed
```

失败期间及阶段故障重试后，其他路径的覆盖数组哈希保持不变；无其他路径单元被污染。

## 快照版本失效

实际结果：

```text
parameter version mismatch -> invalidated
scene content hash mismatch -> invalidated
algorithm version mismatch -> invalidated
payload corruption          -> invalidated
```

失效后进度为 0，重新计算得到与全量相同的提交序列和位图。

## 差异定位

构造单个覆盖像素差异后：

```json
{
  "equal": false,
  "first_pixel": [0, 0],
  "path_id": "orbit_fill",
  "stage": "coverage",
  "reason": "coverage values differ",
  "channel": 0
}
```

抛出错误码：`E_DIFF`。

## 输出产物

- `artifacts/hud_orbit_composite.png`
- `artifacts/badge_combo_composite.png`
- `artifacts/media_cards_composite.png`
- `artifacts/media_cards_cli.png`
- `artifacts/workbench_ui.png`
- `artifacts/validation_report.json`
- `artifacts/verify_summary.json`

检查点内容为运行中间产物，已在验收后清理；需要复现时重新执行 `verify` 即可在 `var/checkpoints/` 下生成版本化快照。


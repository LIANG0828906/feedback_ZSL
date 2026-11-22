# 实际验证记录

验证日期：2026-11-23  
工作目录：`/home/liang/feedback/uid42a8691e-default-134`

## 环境

```text
Python 3.11.18
torch 2.15.0+cpu
numpy 2.4.6
matplotlib 3.11.2
pytest 9.2.0
```

## 自动测试

命令：

```bash
.venv/bin/python -m pytest -q
```

结果：

```text
......................                                                   [100%]
25 passed
```

覆盖内容：

- 缺失字段、非有限值、奇异内参、真值标签不一致、目标损坏。
- 精确前向与目标逐元素一致。
- 球心、半径、反照率、光源强度梯度有限且非零。
- 精确/降采样/分块策略和非法参数。
- 降采样 MAE/RMSE 单调。
- 梯度裁剪、失效冻结、检查点恢复。
- 非法块序和快照漂移拒绝且状态保持。
- 缓冲预算阻塞、块失败重试。
- 内容哈希/配置版本不一致时全量重算。
- 精确与分块模式的三路径逐元素一致。
- 首个参数差异和 `E_DIFF` 定位。
- CLI 成功码和业务错误整数退出码。

## 完整演示

命令：

```bash
.venv/bin/python -m diffrender demo \
  --dataset data --out artifacts/demo --iterations 20
```

顶层结果：

```text
ok = true
```

单物体：

```text
mode = exact
loss_start = 0.0549471772824903
loss_end   = 0.01327434040879456
loss_reduction_ratio = 0.7584163361013156
realtime_vs_resume = true
realtime_vs_oneshot = true
```

多物体：

```text
mode = exact
loss_start = 0.03218666597875371
loss_end   = 0.010102139378627256
loss_reduction_ratio = 0.6861389935417468
realtime_vs_resume = true
realtime_vs_oneshot = true
```

两组的最终参数、完整损失序列、梯度历史和块提交序列均逐元素相等。

## 降采样差异

`compare_downsample_modes` 对单物体和多物体均验证：

```text
factor    single MAE       multi MAE
1         0.000000         0.000000
2         0.015362         0.019417
4         0.029607         0.036398
8         0.057345         0.076062
16        0.123330         0.143808
```

单物体 RMSE：`0 → 0.073181 → 0.106338 → 0.152372 → 0.272364`。  
多物体 RMSE：`0 → 0.079573 → 0.115448 → 0.179388 → 0.272584`。

聚合 MAE 和 RMSE 均随倍率非递减。逐像素 `max_abs` 保留在报告中，但在高动态边缘可能单独波动，因此不用于单调性验收。

## 梯度失效与恢复

注入：

```text
iterations = 2, 3 (consecutive)
label = sphere_0.radius
scale = 1e18
patience = 2
```

结果：

```text
error.code = E_GRAD
field_path = gradient
frozen_parameters = ["sphere_0.radius"]
failed_parameter_unchanged = true
other_parameter_updated = true
other_gradients_match_no_fault_injection_round = true
```

从失败前的窗口检查点重新启动且不再注入后，最终参数、损失、梯度历史和块序列与未中断参考运行逐元素一致：

```text
recovery_matches_uninterrupted_reference = true
```

## 块异常恢复

在 `iteration=0`、块序号 `1`、反向阶段注入一次 `E_BLOCK`：

```text
retries = 1
failed_attempts = 1
matches_no_fault_run = true
```

永久故障且 `max_block_retries=0` 时返回：

```text
code = E_BLOCK
field_path = fault_injection
```

## 错误码链路

演示实际命中：

```text
E_SCHEMA   field_path = cameras[0].intrinsics
E_DECODE   field_path = targets.multi_cam_front.path
E_PARA     field_path = shadow_samples
E_STATE    field_path = blocks.sequence
E_GRAD     field_path = gradient
E_BLOCK    field_path = fault_injection
```

非法块序提交后：

```text
committed_count = 0
records = []
state preserved = true
```

对运行报告人为加入 `1e-9` 的损失差异后，`diffrender compare` 返回整数退出码 `7`，`stderr` 中为 `E_DIFF`，首个差异定位到 `loss_history[0]`。

## 产物

- `artifacts/demo/demo_summary.json`
- `artifacts/demo/single_three_paths/three_path_report.json`
- `artifacts/demo/multi_three_paths/three_path_report.json`
- `artifacts/demo/*_three_paths/realtime_diagnostics/comparison.png`
- `artifacts/demo/*_three_paths/realtime_diagnostics/dashboard.html`
- `artifacts/demo/*_three_paths/realtime_diagnostics/parameter_table.png`
- `artifacts/demo/*_three_paths/realtime_diagnostics/loss_curve.png`
- `artifacts/demo/*_three_paths/realtime_diagnostics/gradient_curve.png`
- `artifacts/demo/*_three_paths/realtime_diagnostics/block_times.png`


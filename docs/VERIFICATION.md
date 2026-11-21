# 实际验证记录

## 环境

- Python `3.12.16`
- NumPy `2.3.5`
- Tkinter `9.0`（使用提供环境中的 Python 3.12）
- 纯本地 CPU；无 GPU、无容器、无外部服务
- 系统 Python 3.14 缺少 Tkinter，因此项目使用 `.venv` 中的 Python 3.12

## 自动化测试

执行：

```bash
.venv/bin/python -m pytest -q
```

结果：`32 passed`。

覆盖内容：

- 二进制往返、校验和/截断、非有限矩阵、密度索引越界、部分介质重叠、缺失字段。
- 12000 预算下 `[1,4]` 块从 `full` 降级到 `balanced`，实际用量等于预检量。
- 100 预算下 6 块全部进入等待并拒绝，`complete_events_before_reject == 0`。
- 整屏真值接缝检查为 0 偏差；主动篡改边界像素可定位 `E_SEAM_UNRESOLVED`。
- 浮点/定点重复渲染颜色缓冲与摘要逐字节一致。
- 双路径误差全部低于逐像素上界。
- 快照强停恢复、哈希链篡改、字段缺失。
- 增量只重渲受影响块、未受影响哈希复用、发布期间查询旧/新完整版本、并发提交 `E_BUSY`。
- 五类注入独立业务码，且不创建输出目录。

GUI 冒烟：

```bash
VOLRENDER_GUI_AUTOCLOSE_MS=250 .venv/bin/python -m volrender.cli render \
  --scene artifacts/demo/sample/scene.volbin \
  --config artifacts/demo/sample/render_config.json \
  --out /tmp/volrender-gui-smoke --mode float64 --gui
```

窗口成功创建并自动关闭，进程退出码为 0。

## 端到端验收

执行：

```bash
.venv/bin/python scripts/verify_demo.py
```

机器可读报告：`artifacts/verification/report.json`

预览目录：`artifacts/demo/preview/current`

预览图：`artifacts/demo/preview/current/preview.png`

本次实测结果：

| 项目 | 结果 |
|---|---|
| 屏幕/分块 | 48x32，6 块，16x16 |
| 单块预算 | 12000 |
| 降级块 | `[1, 4]`，阶段 `balanced`，步长倍率 1.45 |
| 实际总采样 | 34215 |
| 分块阶段用量 | `[3900, 9340, 3890, 3885, 9300, 3900]` |
| 浮点接缝最大颜色/透射率偏差 | `0.0 / 0.0` |
| 定点接缝最大颜色/透射率偏差 | `0.0 / 0.0` |
| 定点最大颜色绝对误差 | `8.561e-06` |
| 定点最大透射率绝对误差 | `2.793e-06` |
| 最大保守误差上界 | `2.087e-02` |
| 低于上界像素 | `1536 / 1536` |
| 两次渲染摘要 | 逐字节一致 |
| 两次渲染浮点/定点颜色 | 均逐字节一致 |
| 超预算拒绝 | `E_BUDGET_EXCEEDED`，0 个已完成块，无输出 |
| 强停 | 2 块后终止 |
| 恢复 | 跳过 `[0,1]`，重算 `[2,3,4,5]`，颜色与摘要等于完整渲染 |
| 重复写入 | 无 |
| 增量变化 | 密度块 0 |
| 增量重渲 | `[0,1,3,4]` |
| 增量复用 | `[2,5]`，内容哈希完全一致 |
| 注入业务码 | 5 类均不同且均带注入点 |

样本指纹：

```text
scene_sha256       8969ad51a568ad349bcc37260468bc17091601bc0ba700fe8a1be17a90725944
scene_fingerprint  f6e212f00240bfc430169223e93b5bed92dca6082e5197bc6ec737ba1ea69f29
density_version    1fbe2770196d23a4332aa243a6124a260d29a67b134dd2083fc1a3ecffcebb8f
```

版本目录 ID（摘要哈希前 20 位）：

```text
f10ffc8ec10fefc140e9
```

完整预览摘要 SHA-256：

```text
f10ffc8ec10fefc140e9efa5bf6ea02942dc2a863e378ebf2b6a6fea58ebbf5d
```

## 验收映射

1. 场景解析与校验：`tests/test_scene_validation.py`。
2. 分块调度与稳定顺序：`tests/test_budget_seams.py`，轨迹中 `submit` 的 `seq` 单调。
3. 介质步进、预算降级与拒绝：`tests/test_budget_seams.py`。
4. 接缝真值比较：正常为 0 偏差；篡改用例返回 `E_SEAM_UNRESOLVED` 并含两侧值。
5. 双路径与摘要：`tests/test_precision_determinism.py`，重复输出逐字节比较。
6. 增量与原子发布：`tests/test_incremental_and_busy.py`。
7. 异常注入：`tests/test_injection_cli.py`，五类独立码且无输出。
8. 快照恢复与 GUI：`tests/test_snapshot_recovery.py`；GUI 代码位于 `volrender/gui.py`，需桌面显示环境运行。

## 可复现性说明

- 样本生成器不使用随机数。
- 摘要 JSON 使用 `sort_keys=True` 和紧凑分隔符。
- 颜色缓冲为固定 `<f8` 小端序。
- 线程只决定块由哪个线程计算，不改变块内像素/步进顺序。
- 快照和输出版本最终文件内容不含时间戳、PID 或绝对路径。
- Tkinter 9.0 已可导入，并通过 `VOLRENDER_GUI_AUTOCLOSE_MS` 实际启动/关闭一次窗口完成冒烟验证；未保存窗口截图。

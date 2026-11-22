# 实际验证记录

验证时间：2026-11-23（Asia/Shanghai）

## 环境

| 项 | 值 |
|---|---|
| WSL2 平台 | `Linux-6.18.33.1-microsoft-standard-WSL2-x86_64` |
| Linux Python | 3.14.4 |
| NumPy | 2.5.5 |
| Pillow | 13.0.0 |
| PySide6 | 6.12.0 |
| pip | 26.3，可用 |
| Windows Python | 3.10.9 |
| Windows NumPy / Pillow / PySide6 | 2.2.6 / 12.2.0 / 6.12.0 |

未使用 Docker、容器、网络服务或外部目标。

## 自动化测试

命令：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest -q
```

结果：

```text
30 passed
```

覆盖范围包括：

- 样本重复生成哈希一致、几何真值可解析；
- 8 组越界/未知/NaN 参数均返回 `E_PARA` 且包含阶段与参数名；
- 4 种模式尺寸、通道数、dtype 保持；
- 按通道分别处理后重新堆叠与整图处理逐像素一致；
- 阶段失效边界、上游对象保持、下游删除；
- 增量路径对空缓存全量路径逐像素一致；
- 注入首像素差异返回 `E_DIFF` 并定位阶段/坐标/通道；
- 快照完整恢复、参数不匹配整体作废、阶段版本不匹配整体作废；
- 输入变化整体失效、损坏快照删除、损坏 PNG 返回 `E_DECODE`；
- 模拟 `os.replace` 失败时旧快照字节不变；
- 取消后前缀快照恢复；
- 独立进程强制终止后前缀恢复；
- 单阶段失败不污染上游、可重试；
- 内存上限返回 `E_MEMORY`；
- PySide6 offscreen 窗口、增量重算、缓存表和全量验算。

## 端到端验收

命令：

```bash
.venv/bin/python scripts/verify_acceptance.py \
  --samples samples \
  --report artifacts/acceptance_report.json \
  --screenshot artifacts/ui_smoke.png
```

结果：

```text
acceptance: 10 passed, 0 failed
[PASS] sample:forest_gray_320x240.png
[PASS] sample:spark_la_480x270.png
[PASS] sample:neon_rgb_640x360.png
[PASS] sample:portrait_rgba_512x512.png
[PASS] errors-and-parameter-invalidation
[PASS] cancel-failure-retry
[PASS] hard-process-exit-recovery
[PASS] e-diff-first-pixel
[PASS] input-change-invalidates-all
[PASS] pyside6-offscreen-smoke
```

### 样本与增量统计

| 样本 | 模式 | 基线重算 | 仅改局部对比度 | 改锐化及下游 | 重启恢复 |
|---|---|---:|---|---|---|
| forest 320×240 | L | 3 | 重算 1 / 命中 2 | 重算 2 / 命中 1 | 重算 0 / 命中 3 |
| spark 480×270 | LA | 3 | 重算 1 / 命中 2 | 重算 2 / 命中 1 | 重算 0 / 命中 3 |
| neon 640×360 | RGB | 3 | 重算 1 / 命中 2 | 重算 2 / 命中 1 | 重算 0 / 命中 3 |
| portrait 512×512 | RGBA | 3 | 重算 1 / 命中 2 | 重算 2 / 命中 1 | 重算 0 / 命中 3 |

四组样本在基线、末端增量、中间阶段增量、快照恢复四条路径上均与空缓存
全量逐像素一致；有效缓存条目均为 3，不大于全量阶段数 3。

### 中断与失败

| 场景 | 实际结果 |
|---|---|
| 用户取消 | 状态 `cancelled`，已提交 `denoise` 前缀并持久化 |
| 取消后恢复 | 部分快照恢复，命中 1、重算 2，最终与全量一致 |
| 强制终止 | 子进程在首阶段提交后 `os._exit(37)` |
| 强制终止后快照 | `chain_complete=false`，仅 `["denoise"]` |
| 强制终止后恢复 | 恢复前 `last_run=None`；恢复后命中 1、重算 2，结果一致 |
| 阶段失败 | `sharpen` 返回 `E_STAGE`，仅 `denoise` 缓存保留 |
| 失败重试 | 命中 1、重算 2，最终与全量一致 |

强制终止路径明确显示“不对强制终止前的完整报告作任何声明”，没有把父进程
恢复后的新报告冒充为旧进程报告。

### E_DIFF

向 `local_contrast` 第一次输出写入 1 个 LSB 差异后，自动全量比对返回：

```json
{
  "code": "E_DIFF",
  "stage": "local_contrast",
  "x": 0,
  "y": 0,
  "channel": 0
}
```

### UI

- WSL offscreen 冒烟通过，截图：`artifacts/ui_smoke.png`。
- Windows 原生冒烟通过：

```text
platform=windows
python=3.10.9
run_status=complete
cache_entries=3
screenshot=artifacts/windows_ui.png
```

Windows 截图：`artifacts/windows_ui.png`。窗口实际包含原图/增强对照、
差异热力图、参数面板和运行控制。

## 机器可读证据

- `artifacts/acceptance_report.json`：环境、逐样本统计、错误码、中断、
  强制终止、E_DIFF、输入失效、UI 冒烟与通过/失败汇总。
- `artifacts/ui_smoke.png`：Linux/WSL offscreen 渲染。
- `artifacts/ui_dependency.png`：依赖图与缓存表。
- `artifacts/ui_diagnostics.png`：诊断统计面板。
- `artifacts/windows_ui.png`：Windows 原生 Qt 平台渲染。

## 复现步骤

```bash
source .venv/bin/activate
python -m pip install -e ".[test]"
python scripts/generate_samples.py samples
QT_QPA_PLATFORM=offscreen python -m pytest -q
python scripts/verify_acceptance.py
python -m filter_studio --samples samples --cache .cache
```

Windows：

```powershell
.\.venv\Scripts\Activate.ps1
python -m pip install -e ".[test]"
python scripts\windows_smoke.py
python -m filter_studio
```

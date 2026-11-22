# SpectraForge 实际验证记录

验证日期：2026-11-23（环境时钟）

## 环境

| 项目 | 实际值 |
|---|---|
| 操作系统 | WSL2，Ubuntu 26.04，Linux 6.18 |
| Python | 3.14.4 |
| NumPy | 2.5.5 |
| PySide6 | 6.12.0 |
| Qt 平台 | `offscreen`（界面烟雾测试与截图） |
| 容器 | 未使用 Docker 或其他容器 |
| 网络 | 运行与验收阶段不访问网络、不使用外部数据集 |

说明：本执行环境是 Windows 主机上的 WSL2，不是原生 Windows 终端；因此没有直接声称在 Windows
解释器中完成运行。代码使用跨平台路径、原子替换与 `msvcrt`/`fcntl` 双实现，并提供
`setup_windows.ps1` 与 PowerShell 步骤。实际可执行验证发生在上述 WSL2 环境。

## 一键验收

运行：

```bash
QT_QPA_PLATFORM=offscreen python verify_acceptance.py --workspace artifacts/acceptance
```

结果：

```json
{
  "ok": true,
  "summary": "artifacts/acceptance/acceptance_summary.json"
}
```

四条验收命令均返回 `0`：

1. 23 项 unittest 全部通过；
2. 三路径验证通过；
3. 场景、解析真值与配准检查输出；
4. 错误码链路全部命中。

## 自动化测试

命令：

```bash
QT_QPA_PLATFORM=offscreen python -W error::ResourceWarning -m unittest discover -s tests -v
```

实际结果：

```text
Ran 23 tests in 1.343s

OK
```

以 `ResourceWarning` 作为错误运行时仍全部通过，无未关闭文件或句柄警告。

关键测试：

- `test_deterministic_generation`
- `test_fault_samples_have_stable_codes`
- `test_analytical_color_truth_and_peak`
- `test_nested_exposure_subset_peak_is_monotonic`
- `test_committed_parameter_buffers_are_locked`
- `test_one_corrupt_segment_invalidates_whole_snapshot`
- `test_one_failed_segment_can_be_retried_without_polluting_others`
- `test_strict_gamut_failure_reports_global_pixel_and_channel`
- `test_forced_termination_recovers_only_committed_segments`
- `test_buffer_pool_blocks_instead_of_dropping`
- `test_per_thread_buffer_limit_blocks_segment_start`
- `test_three_path_outputs_are_pixel_exact`
- `test_band_segmented_path_is_also_pixel_exact`
- `test_comparison_detects_one_ulp_and_reports_segment_stage`
- `test_peak_and_gamut_set_are_compared_exactly`
- `test_export_is_complete_and_ordered`
- `test_offscreen_ui_loads_processes_and_renders`

## 三路径一致性

场景：`neon_forge_424242`，8 波段，6 曝光，128×96，32×32 像块，共 12 段。

| 路径 | 峰值辐亮度 | 色域越界像素 | 输出哈希 | 用时 |
|---|---:|---:|---|---:|
| 实时，4 workers | 35.66048049926758 | 1075 | `sha256:5bbbc5ce…d4b3e5d5c` | 0.1077 s |
| 断点恢复，先暂停 8 段已提交 | 35.66048049926758 | 1075 | `sha256:5bbbc5ce…d4b3e5d5c` | 0.1187 s |
| 全量重算，1 worker | 35.66048049926758 | 1075 | `sha256:5bbbc5ce…d4b3e5d5c` | 0.1000 s |

比较结果：

```text
live_vs_resume = true
live_vs_full   = true
resume_vs_full = true
```

比较阶段：

```text
radiance, rgb_chroma, linear_rgb, display, gamut_channel_mask,
peak_radiance, gamut_set
```

未使用容差比较。额外测试把 `linear_rgb[19,71,2]` 增加一个 float32 ULP，比较器正确定位：

```text
stage=linear_rgb, x=71, y=19, segment_id=2
```

报告：`artifacts/acceptance/three_path_report.json`

## 解析色块真值

8 个色块全部在声明容差内：

```text
declared tolerance = 0.045
max absolute RGB error = 0.0028379474
all patches within tolerance = true
```

峰值辐亮度：

```text
analytical peak = 35.7550789789
fused peak      = 35.6604804993
relative error  = 0.0026457354
```

## 曝光子集单调性

从只有最长曝光开始，逐步加入更短曝光，实际峰值（测试样本）保持非递减，并收敛到解析峰值。
该性质由嵌套子集测试保证；使用 sRGB 默认参数与完整 6 档曝光时的最终峰值见上表。

## 检查点与强制终止

### 参数锁定

先用参数 A 提交若干段，再把 `weight_power`、`gamut_intent`、`tone_key` 改为参数 B 并继续：

```text
committed segment file bytes unchanged = true
committed param_version unchanged      = true
parameter_snapshots >= 2               = true
```

### 单段失败与重试

向分段 1 注入异常：

```text
failed = 1
committed = total - 1
assemble(require_complete=True) -> E_INCOMPLETE
retry -> committed = total
```

其他分段输出不受污染。

### 单段损坏导致整快照作废

翻转一个分段 NPZ 的字节后重启：

```text
invalidated_reason.code = E_STATE
committed = 0
pending = total
archived snapshots = 1
```

不存在“部分旧段 + 部分新段”的混用。

### 子进程硬终止

测试在独立 Python 子进程中处理 4 个分段，并在第 2 次提交后执行 `os._exit(86)`：

```text
subprocess exit code = 86
recovered committed = 2
recovered pending = 2
assemble full -> E_INCOMPLETE
resume -> 4/4 committed
```

恢复到的是检查点实际持久化的 2 段，而不是重算整段。

## 错误码链路

命令：

```bash
QT_QPA_PLATFORM=offscreen python -m spectraforge check-errors --workspace artifacts/acceptance/errors
```

实际结果：

| 样本 / 场景 | 期望 | 实际 | 关键详情 |
|---|---|---|---|
| 缺少 `scene.json` | `E_META` | `E_META` | 缺失路径 |
| 元数据宽度 130，图像宽度 128 | `E_DIM` | `E_DIM` | expected `[96,130]` actual `[96,128]` |
| 波段载荷篡改 | `E_DECODE` | `E_DECODE` | payload checksum mismatch |
| `weight_power=99` | `E_PARA` | `E_PARA` | field `weight_power` |
| `PENDING -> COMMITTED` | `E_STATE` | `E_STATE` | 原状态仍为 `pending` |
| 负 R 通道色度 | `E_GAMUT` | `E_GAMUT` | `x=0, y=0, channel=R` |
| 单像素显示差异 | `E_DIVERGE` 诊断 | `E_DIVERGE` 诊断 | stage `display`, `(0,0)`, segment `0` |

完整输出：

- `artifacts/acceptance/error_codes.json`
- `artifacts/acceptance/scene_inspection.json`
- `artifacts/acceptance/acceptance_summary.json`

## 导出

命令：

```bash
python -m spectraforge process --scene neon_forge_424242 --fresh \
  --workspace /tmp/sf_export_runtime --export --export-dir artifacts/export_sample
```

实际输出：

| 文件 | 字节 |
|---|---:|
| `display.png` | 22,771 |
| `aligned_bands.npy` | 393,344 |
| `alignment_report.json` | 689 |
| `radiance.npy` | 49,280 |
| `linear_rgb.npy` | 147,584 |
| `rgb_chroma.npy` | 147,584 |
| `gamut_channel_mask.npy` | 36,992 |
| `stats.json` | 20,509 |
| `export_manifest.json` | 1,178 |

导出清单记录每个文件的 SHA-256、稳定分段顺序、参数版本、元数据与算法版本。
不完整快照会在 `assemble` 阶段返回 `E_INCOMPLETE`，不会生成完整导出声明。

## UI 渲染

离屏实际处理并渲染：

```bash
QT_QPA_PLATFORM=offscreen python capture_ui_demo.py
```

结果：

```text
committed = 12 / 12
complete = true
peak_radiance = 35.66048049926758
gamut_pixels = 1075
comparison_exact = true
```

截图：

- `artifacts/ui_workbench.png`：合成、分段表、直方图与色调曲线；
- `artifacts/ui_band_array.png`：8 波段与 HDR 概览；
- `artifacts/ui_difference.png`：全量重算差异热力图与 `EXACT MATCH`。

## 结论

在本机固定种子样本上，以下验收项均通过：

- 多光谱配准、HDR 曝光融合与色度重建；
- 色块解析真值与峰值误差；
- 实时、断点恢复、全量重算逐像素一致；
- 检查点版本/哈希整体校验、参数锁定、单段失败隔离与强制终止恢复；
- `E_META`、`E_DIM`、`E_DECODE`、`E_PARA`、`E_STATE`、`E_GAMUT`、`E_DIVERGE` 链路；
- PySide6 专业界面交互与确定性导出。

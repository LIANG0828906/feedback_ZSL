# 实际验证记录

- 生成时间（UTC）：`2026-11-22T22:04:13.074626+00:00`
- Python：`3.11.18 (main, Nov 11 2026, 00:16:37) [Clang 23.1.3 ]`
- 自动化测试：`OK`
- 运行环境：本地 CPU，无网络、GPU 或外部服务。

## 三路径逐像素一致性

| 场景 | full vs realtime | full vs resume | realtime vs resume | 残留标准差 | 边缘保真度 | 越界像素 |
|---|---:|---:|---:|---:|---:|---:|
| scene_00_neon_dusk | True | True | True | 0.01022557 | 0.86852067 | 1034 |
| scene_01_forest_studio | True | True | True | 0.00914869 | 0.87015670 | 1093 |
| scene_02_spectral_lab | True | True | True | 0.00917659 | 0.85312448 | 1114 |

比对采用 `np.array_equal` 严格相等，不设浮点容差；首个差异会定位到像素坐标、分段编号与字段名。

## 异常注入链路

| 注入 | 预期错误码 | 实际错误码 | 结果 |
|---|---|---|---|
| missing_band | E_META | E_META | 通过 |
| exposure_order | E_ORDER | E_ORDER | 通过 |
| bit_depth_change | E_VER | E_VER | 通过 |
| bad_segment | E_SEG | E_SEG | 通过，随后断点重试 |

## 强制终止恢复

- 第 4 段提交后以退出码 `91` 强制终止：91
- 断点恢复完成并输出：12/12
- 恢复结果与 full 路径严格比较：True
- 坏段重试结果与 full 路径严格比较：True

## 证据文件

- `outputs/acceptance/acceptance_report.json`
- `outputs/paths/paths_report.json`
- `outputs/paths/comparisons/*/*.json`
- `outputs/paths/full/*/stats.json`
- `outputs/acceptance/bad_segment_comparison.json`
- `outputs/acceptance/forced_resume_comparison.json`

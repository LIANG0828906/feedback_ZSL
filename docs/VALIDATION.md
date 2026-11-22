# 实际验证记录

- 生成时间（UTC）：`2026-11-22T16:53:39.734349+00:00`
- 算法版本：`edgepipe-1.0.0`
- Python：`3.11.18 (main, Nov 11 2026, 00:16:37) [Clang 23.1.3 ]`
- NumPy/OpenCV：`2.4.6` / `5.1.0`
- 总体结果：**通过**

## 样本验收矩阵

| 样本 | 三路径逐像素 | 分块/整图阶段一致 | 提交序列一致 | 几何真值 | 检查点失效 | 分块×阶段 | 有效边带 |
|---|---|---|---|---|---|---|---|
| `gray_lines_lowcontrast_96x128` | 是 | 是 | 是 | 通过（最大位移 0.520px） | 是 | 30 | 9px |
| `gray_noisy_131x97` | 是 | 是 | 是 | 通过（最大位移 0.365px） | 是 | 45 | 9px |
| `color_shapes_120x160` | 是 | 是 | 是 | 通过（最大位移 0.540px） | 是 | 60 | 9px |
| `color_lowcontrast_150x110` | 是 | 是 | 是 | 通过（最大位移 0.863px） | 是 | 60 | 9px |

## 三路径运行摘要

| 样本 | 路径 | 复用提交 | 新提交 | 耗时(s) |
|---|---|---:|---:|---:|
| `gray_lines_lowcontrast_96x128` | `full` | 0 | 30 | 0.262492 |
| `gray_lines_lowcontrast_96x128` | `incremental` | 7 | 23 | 0.154566 |
| `gray_lines_lowcontrast_96x128` | `resume` | 7 | 23 | 0.137488 |
| `gray_noisy_131x97` | `full` | 0 | 45 | 0.242408 |
| `gray_noisy_131x97` | `incremental` | 7 | 38 | 0.236584 |
| `gray_noisy_131x97` | `resume` | 7 | 38 | 0.214500 |
| `color_shapes_120x160` | `full` | 0 | 60 | 0.469949 |
| `color_shapes_120x160` | `incremental` | 7 | 53 | 0.435015 |
| `color_shapes_120x160` | `resume` | 7 | 53 | 0.432731 |
| `color_lowcontrast_150x110` | `full` | 0 | 60 | 0.532868 |
| `color_lowcontrast_150x110` | `incremental` | 7 | 53 | 0.526463 |
| `color_lowcontrast_150x110` | `resume` | 7 | 53 | 0.579130 |

## 几何真值统计

| 样本 | 掩膜像素 | 最大系统位移(px) | 最大圆半径误差(px) | 覆盖/位移/圆拟合 |
|---|---:|---:|---:|---|
| `gray_lines_lowcontrast_96x128` | 600 | 0.519879 | 0.007249 | True/True/True |
| `gray_noisy_131x97` | 563 | 0.365242 | 0.044475 | True/True/True |
| `color_shapes_120x160` | 970 | 0.540362 | 0.048532 | True/True/True |
| `color_lowcontrast_150x110` | 1951 | 0.863400 | 1.743957 | True/True/True |

## 异常码实测

| 检查 | 错误码 | 字段 | 说明 |
|---|---|---|---|
| `E_PARA_threshold_order` | `E_PARA` | `low_threshold` | low_threshold 不得大于 high_threshold |
| `E_PARA_threshold_range` | `E_PARA` | `high_threshold` | high_threshold 必须在 [0, 4096] 范围内 |
| `E_BAND_insufficient` | `E_BAND` | `band_width` | band_width=1 小于确定性边界所需值 7 |
| `E_STATE_illegal_transition` | `E_STATE` | `state` | 非法状态迁移 PENDING -> COMMITTED |
| `E_DECODE_corrupt_image` | `E_DECODE` | `path` | 图像解码失败或内容损坏: artifacts/validation_work/error-code-temp/broken.png |
| `E_DIFF_first_pixel` | `E_DIFF` | `mask` | 掩膜存在逐像素差异 |

## 说明

- 三路径比较覆盖五个阶段掩膜、最终掩膜、几何统计、提交序列与状态矩阵。
- 断点恢复路径在写入指定数量原子提交后抛出模拟崩溃，再由新引擎实例恢复。
- 检查点验收复制完整快照后分别注入参数版本、算法版本、输入哈希和分块内容哈希失配；四种情形均被整体判废。
- 分块边界验收将每阶段分块合并结果与整图单块结果逐像素比较，未裁掉任何边界像素。

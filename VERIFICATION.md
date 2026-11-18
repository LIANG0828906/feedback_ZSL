# 实际验证记录

验证环境：Linux 工作区、Python 3.11.18、NumPy 2.4.6、Pillow 13.0.0。全部样本由 `canopy_render.scene.generate_scene()` 生成。

## 自动化测试

```text
python -m unittest discover -s tests -v
Ran 25 tests
OK
```

覆盖项：

- 确定性场景与材质表逐字节一致，层/褶皱/三角形/流苏/灯列表重排不改变场景摘要和像素。
- 索引越界、退化三角形、透光率越界、重复层号、重复褶皱号、灯贴面与灯在闭合织物体积内均整体拒绝；错误含层号、褶皱号与业务码。
- 背光织物最大 RGB 分量低于 246；全织物与最暗四分位均有可见亮度方差，褶皱深处纹样未被白化。
- 关闭种子法线扰动后像素发生变化，确认褶皱凹凸通过扰动法线参与明暗调制。
- 层厚与入射角共同影响透射；逐层透射/反射能量占比可复核。
- 流苏投影位置随灯位和悬挂高度改变，并可由报告坐标与 JSON 中灯/壁参数复算。
- 三组流苏的壁距增大时，10%–90% 软边宽度严格增大；同一输入两次测量逐值一致。
- 硬杀后仅用已提交快照续算，RGBA、归属图、织物复合图、逐层图与投影坐标和完整运行逐字节一致。
- 损坏快照与场景摘要不匹配快照被整体丢弃；续算统计、图像摘要和恢复次数稳定。
- 人为修改一个像素后，`compare` 写出分块号、像素坐标与通道差异并以退出码 3 结束。

## 完整样本指标

画布 `192 x 160`，分块 `64 x 40`，共 `3 x 4 = 12` 块；3 层织物、6 个流苏、3 盏灯。

```text
soft shadow transition widths (px):
  tassel_near_left  distance 0.4975 -> 1.95
  tassel_mid_left   distance 0.9775 -> 4.60
  tassel_far_left   distance 1.4575 -> 10.475
```

上述宽度趋势由 `tests/test_render.py::test_soft_shadow_width_grows_with_wall_distance` 强制检查。每盏灯 9 个灯盘样本；12 块共执行 `442,638` 条壁面可见性采样射线（实际值写入报告 `soft_shadows.soft_shadow_sample_count`）。

## 恢复一致性

硬杀测试在第 4 块提交后执行不可捕获退出：

```text
snapshot.completed_tile_indices = [0, 1, 2, 3]
hard-kill exit code = 137
restart: snapshot_status = resumed, recovery_count = 1
render.png / ownership.png / fabric_composite.png 与参考逐字节一致
projection.points、image_summary、layers、soft_shadow_sample_count 与参考一致
```

完整机器可读结果与输出图像位于 `artifacts/canopy_render_validation/`。


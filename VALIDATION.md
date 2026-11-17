# 实际验证记录

验证日期：2026-11-18  
验证环境：Ubuntu 26.04 / Python 3.14.4 / NumPy 2.5.5 / Pillow 13.0.0  
代码约束：Python >= 3.10，无容器、无网络数据、无外部纹理。

## 1. 自动化测试

```bash
.venv/bin/python -m unittest discover -s tests -v
```

结果：

```text
Ran 33 tests in 10.991s
OK
```

覆盖内容：

- 层号重复/缺失、编号重复、索引越界、退化三角形；
- 透光率/反射率越界、方向光非单位向量/平行层平面；
- 相机和地面参数非法；
- CLI 校验错误退出码 `2`，且未创建渲染输出；
- 纹理同参数逐字节一致、遍历顺序无关、不同种子可区分、同时调制透光与染色；
- 投影落点共面/共地、层间距变化导致落点偏移；
- 半影宽度对接收距离和光源距离单调增加；
- 软边单调、无单步硬边，纱绢阴影相邻像素最大跳变受限；
- 越界地面投影无遮挡；
- 完整输出结构、无惨白过曝、跨层染色、层 JSON 顺序无关；
- 两次运行 RGBA/PNG/报告/投影逐字节一致；
- Ctrl+C 后当前块提交、退出码 0、续算与基线一致；
- SIGKILL 后只复用已提交块，续算与基线一致；
- 损坏快照整体丢弃、场景摘要变化整体丢弃；
- 差异报告包含块号、像素坐标和通道，比较命令退出码 `4`。

## 2. 样本场景

```bash
.venv/bin/python render.py sample --output examples/garden_door.json
.venv/bin/python render.py render \
  --scene examples/garden_door.json \
  --out artifacts/garden_door_demo --fresh
```

输出：

```text
status=complete
completed_blocks=20
tile_count=20
scene_digest=411b343fc1ff25a173ed07671f7297da709db2dd0e359148bf878ba0b55f2ef1
```

主要哈希：

| 内容 | SHA-256 |
| --- | --- |
| 规范化场景 | `411b343fc1ff25a173ed07671f7297da709db2dd0e359148bf878ba0b55f2ef1` |
| RGBA 原始像素 | `d81408a29bf35cfeb5176582e9528a69e8ff06d1aec6925899f6dda3e60c4cea` |
| `render.png` 文件 | `fbca6fcee1171b552bcff488f1d3ede7054c543043afb147a4d2264c85235c5e` |
| 层归属原始数组 | `da89ea89e189053b819b2d28773582f13b29218190d9d4bb4a92853dc3eae58e` |
| `projection_points.json` | `468edaa152328a24372d88a9ef5a21a3ea5770e64e07d532749afa5278f1b5e7` |
| `report.json` | `45c0a4b60116d58a1e66590fb26871b1618547663660b031a0f291d7ffb15b74` |

图像/层统计：

```text
尺寸: 320 x 240
分块: 5 x 4，tile_size=64
纱绢/表面区域 RGB 最大通道值: 211
表面区域 RGB 三通道均 > 250 的像素: 0
表面区域平均色度 max(R,G,B)-min(R,G,B): 48.80
Alpha 模式: display-space premultiplied RGBA
```

每层可见片元、平均透光率、透射/反射贡献：

| 层号 | ID | 类型 | 可见片元 | 平均透光率 | 透射占比 | 反射占比 |
| ---: | --- | --- | ---: | ---: | ---: | ---: |
| 0 | `silk_inner` | silk | 12856 | 0.536543 | 0.658944 | 0.341056 |
| 1 | `lattice_inner` | mullion | 1675 | 0 | 0 | 1 |
| 2 | `silk_middle` | silk | 13799 | 0.598368 | 0.768428 | 0.231572 |
| 3 | `lattice_outer` | mullion | 1796 | 0 | 0 | 1 |
| 4 | `silk_outer` | silk | 14851 | 0.520447 | 0.734016 | 0.265984 |

投影/半影：

```text
projection_case_count = 4
projected_triangle_count = 64
in_bounds_triangle_count = 12
edge_profile_sample_points = 256
silk_shadow_receiver_sample_points = 83012
ground_shadow_receiver_sample_points = 62988
penumbra_widths = [0.287498, 1.2, 0.284520, 1.2]
edge_transition_widths_10_90 = [0.177975, 0.742857, 0.176132, 0.742857]
```

接收距离趋势（固定光源距离约 11.61）：

```text
0.03 -> 0.039477
0.06 -> 0.078955
0.12 -> 0.157910
```

光源距离趋势（固定接收距离 0.06）：

```text
6.0  -> 0.064000
12.0 -> 0.080000
24.0 -> 0.112000
```

纱绢层阴影相邻像素最大跳变：

```text
silk_inner   max 0.13319, p99 0.10851
silk_middle  max 0.12188, p99 0.10969
silk_outer   无本层接收阴影
```

未修正前的样本配置中，单像素阴影因子可跳变 0.78；扩大可解析层间距并提高采样密度后，已降为有多个像素过渡的 0.13 量级。

## 3. 重复运行一致性

```bash
.venv/bin/python render.py render \
  --scene examples/garden_door.json \
  --out /tmp/serene_final_b_2 --fresh
.venv/bin/python render.py compare \
  --reference artifacts/garden_door_demo \
  --candidate /tmp/serene_final_b_2
```

结果：

```text
compare exit=0
{"ok": true, "status": "identical"}
```

## 4. Ctrl+C 与续算

在 `--block-delay-ms 35` 下，观察到快照完成 1 块后发送 SIGINT：

```text
进程退出码 = 0
恢复状态 = interrupted
中断请求时快照 completed_count = 1
当前块完成后 recovery completed_count = 2 / 20
next_block = 2
```

随后同一命令续算：

```bash
.venv/bin/python render.py render \
  --scene examples/garden_door.json \
  --out /tmp/serene_final_c_2 \
  --verify-baseline artifacts/garden_door_demo
```

结果：

```text
status=complete
completed_blocks=20
--verify-baseline exit=0
独立 compare exit=0
{"ok": true, "status": "identical"}
```

这说明中断请求到达时当前块被完成并提交，续算后的 RGBA、层归属、投影与稳定报告均与无中断基线一致。

## 5. 校验失败退出

将样本中层 2 的 `transmittance` 改为 `3.0`：

```text
exit=2
output_dir_created=false
```

stderr：

```json
{
  "code": "SCENE_VALIDATION_ERROR",
  "errors": [
    {
      "field": "transmittance",
      "layer_index": 2,
      "mesh_id": "silk_middle",
      "message": "transmittance must be in [0.0, 1.0]"
    }
  ],
  "ok": false
}
```

## 6. 快照损坏与场景变化

- 修改快照首块 `rgba_sha256`：恢复状态为 `discarded`，全部块重算，最终输出与基线一致。
- 完整渲染后修改场景 `exposure`：场景摘要不匹配，恢复状态为 `discarded`，旧块偏移未复用。
- 非连续 / 坐标或偏移异常块：快照加载阶段拒绝，整份丢弃。

以上场景由 `tests/test_resume.py` 自动执行并通过。

## 7. 差异输出

将候选结果的 `render_rgba.npy` 在 `(x=70, y=10)` 的 G 通道加 1：

```text
compare exit=4
resume_diff.json code=RESUME_OUTPUT_MISMATCH
block_index=1
pixel=[70,10]
channel includes G
```

该行为由 `tests/test_resume.py::test_compare_reports_block_pixel_and_channel_on_mismatch` 验证。

## 8. 示例文件

`artifacts/garden_door_demo/` 保留了本次样本场景的完整输出，包括：

- `render.png` / `render_rgba.npy`
- `layer_map.png` / `layer_map.npy`
- `projection_points.json`
- `report.json`
- `checkpoint.json`
- `textures/`

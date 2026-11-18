# 实际验证记录

验证日期：2026-11-18

本记录中的数字来自工作区内的确定性样本与命令输出，不依赖网络、附件、GPU 或容器。机器可读汇总见 [artifacts/validation_summary.json](artifacts/validation_summary.json)。

## 环境

- 实际运行：Python 3.14.4、NumPy 2.5.5、Linux x86_64；
- 代码兼容目标：Python 3.11+、NumPy 1.26+；
- 未使用 GPU、Docker 或外部媒体；
- 场景与全部参考输出均由 `leather_raster.sample` 确定性生成。

## 复现命令

```bash
python -m leather_raster sample \
  --out examples/sample_scene.json \
  --width 192 --height 144 \
  --block-width 48 --block-height 36 \
  --supersampling 2 --projection-samples 4

python -m leather_raster validate examples/sample_scene.json

python -m leather_raster render \
  examples/sample_scene.json \
  --out artifacts/sample_render --fresh

python -m leather_raster render \
  examples/sample_scene.json \
  --out artifacts/resume_demo --fresh \
  --interrupt-after-blocks 4

python -m leather_raster resume \
  examples/sample_scene.json \
  --out artifacts/resume_demo \
  --verify-baseline artifacts/sample_render
```

## 场景校验

样本场景：

- 场景摘要：`3038d8c90c07709c7281ece14b507b0708daa31bfdd267d6a71965108ea66d3c`；
- 分辨率：`192 x 144`；
- 4 层牛皮，每层 228 个三角形、4 个连续镂空编号；
- 3 盏灯：`lamp_left`、`lamp_center`、`lamp_right`；
- 分块：`48 x 36`，共 16 块；
- 超采样：`2 x 2`；
- 投影采样密度：`4 x 4`；
- `validate` 退出码：`0`。

自动校验测试同时覆盖了索引越界、退化三角形、透光率越界、层序号重复/缺失、镂空编号重复/缺失、灯光落入实心网格、相机基向量非法和幕布轴非法；失败均返回结构化 JSON 与退出码 `2`。

## 渲染质量

实际 `stats.json` 指标：

| 指标 | 实测值 |
| --- | ---: |
| RGB 最大值 | 197 |
| 任一 RGB 通道 ≥ 250 的像素 | 0 |
| 完全纯白像素 | 0 |
| 皮革覆盖像素平均饱和度 | 0.653243 |
| 层归属像素：层 0 / 1 / 2 / 3 | 3832 / 62 / 41 / 11 |
| 背景/幕布像素 | 23702 |

逐层透射与反射占比（投影光计入接收层透射）：

| 层 | 超采样片元 | 透射占比 | 反射占比 |
| ---: | ---: | ---: | ---: |
| 0 | 14658 | 26.655311% | 73.344689% |
| 1 | 14406 | 34.452555% | 65.547445% |
| 2 | 13947 | 24.935006% | 75.064994% |
| 3 | 13624 | 38.407257% | 61.592743% |

这组结果说明正对灯光的面部没有被截断成纯白，同时保留较高平均饱和度。

## 三灯镂空投影

- 总投影案例：336；
- 每盏灯：112 个案例；
- 接收面范围内：328；
- 接收面范围外：8，均标记为不贡献且只使用加法光照，不产生硬黑边；
- 至少被一盏灯照亮的输出像素：4151；
- 两盏及以上灯重叠的输出像素：41；
- 三盏灯最大幕布覆盖率：0.864212 / 0.861568 / 0.865261。

投影落点、世界/像素坐标、接收面 UV、半影世界/像素宽度与采样密度均写入 [projection_points.json](artifacts/sample_render/projection_points.json)。

## 半影趋势

`stats.json` 中的两项趋势检查均通过：

```text
receiver_gap_non_decreasing = true        (33 组)
light_distance_non_decreasing_raw_world_width = true
light_distance_non_decreasing_radius_normalized_width = true
```

一个实际比较（`lamp_left`、层 0、镂空 2）：

| 接收面 | 层间距离 | 半影世界宽度 | 输出像素宽度 | 10%–90% 过渡宽度 |
| --- | ---: | ---: | ---: | ---: |
| 相邻层 `layer:1` | 0.05 | 0.051072 | 2.480021 px | 1.984016 px |
| 幕布 `screen` | 1.00 | 0.147234 | 9.441401 px | 7.553121 px |

边缘亮度采样（归一化覆盖率）为 `0.0, 0.15625, 0.5, 0.84375, 1.0`，体现平滑的半影过渡。`projection_samples = 1` 与 `4` 的确定性测试产生不同但各自可重复的边缘覆盖，说明采样密度实际参与结果。

## 中断、恢复与逐字节一致性

本次演示在提交 4/16 块后中断：

- 中断退出码：`0`；
- 恢复时加载块数：4；
- 恢复时新渲染块数：12；
- 恢复完成退出码：`0`；
- `--verify-baseline`：通过。

恢复输出与完整渲染逐字节一致：

| 文件 | SHA-256 | 大小 | 恢复一致 |
| --- | --- | ---: | --- |
| `rgba.bin` | `1b1313f87349bee905c98aa941d64a2317853765b943b10892fc89bdb889174e` | 110592 | 是 |
| `layer_map.bin` | `a47343bcd457c5801229b3efebd0eb8a506f5f806da46cc7df03d037462354a0` | 27648 | 是 |
| `projection_points.json` | `813ca50f45181dac143dc2b36efb5f473b68b16af42b61e1f54a9dbb722b29ba` | 510340 | 是 |
| `stats.json` | `c9d3fd7db8acad735a85c95250bdcb8194854934e1d973d17fa3a6159d12d7f1` | 91001 | 是 |

中断时报告：[interrupted_recovery_report.json](artifacts/interrupted_recovery_report.json)。续算完成报告：[recovery_report.json](artifacts/resume_demo/recovery_report.json)。

## 自动化测试

```text
$ python -m unittest discover -v
...
Ran 27 tests in 29.591s

OK
```

完整日志：[artifacts/test_run.log](artifacts/test_run.log)。

覆盖的验收点：

- 两次完整渲染 RGBA、层图、投影与统计逐字节一致；
- 反转 `layers` 输入顺序后像素与投影不变；
- 改变分块尺寸后 RGBA 与层图不变；
- 同一纹理种子与 UV 逐元素一致，换种子后不同；
- 同色相邻层在改变种子后会改变输出，证明显式种子参与可区分纹理；
- 三灯投影均存在相邻层与幕布落点，并存在多灯重叠；
- 半影随接收面深度和灯到镂空距离单调增宽；
- 优雅中断完成当前块、写报告并退出 `0`；
- `SIGKILL` 强制终止后从已提交快照恢复并通过逐字节基准校验；
- 快照字节损坏后整份丢弃并从零重算；
- 场景摘要变化后旧快照整份丢弃；
- 人为改动基准时退出码为 `4`，`diff.json` 含块编号、像素坐标和通道。

## 产物

- 样本场景：[examples/sample_scene.json](examples/sample_scene.json)
- 完整 RGBA 预览：[rgba.png](artifacts/sample_render/rgba.png)
- 层归属预览：[layer_map.png](artifacts/sample_render/layer_map.png)
- 投影落点清单：[projection_points.json](artifacts/sample_render/projection_points.json)
- 稳定统计报告：[stats.json](artifacts/sample_render/stats.json)
- 输出清单与哈希：[manifest.json](artifacts/sample_render/manifest.json)
- 机器可读验收汇总：[validation_summary.json](artifacts/validation_summary.json)


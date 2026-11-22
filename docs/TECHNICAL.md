# 技术说明

## 1. 数据模型

场景 JSON 的坐标为量化定点整数：

```json
{
  "scene_id": "hud_orbit",
  "coord_scale": 1024,
  "coord_bounds": [0, 0, 524288, 327680],
  "render_bounds": [0.0, 0.0, 512.0, 320.0],
  "paths": [
    {
      "id": "orbit_fill",
      "kind": "fill",
      "closed": true,
      "segments": [
        {"start": [0, 0], "c1": [1, 2], "c2": [3, 4], "end": [5, 6]}
      ],
      "style": {
        "fill_color": [0.08, 0.72, 0.96],
        "stroke_color": [0.9, 0.9, 0.9],
        "stroke_width": 0.0,
        "opacity": 0.68,
        "blend": "normal",
        "join": "round"
      },
      "injection": null
    }
  ]
}
```

量化坐标除以 `coord_scale` 得到场景用户单位。`render_bounds` 决定画布视口，输出尺寸为：

```text
width  = max(1, ceil((max_x - min_x) * scale))
height = max(1, ceil((max_y - min_y) * scale))
```

子采样网格只影响覆盖精度，不改变画布尺寸。

## 2. 结构校验

`validation.load_scene` 执行以下顺序：

1. JSON 与必填字段检查，失败为 `E_DECODE`。
2. 坐标必须是真正整型，不能是布尔值或浮点；超界为 `E_GEOM`。
3. 段起点必须等于上一段终点；最后一段终点必须等于第一段起点；失败为 `E_GEOM`。
4. 颜色、透明度、描边宽度、blend、join 必须位于清单 `style_contract` 内。
5. 若提供真值清单，检查场景内容哈希、路径集合、样式声明、包围盒和面积。
6. 几何或校验错误携带 `path_id`、`segment`；字段错误携带 `field`。

## 3. 自适应扁平化

每个三次贝塞尔段使用递归 de Casteljau 细分。停止条件为：

```text
max(distance(c1, chord), distance(c2, chord)) <= tolerance
```

`tolerance` 单位为输出像素。控制点到弦的最大距离随细分严格下降，因此容差收紧时使用二分递归只会增加或保持细分数量。最大深度为 24。

输出为 `N x 2 float64` 折线，不把首点重复追加到末尾；闭合由段链和首尾坐标相等保证。相邻重复端点在输出阶段清理。真值比较时：

- 面积除以 `scale²` 回到场景单位；
- 曲线采样点乘 `scale` 后计算到折线的最短距离；
- 闭合检查点到折线距离应为零。

## 4. 覆盖与抗锯齿

### 填充

把轮廓映射到子采样画布，并在每个子采样行使用扫描线求交：

- 水平线与边交点的奇偶区间填充。
- 子采样格点中心判断，避免边界偏好。
- 每个像素覆盖值为：

```text
coverage(y, x) = filled_subsamples / (subsamples²)
```

因此对任意合法子采样数，覆盖值都在 `[0,1]`，并按样本数归一。

### 描边

每条闭合轮廓按线段矩形并集生成描边带，不依赖 GUI 绘图库：

- `round`：线段胶囊并集 + 顶点圆盘。
- `bevel`：线段矩形 + 顶点斜接三角。
- `miter`：按两段法线求 miter tip，限幅 4 倍半宽。

描边宽度乘以 `scale` 超过 `max_stroke_pixels` 时返回 `E_CLIP`。覆盖缓冲所需字节超过每线程 `coverage_buffer_limit` 时同样返回 `E_CLIP`，避免先分配再失败。

## 5. 合成顺序与公式

路径严格按场景声明顺序合成，每次合成使用 `float32`：

```text
a = coverage * opacity
normal:
  rgb = src * a + dst * (1 - a)
  A   = a + A_dst * (1 - a)
add:
  rgb = clip(dst + src * a)
  A   = clip(A_dst + a)
multiply:
  rgb = dst * (1 - a + a * src)
  A   = a + A_dst * (1 - a)
```

合成顺序不可由并行调度改变。并行只用于各路径的扁平化/覆盖预计算。

## 6. 三阶段状态机

每条路径具有三个独立单元：

```text
flatten -> coverage -> compose
```

合法外部迁移：

```text
pending    -> processing -> committed
pending    -> processing -> failed
failed     -> processing -> committed/failed   (重试)
```

其他迁移返回 `E_STATE` 并保持原状态。`coverage` 要求本路径 `flatten` 已提交；`compose` 要求本路径 `coverage` 已提交，且**前一条路径的 compose 已提交**。进度为：

```text
committed_cells / (path_count * 3)
```

## 7. 异常注入

异常只从两处进入：

1. 场景输入路径的 `injection` 字段。
2. `Pipeline` 的 `injections` 参数（界面中的“Apply to selected”）。

没有用于制造失败的源码分支或调试开关。

| 注入 | 阶段 | 错误码 |
|---|---|---|
| `self_intersection` | flatten | `E_SELF` |
| `degenerate` | coverage | `E_DEGEN` |
| `clip` | coverage | `E_CLIP` |
| `stage_failure` | 指定阶段 | `E_STAGE` |

即使禁用注入，真实的自交、零面积和超宽描边仍会被几何/栅格检测。单路径单元失败只更新该单元，其他已提交路径和阶段保留。`failed` 单元可通过 `begin` 合法重试。

## 8. 检查点快照

快照目录：

```text
var/checkpoints/<run>/snapshot/
  manifest.json
  000000_<path>_flatten_<hash>.bin
  000001_<path>_coverage_<hash>.bin
  000002_<path>_compose_<hash>.bin
  ...
```

清单包含：

- `scene_hash`：规范化场景 JSON 的 SHA-256。
- `param_hash`：缩放、容差、子采样、缓冲上限、描边限幅的版本哈希。
- `parameters`：人可读参数值。
- `algorithm_version`：算法版本常量。
- 每个阶段条目的路径、阶段、序号、文件、大小和 payload SHA-256。

写入语义：

- 进程内 `RLock` + 跨进程文件锁，提交串行。
- payload 先写同目录临时文件、`fsync`、`os.replace`，再原子写 manifest。
- 提交顺序必须等于路径优先的 `path:stage` 期望序列，否则 `E_STATE`。
- 加载时检查元数据和每个 payload 哈希；任一缺失、损坏或版本不匹配，整体快照替换为空并全量重算，不混用旧阶段。

每线程覆盖缓冲上限与最大并行度由 `RenderParams` 校验；默认并行度 2，缓冲 32 MiB。

## 9. 三条执行路径

- **full**：清空快照，从零执行全部阶段。
- **incremental**：加载已持久化合法前缀，只补算缺失阶段。
- **resume**：模拟强制终止后新进程启动，只能读取已提交且版本匹配的前缀。

三条路径使用相同路径优先提交序列、相同浮点运算顺序和相同检查点反序列化类型，最终位图数组应满足 `np.array_equal`。

用户取消与硬停止都在阶段边界协作生效：

- `cancel`：状态 `cancelled`，`recoverable=True`，可继续。
- `terminate`：状态 `terminated`，`recoverable=False`，重启只读取已持久化前缀。

这保证不会把“处理中但未提交”的阶段误当作恢复输入。

## 10. 差异定位

`diff.compare_pipelines` 依次比较：

1. 画布尺寸。
2. 阶段提交序列。
3. 每条路径的扁平化数组。
4. 每条路径的覆盖数组。
5. 重放各路径 compose 阶段后的画布。
6. 最终位图。

首个不一致位置返回 `E_DIFF`，并携带：

```text
first_pixel=(x, y), path_id, stage, channel, reason
```

界面自动比对会在隔离检查点目录执行 full/incremental/resume，并把差异热图显示在第四块预览中。

## 11. 并发边界

- 核心用有界 `ThreadPoolExecutor` 并行预计算独立路径的 flatten/coverage。
- 最大并行度限制为 `1..16`，默认 2。
- 每个覆盖任务在分配前检查自身缓冲字节上限。
- 即使预计算并行，状态提交和快照写入仍由主管线按路径顺序串行执行。
- 界面使用单任务 `QThreadPool` 保持事件循环响应；路径级并行由核心工作池完成。

## 12. 已知边界

- “强制终止”是当前进程内的协作式硬停止，不模拟操作系统断电或 `SIGKILL` 的进程内残留内存；恢复语义只承诺已持久化且版本匹配的阶段。
- 闭合折线输出不重复首点；调用方通过首尾不相等的点列 + 隐式闭合边进行填充。
- 非简单多轮廓、孔洞和布尔运算未纳入本版本范围；自交轮廓会被诊断拒绝。


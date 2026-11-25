# 技术设计

## 1. 内部模型与单位

内部模型只保存规范化的整数表达：

- 坐标 `coord = logical_pixel * 4096`。
- 时间 `tick = second * 36,000,000`。
- 透明度 `0..10000`。
- 覆盖 `0..65535`。
- 颜色 RGBA8。

时间基可整除常见帧率，因此帧与毫秒在样本使用的 60 FPS 下可以无损往返。不可整除时使用声明的 `floor`、`ceil` 或 `nearest` 舍入；乘法与除法用 128 位中间值或超出检测，溢出返回 `E_OVF`。

v2 的增量坐标以每个关键帧的 `base` 派生绝对点：

```text
absolute = base + delta
delta    = absolute - base
```

这两次运算都检查 int64 溢出。加载后所有版本进入同一 `Scene/Path/Keyframe/Contour/Segment` 结构，`Scene.SourceVersion` 保留源版本。

## 2. 关键帧插值

对采样时刻 `t`：

1. 查找覆盖 `t` 的相邻关键帧。
2. 精确命中关键帧或位于首尾外部时直接使用对应轮廓。
3. 归一化 `n = t-k0.time`、`d = k1.time-k0.time`。
4. 线性插值系数为 `n/d`。
5. smoothstep 系数为 `n²(3d-2n)/d³`。
6. hold 使用左关键帧。

控制点插值使用 `a + round((b-a)*coef)`，全程有理数运算。所有关键帧必须共享段数和段类型，否则返回 `E_SCHEMA`。

## 3. 闭合扁平化

每条二次/三次贝塞尔按控制多边形长度与容差决定分段数：

```text
ratio = ceil(control_polygon_length / tolerance)
divisions = ceil(sqrt(ratio))
```

该式随容差收紧单调不减，且设置 1024 段硬上限。曲线点通过整数 De Casteljau 计算，不使用浮点累积。最后一点若未回到起点则补回起点，因此输出始终闭合。

面积提供两种观测：

- `PolygonArea`：扁平折线的 shoelace 面积。
- `ContourAreaExact`：对每段贝塞尔多项式精确积分 `∮(x dy - y dx)/2`，使用 `big.Rat`。

这使容差收紧时的折线面积误差可以独立对比解析面积。

## 4. 描边带

1. 将闭合折线拆成边。
2. 对每条边用整数法线和半宽度生成四边形环。
3. 每个顶点按连接方式追加连接环：
   - `round`：8 边圆环；
   - `bevel`：两侧三角环；
   - `miter`：偏移线交点四边形，超长时退化为 bevel。
4. 所有环统一为同向绕数。
5. 扫描线以 nonzero 规则求这些环的并集。

填充与描边同时存在时分别算覆盖，再按像素取覆盖最大值，最后对该路径统一应用一次透明度/合成模式。

## 5. 扫描线与覆盖

视图包围盒来自当帧所有路径的填充折线和描边环，扩展按 `Scale` 换算，并加配置边距：

```text
bitmap_w = ceil(bbox_w * scale / 4096) + 2*margin
bitmap_h = ceil(bbox_h * scale / 4096) + 2*margin
```

每条子采样行按 y 维护活动边表，计算与多边形边的交点。交点按 x 排序后形成 nonzero 或 even-odd 区间；每个像素统计落入区间的子列中心。覆盖值：

```text
coverage = round(count * 65535 / grid²)
```

位图尺寸、像素缓冲上限在分配前检查，超限返回 `E_LIMIT`。

## 6. 合成

合成按场景路径顺序进行。每条路径先得到覆盖图，再应用：

```text
sample_alpha = color.a * opacity * coverage / (255*10000)
```

支持 `source-over`、`normal`、`add`、`multiply`、`screen`。非法透明度、混合模式或连接方式返回 `E_MODE` 并携带路径标识。合成阶段是纯函数：输入位图不会被修改，输出新位图用于原子缓存替换。

## 7. 状态机

每条路径在每个采样时刻独立维护：

```text
flatten -> coverage -> composite
```

每阶段状态：

```text
pending -> processing -> committed
                       -> failed
failed  -> pending -> processing
committed 为正常迁移终点
```

非法迁移返回 `E_STATE` 且不改变原状态。一个采样时刻所有路径的三阶段完成后冻结为该时刻结果；已提交时刻不参与后续编辑失效。失败时刻可通过 `RetryTime` 将失败阶段回到 pending，已提交的更早时刻不受影响。

## 8. 缓存键与失效传播

缓存键为：

```text
SHA256(algorithm, input_content_hash, path_id, time_index,
       stage, parameter_hash, upstream_cache_key)
```

- flatten 的输入是当帧插值轮廓、填充标志、描边宽度/连接、容差与相关关键帧。
- coverage 的输入是几何内容哈希、视图哈希、网格和填充规则，上游为 flatten 键。
- composite 的输入是覆盖内容、颜色/透明度/混合模式和上游覆盖/前一层合成键。

缓存条目记录关键帧、路径、时刻、阶段、图层和上游键。编辑关键帧时只检查该路径真正使用该关键帧的采样时刻；若包围盒改变则传播到整帧 coverage/composite，否则只传播该路径及后续合成层。透明度/混合模式只失效 composite。返回的 `Invalidation` 清单包含 `pathId`、`timeIndex`、`stage`、`reason`。

## 9. 增量一致比对

`CompareTimelines` 按顺序比较：

1. 时刻和提交序号。
2. 视图包围盒与尺寸。
3. 每路径 coverage。
4. 每路径累计合成位图。
5. 最终位图。

首个差异转换为 `E_DIFF`，附带 `TimeIndex`、`X`、`Y`、`PathID`、`Stage` 和原因。

## 10. SQLite 检查点

`checkpoints` 表每个采样时刻一行，主键 `(scene_id,time_index)`，并有 `(scene_id,commit_seq)` 唯一约束。行内包含：

- 场景哈希、配置哈希、内容哈希、payload SHA-256。
- 插值、容差、合成、算法版本。
- RGBA8 位图宽高。
- 完整 gob payload：折线、描边环、coverage、逐路径合成位图、最终位图。

`OpenCheckpoint` 固定单连接，`AppendTime` 再使用互斥锁和单事务，保证提交串行且不可交错。恢复时校验版本、场景/配置哈希、payload 哈希、提交序号连续性和索引列一致性；失败返回 `E_DECODE` 或 `E_VER`。

## 11. 并发边界

- `MaxWorkers` 实际控制采样时刻并行度。
- 硬上限 8；越界 `E_PARA`。
- 每个 worker 分配像素缓冲前检查 `MaxPixelBufferBytes`。
- 渲染按采样时刻并行，但检查点提交严格按时刻升序串行。
- 编辑方法在渲染期间返回 `E_STATE`，避免模型与缓存竞态。


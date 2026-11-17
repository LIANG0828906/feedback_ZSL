# Deterministic Rendering, Caustics, and Recovery

## 1. Canonical Input and Texture Tables

解析成功后：

1. 嵌片按 `(layerIndex, pieceId)` 排序；
2. 每个嵌片内部三角形按排序后的顶点三元组规范排序；
3. 灯光按 `id` 排序；
4. 场景摘要由规范化结构、所有双精度位模式和渲染参数生成 SHA-256。

因此 JSON 中嵌片、灯光或三角形数组的遍历顺序不会影响场景摘要、纹理表或图像。

每个嵌片的气泡/流纹键为：

```text
hash(scene.seed, layerIndex, pieceId, domainConstant)
```

每片直接由 SplitMix64 生成气泡位置、半径、强度、IOR 偏移、流纹方向、频率和相位，不使用全局 RNG。`texture_table.bin` 按层号/嵌片号排序写出，重复运行逐字节一致。每个嵌片的透射缩放和流纹相位不同，因此同层同色相邻片仍会产生可分辨的 RGB 与折射率差。

## 2. Fragment Collection and Layer Composition

每个像素从相机生成一条射线，收集所有嵌片表面的命中，再按：

```text
layerIndex ASC, pieceId ASC, camera distance ASC, triangle index ASC
```

排序。这个顺序只用于把歧义几何固定成唯一语义；正常单层每像素只有一个片元。合成从地坪/背景开始，按层号由内向外执行预乘 Alpha `over`：

```text
out.rgb = src.rgb + dst.rgb * (1 - src.a)
out.a   = src.a   + dst.a   * (1 - src.a)
```

每个片元内部按 `settings.compositionOrder` 声明的顺序合成 `transmission` 和 `reflection` 两个分量。

## 3. Transmission and Edge Glare

对每个点光源：

```text
path = thickness / max(abs(dot(normal, lightDir)), 0.22)
attenuation[c] = exp(-(1 - transmission[c]) * 45 * path)
```

纹理采样进一步调制三通道透射率和边缘 IOR。Fresnel 使用 Schlick 近似，并对红/绿/蓝使用轻微色散 IOR 比例。低入射角项为：

```text
lowAngle = pow(1 - cosine, edgePower)
```

它和边缘带宽内的 Fresnel 反射一起形成边缘眩光。最终 RGB 在写入 8 位图像前执行保色相比例压缩：

```text
if max(r,g,b) > 1: rgb /= max(r,g,b)
```

因此任何通道都不会溢出，也不会通过逐通道截断把彩色高光强行变成白色。

## 4. Caustic Projection and Penumbra

对每个非最外层嵌片：

1. 从该嵌片网格只出现一次的边界边取均匀样本；
2. 对每个点光源，沿“光源 -> 边缘点”方向投影到相邻外层平面；
3. 用边缘切向和嵌片厚度产生确定性折射偏移；
4. 若落点在相邻外层三角网格内，写一条 `outerPiece` 记录；
5. 同时沿远离光源方向投影到地坪顶面，写一条 `floor` 记录；
6. 外层未命中时不生成遮挡掩码，地坪记录采用更高权重，从而按“无遮挡”处理，不产生硬黑边。

半影 sigma 为：

```text
sigmaWorld =
    penumbraBase
  + penumbraThickness * pieceThickness
  + penumbraLayerDistance * targetDistance
```

再用相机焦距/深度转成像素。每个投影样本以高斯核写入固定点 `uint64` RGB 累加缓冲；核支持半径由 `splatSupport` 声明。增加厚度、层间距离或采样密度会分别改变落点、半影宽度和平滑度。

`caustic_gray.u16` 是该固定点缓冲的亮度量化结果。`caustics.bin` 使用固定小端记录布局，`caustics.json` 提供可读落点。

## 5. Chunk Snapshot Protocol

`settings.image.chunkWidth/chunkHeight` 定义行主序块。每块包含 RGBA、归属键和按层统计。

提交顺序：

1. 原子写 `snapshot/chunks/chunk_NNNNNN.bin`；
2. 原子整体替换 `snapshot/snapshot.json`；
3. 清单记录场景摘要、块编号、每块颜色摘要、文件 SHA-256、RGBA SHA-256、归属图 SHA-256。

如果进程在步骤 1 和 2 之间被终止，新块文件是未提交孤儿，不会被读取。清单始终引用一个完整可验证的文件。

加载时逐项校验：

- 清单格式/版本/状态；
- 场景语义摘要和场景摘要对象内维度；
- 分块尺寸、总块数；
- 完成块集合唯一且与块条目完全对应；
- 相对路径不含绝对路径或 `..`；
- 每个块文件 SHA-256；
- 块头、块编号、边界、层统计、载荷长度；
- RGBA/归属图 SHA-256；
- 每块平均颜色摘要和两个颜色摘要哈希。

任一失败都会删除整个缓存目录，`reusedChunks` 清零，从第 0 块重算。不会从损坏块之后或部分已加载块继续。

`SIGINT`/`SIGTERM` 只设置中断标志，当前块仍会完成、提交并写 `recovery.json`，进程以 `0` 退出。`SIGKILL` 没有清理机会，重启只读取最后一次原子提交的清单。

## 6. Difference Report

`verify` 比较：

- `image.rgba`
- `ownership.u64`
- `caustic_gray.u16`
- `caustics.bin` 的记录数和目标坐标
- `texture_table.bin`

发现差异时写出：

```json
{
  "format": "mosaic-difference",
  "version": 1,
  "status": "different",
  "differenceCount": 1,
  "truncated": false,
  "differences": [
    {
      "artifact": "image.rgba",
      "chunk": 3,
      "x": 65,
      "y": 50,
      "channel": "G",
      "a": "127",
      "b": "126",
      "index": 12865
    }
  ]
}
```

差异列表最多保留 2000 条，`differenceCount` 仍是完整计数。发现差异返回 `40`；验证输入不可读返回 `41`。

## 7. Observed Acceptance Trends

正式样本实测：

| Source layer | Thickness | Target distance | Penumbra width |
|---|---:|---:|---:|
| 0 | 0.014191 | 0.075 | 2.095130 px |
| 1 | 0.019278 | 0.155 | 3.208172 px |

投影灰度在相同采样密度下两次运行逐字节相同。厚度增加 `0.01` 的变体最大投影位移为 `2.321089` 世界单位。外层缺口产生的 `1387` 条地坪回退记录全部有非零焦散灰度，证明没有硬黑遮挡边。

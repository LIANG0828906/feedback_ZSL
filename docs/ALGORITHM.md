# 渲染与恢复算法说明

## 1. 确定性数据

每个随机量都用整数哈希派生，不保存跨层 RNG 状态：

```text
layerSeed = splitmix64(sceneSeed, layerId, quadrant, stream)
```

每层生成 `32x32` 木格 RGB8 纹样表和 `32x32` 薄纱 U8 透射表。纹样表包含按 8x8 单元分块的颜色偏移和细纹理，因此同色系相邻木格仍能区分。薄纱表含固定织物条纹，只调制场景给定的 `transmittance`。表文件和清单含哈希；同一场景重复生成的字节一致，与层遍历顺序无关。

## 2. 分块光栅

块号按行优先映射到场景 `tile_size` 网格。每块独立执行：

1. 将三角形变换到相机空间，按 `near` 平面做 Sutherland-Hodgman 裁剪。
2. 投影到全图子样本坐标，仅遍历当前块包围盒。
3. 用重心坐标做三角形覆盖测试，按 `1/z` 进行透视校正插值，得到世界位置、UV 和深度。
4. 对同一层、同一子样本的重复片元按 `(layer_id, depth, quadrant, triangle)` 稳定排序并取最近片元，因此相邻象限接缝和输入列表顺序不会改变结果。
5. 斗拱写入单独的不透明深度缓冲；后方的彩画片元被剔除。

每个像素使用固定的 `aa_samples x aa_samples` 子样本网格并做盒式降采样。油灯面积采样方向同样用固定 Fibonacci 球面序列，不使用时间或未播种随机数，这避免了移动相机时的随机边缘噪点。

## 3. 材质与着色

每个命中片元由 UV 和网格行列数计算木格遮罩 `m_wood`，其余为薄纱 `m_gauze=1-m_wood`。木格遮罩使用平滑过渡，避免细线硬边。

对每盏灯：

```text
irradiance = lampColor * intensity * visibility / (1 + 0.42*d^2)
diffuse   += albedoTexture * irradiance * (0.15 + 0.85*abs(N·L))
transmit  += silkTint * irradiance * T * (0.66 + 0.34*abs(N·L))
reflect   += lampColor * irradiance * Fresnel * specular
```

其中：

- `T` 是层透光率乘薄纱采样表调制值；
- `Fresnel` 用 Schlick 近似，低入射角时反射增强；
- `specular` 为固定高光指数的 Blinn-Phong 项；
- 额外加入小幅环境漫反射和绢纱环境透射，防止侧向纹样变黑。

所有辐射先按亮度做保色相压缩：

```text
scale = 0.955 * (1 - exp(-1.30 * luminance)) / luminance
```

因此最大编码通道不超过 250，不会出现三通道同时削顶造成的白斑。片元 Alpha 由木格/薄纱遮罩、反射率和透光率共同决定，最终颜色以 `premultiplied = radiance * alpha` 保存。

## 4. 层合成与归属

层按 `layer_id` 升序由内向外处理，采用前到后预乘合成：

```text
C += C_i * (1 - A)
A += A_i * (1 - A)
```

若斗拱在相机视线中最近，则斗拱作为不透明后景，只有位于其前方的层可继续覆盖。逐子样本累计每层实际贡献，像素级归属图为：

- R：覆盖样本中出现次数最多的最近层号；
- G：可见层数平均值；
- B：累计贡献最大的层号；
- A：几何覆盖率；
- 255：斗拱。

报告中的逐层透射/反射能量按最终可见贡献拆分，并给出占比。

## 5. 斗拱投影与软阴影

油灯按半径 `r` 采样球面点 `P_sample = P_lamp + r*dir_i`。从受影点到每个采样点做线段与斗拱三角形求交，未被遮挡的样本比例即灰度可见度：

```text
visibility = unblocked_samples / total_samples
```

同一油灯、同一斗拱、同一受影层的报告剖面通过落点并沿受影面切向采样。亮度以 `round(visibility*255)` 输出；在 `0.1` 和 `0.9` 交叉之间线性插值得到边缘过渡宽度。斗拱离受影面越深，面积光造成的半影越宽，报告给出毫米单位的趋势点。

投影落点计算：

```text
direction = dougongVertex - lampPosition
t = dot(layerPlanePoint - lampPosition, layerNormal)
    / dot(direction, layerNormal)
landing = lampPosition + t * direction,  t > 1
```

`shadow_landings.csv` 与 `render_report.json` 均以毫米整数记录，可由场景的灯、斗拱顶点和层平面直接复算。

## 6. 快照协议

每块完成后：

1. 将 `RGBA 预乘像素 + 归属像素`（每像素 8 字节）写入 `blocks/block_NNNNNN.bin` 临时文件，再原子替换。
2. 计算块 SHA-256、颜色通道和、归属通道和及逐层能量统计。
3. 用临时文件整体替换 `snapshot.json`。清单包含场景摘要、尺寸/分块/AA 参数、连续块编号、文件相对路径、块哈希、颜色摘要、统计和整份清单校验和。

续算时：

- 只信任当前清单列出的块；
- 从当前场景和块编号重新计算块偏移，不使用快照中的偏移记录；
- 重算文件 SHA 和所有颜色摘要；
- 校验场景摘要、尺寸、层数、连续编号和清单校验和；
- 任一项不匹配或 JSON 损坏，即删除清单和块目录，从零重算。

安全中断在完成当前块并提交清单后检查，写 `recovery_report.json` 并以 0 退出。成功完成全部块后，最终 PNG、CSV 和报告才写成；全部成功后才删除快照。

## 7. 逐像素验证

`--verify-against <reference-dir>` 使用 libpng 读取本次 `image.png`、`premultiplied.png`、`ownership.png` 与基准图像，逐像素比较 RGBA；同时逐字节比较投影落点 CSV。

出现像素差异时 stderr 输出：

```json
{"error":{"code":"E_RECOVERY_PIXEL_MISMATCH","message":"resumed output differs from reference pixels","differences":[{"file":"image.png","block_index":4,"x":120,"y":80,"channel":"G","reference":91,"actual":92}]}}
```

退出码为 4。投影落点不一致时业务码为 `E_RECOVERY_LANDING_MISMATCH`。


# 技术说明

## 1. 模块边界

- `scene.py`：JSON 读取、默认值补全、聚合校验、规范化场景摘要。
- `texture.py`：只依赖种子和分辨率的整数哈希编织表。
- `geometry.py`：相机射线、三角求交、平面/UV、花格图案和纹理采样。
- `projection.py`：软投影可见度、落点清单、半影审计。
- `renderer.py`：逐层透射/散射、预乘 Alpha、层归属统计。
- `snapshot.py`：每块快照、原子 manifest、全有或全无恢复。
- `reporting.py`：稳定统计和最终工件。
- `compare.py`：逐块/逐像素/逐通道差异。

所有渲染和报告数学均为 CPU NumPy 运算；没有时间、线程调度、全局 RNG
或文件遍历顺序参与像素值。

## 2. 场景语义和校验

场景根必须有 `schema_version: "veil-scene/1"`、合法图像/相机/地面参数、
至少一个点光源和至少一层网格。

每个 layer：

- `layer`：唯一正整数，由内向外。
- `id`：唯一非空字符串。
- `kind`：`gauze` 或 `lattice`。
- `mesh.vertices`：严格为 `N x 3` 有限数字。
- `mesh.triangles`：非空整数三元组；索引必须落在顶点范围内；
  三角形叉积模长不得小于 `1e-10`。
- `thickness`：正数；纱幕入射路径和透射累计使用。

纱幕层还需要：

- `transmittance ∈ [0,1]`
- `tint`：三个 `[0,1]` 分量
- `weave_seed`：非负整数
- `resolution`：`[W,H]`，各在 `[1,4096]`
- `texture_repeat > 0`

花格层还需要：

- `reflectance ∈ [0,1]`
- `width > 0`
- `metal_color`：三个 `[0,1]` 分量
- `pattern.style ∈ {grid, diamond}`
- `pattern.pitch > width`
- `pattern.edge_softness >= 0`
- `pattern_seed`：非负整数

光源位于任意三角形网格表面内（含平面容差）时，每个涉及层产生一条
`E_LIGHT_INSIDE_LAYER`。校验不会先渲染再失败；成功返回前不会建立快照。

相机求交支持任意三角网格；花格透视投影以该层首个非退化三角形确定的
平面为参考，因此实际制作中的纱幕/花格层应保持共面。样例和测试均使用
平面面板。

场景摘要是补全默认值后规范化 JSON 的 SHA-256。键顺序和 JSON 空白不影响
摘要；语义内容变化会触发旧快照整体失效。

## 3. 确定性编织纹理

`generate_weave_texture(seed, [W,H])` 对每个像素由坐标与种子构造 uint64
哈希，再组合固定整数三角波作为经线/纬线、周期相位和噪声。输出是
行主序 `uint8[H,W]`，SHA-256 可仅凭 `seed` 和 `resolution` 复算。

纹理在渲染中同时影响：

- 法向透光率：`tau * (0.76 + 0.44*w)`
- 染色混合量：`0.54 + 0.40*w`
- 散射纹理色：`tint * (0.84 + 0.28*w)`

因此相同染色、相邻位置的纱幕使用不同种子/分辨率时仍会产生可区分纹理。

## 4. 纱幕与花格着色

相机使用右手基和规则子像素网格；`camera.samples=2` 表示 2x2 个子像素，
不是随机抖动。

纱幕的第 `i` 个片元：

```text
cos_i       = clamp(abs(n · v), 0.22, 1)
path_i      = (thickness_i / 0.006) / cos_i
tau_i       = clamp(tau * (0.76 + 0.44*weave), 0.002, 0.995) ** path_i
Ctrans_i    = tau_i * mix([1,1,1], tint_i, 0.54 + 0.40*weave)
L_i         = luminance(compress(sum_j light_j * falloff_j * angle_j * visibility_j))
S_i         = tint_i * L_i * scatter_strength * texture_terms + ambient_tint
```

`compress(E)=E*(1-exp(-luminance(E)))/luminance(E)` 只压缩亮度、保留 RGB
比例，因此正对光源时不会把染色通道一起推到纯白；背光侧仍由染色透射和
低幅纹理散射保留色相。

花格图案以多层斜向/正交条带覆盖率 `c_i` 表示：

```text
T_i = 1 - c_i
S_i = metal_color * reflectance * compressed_light * coverage_terms
```

所有层按 `layer` 升序由内向外累积：

```text
P' = P * T_i + S_i * (1 - max(T_i))
A' = A * max(T_i) + (1 - max(T_i))
```

最终写 PNG 前满足 `P <= A`。这保证文件确实是预乘 Alpha，不会出现亮色
超过 Alpha 的非法像素。

层归属权重使用同一合成顺序的 `remaining * (1-max(T_i))`。主归属是在
全部子像素上权重和最大的层，平局取较小 ordinal；`B` 是至少命中一次的
不同层数。因此归属图可由场景、相机和声明采样规则复算。

## 5. 花格软投影

对接收点 `P` 和点光源 `L`，向光源方向与花格平面求交：

```text
s = n·(q0 - P) / n·(L - P)
Q = P + s*(L - P),  0 < s < 1
```

`Q` 必须位于花格网格内。条带覆盖率在确定性规则的 `N x N` 偏移上采样，
`N = projection.samples_per_axis`；偏移为
`((i+0.5)/N - 0.5)`，不使用随机数。每个光源分别计算，得到
`visibility ∈ [0,1]`，再乘入该光源的散射/反射贡献。

半影宽度：

```text
w = min(max_penumbra,
        base * (1 + distance_gain*|P-Q| + source_gain*|Q-L|))
```

两项距离增益均为非负数，故增加花格到接收面距离或光源到花格距离都会
单调增宽。报告的实际落点项直接包含 `penumbra_width`；审计部分使用固定
距离序列和边缘亮度剖面，并给出两项单调性布尔值。

落点报告从花格条带中心采样源点 `Q`，沿 `L -> Q` 延长线分别求地面和
最近外层纱幕交点。目标在声明的接收面范围外时 `inside_receiver=false`、
`occluded=false`；渲染也只在真实接收几何内应用阴影，因此不会在接收面
外创建硬黑边。

## 6. 分块快照协议

块编号为行优先：`tile_id = tile_y * tiles_x + tile_x`。每块二进制 tile
包含：

```text
magic/version/tile_id/height/width/layer_count
RGBA8 像素
归属图 RGBA8 像素
每层 hit_count, transmittance_sum, transmission_sum, reflection_sum
```

提交顺序：

1. 写入 `tile-XXXXXXXX.tile.tmp`，fsync，原子替换正式文件。
2. 在内存中的完整 manifest 加入该 tile 的文件 SHA-256、RGBA 摘要、
   归属图摘要。
3. 写 `manifest.json.tmp`，fsync，原子替换 manifest，并 fsync 目录。

恢复只信任 manifest 列出的块。场景摘要、算法版本、图像/块尺寸、块号、
manifest 计数、每块文件摘要、RGBA/归属摘要和 tile 形状全通过才续算。
任一项失败则删除整个 `snapshot/` 并从零重算。manifest 未列出的孤儿 tile
不会被采用。

Ctrl+C 的 handler 只设置标志；渲染循环完成当前 tile 和快照后退出。若仍有
未完成块，写 `recovery.json` 并返回 0。SIGKILL 没有机会执行 handler；只
依赖最后成功原子替换的 manifest。

## 7. 一致性比较

`compare` 对 `rgba.png` 和 `layer_ownership.png` 做逐像素比较，对投影 JSON
做递归叶节点比较。图像差异项：

```json
{
  "artifact": "rgba.png",
  "tile_id": 3,
  "pixel": {"x": 78, "y": 55},
  "channel": "R",
  "expected": 91,
  "actual": 90
}
```

发现任何差异时返回 3，并把完整结构写到 `differences.json`。`render
--baseline DIR` 可在完成渲染后自动执行同一比较。

## 8. 稳定性边界

稳定统计只依赖最终 tile、场景和固定报告算法，不含耗时、恢复模式或进程
编号，因此不中断运行和续算运行会得到逐字节相同的 `report.json`。
`run_metrics.json` 单独记录耗时与本次提交块数。


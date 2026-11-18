# 哥特花窗离线软光栅验证器

这是一个仅依赖 Python、NumPy 和 Pillow 的确定性离线验证组件，用于在美术把花窗 JSON 合入正式场景前检查：

- 多波段透射是否保持蓝/红色相；
- 玻璃条遮挡、投影落点与侧墙软阴影；
- 釉面反射和透射是否分离、掠射是否出现彩色噪点；
- 分块快照在优雅中断或强制终止后能否逐像素恢复。

组件不访问网络、不使用 GPU，也不依赖贴图素材。场景 JSON、玻璃内部纹理和测试图都由整数种子确定性生成。

## 安装与运行

要求 Python 3.10 或以上。

Windows PowerShell：

```powershell
py -3.10 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
```

Linux/macOS：

```bash
python3 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
```

生成并验证本地样本，然后完整渲染：

```bash
python -m glass_verify sample --out samples/scene.json
python -m glass_verify validate --scene samples/scene.json
python -m glass_verify render --scene samples/scene.json --out out --fresh
```

输出目录包含：

| 文件 | 内容 |
| --- | --- |
| `color.png` | 8 位 RGB，平方根 gamma 编码，无时间戳元数据 |
| `attribution.png` | 16 位灰度逐像素玻璃归属图 |
| `attribution.json` | 归属值到 `sector_id`/`pane_id` 的稳定映射；`0` 为墙/背景，`1` 为窗棂 |
| `projection_hits.json` | 窗棂前后深度层投影到侧墙的源点、落点、距离和偏移 |
| `shadow_profiles.json` | 隔离窗棂边缘的亮度采样、10–90% 过渡宽度及深度趋势 |
| `textures.npy` | 逐片内部斑驳采样表及确定性二进制头 |
| `stats.json` | 无时间戳、无绝对路径的稳定统计报告 |
| `snapshot.npz` | 原子提交的分块快照 |

模拟优雅中断、恢复并做逐字节核对：

```bash
python -m glass_verify render --scene samples/scene.json --out resumed --fresh --stop-after-chunk 3
python -m glass_verify render --scene samples/scene.json --out resumed
python -m glass_verify compare --scene samples/scene.json --reference out --candidate resumed
```

`render` 也可以直接传入 `--reference out`；恢复结果不一致时会写出 `differences.json` 并以退出码 `3` 结束。

运行验收测试：

```bash
python -m unittest discover -s tests -v
```

## 场景 JSON 契约

完整样本见 `samples/scene.json`。关键字段如下：

```json
{
  "schema_version": 1,
  "sectors": [{"sector_id": 0}, {"sector_id": 1}],
  "window": {
    "origin": [0, 0, 0],
    "normal": [0, 0, 1],
    "axis_u": [1, 0, 0],
    "axis_v": [0, 1, 0]
  },
  "panes": [
    {
      "sector_id": 0,
      "pane_id": 0,
      "vertices": [[-0.6, 0.3, 0.06], [0.6, 0.3, 0.06],
                   [0.6, 1.1, 0.06], [-0.6, 1.1, 0.06]],
      "triangles": [[0, 1, 2], [0, 2, 3]],
      "transmission_color": [0.035, 0.20, 0.98],
      "transmittance": 0.74,
      "glaze_reflectance": 0.035,
      "lead_width": 0.03,
      "lead_depth": 0.04
    }
  ],
  "camera": {
    "position": [-1.15, 1.02, 4.8],
    "target": [0.12, 1.98, 0],
    "up": [0, 1, 0],
    "fov_y_deg": 58,
    "width": 180,
    "height": 120,
    "near": 0.08,
    "far": 20
  },
  "directional_light": {
    "direction": [0.55, -0.22, 0.80],
    "irradiance": [3.0, 2.85, 2.70],
    "bands_nm": [650, 550, 450],
    "angular_radius_deg": 1.25
  },
  "texture": {
    "seed": 20261118,
    "table_size": 40,
    "transmission_strength": 0.28,
    "reflection_strength": 0.50,
    "octaves": 3
  },
  "walls": [
    {
      "id": "side_aisle",
      "point": [2.75, 2.0, 2.3],
      "normal": [-1, 0, 0],
      "axis_u": [0, 0, 1],
      "axis_v": [0, 1, 0],
      "extent_u": 6.6,
      "extent_v": 4.2,
      "albedo": [0.36, 0.335, 0.295],
      "receives_shadow": true
    }
  ],
  "render": {
    "chunk_size": [45, 60],
    "supersample": 2,
    "exposure": 0.42,
    "background": [0.008, 0.010, 0.014],
    "ambient_light": [0.05, 0.056, 0.068],
    "reflection_tint": [0.78, 0.84, 0.94],
    "composite_order": "transmission_then_reflection",
    "grazing_reflectance_cap": 0.28,
    "output_rounding": "half_up"
  },
  "projection": {
    "wall_index": 0,
    "samples_per_axis": 9,
    "penumbra_per_depth": 0.55,
    "profile_points": 33,
    "depth_levels": 5
  }
}
```

约定：

- `directional_light.direction` 是光线传播方向，即从光源走向场景/室内。
- `directional_light.irradiance` 可给 RGB 三波段向量，也可给三通道相同的标量。
- `panes` 使用平铺列表；每片玻璃带 `sector_id`、`pane_id`、三角网格和完整材质参数。
- `sectors` 可省略；给出时必须与玻璃中的扇区集合一致，且不能重复声明。
- 扇区编号必须从 `0` 连续；每扇区片序号也要从 `0` 连续且唯一。
- `sector_id` 数值小的一侧视为内侧；颜色层级按 `(sector_id, pane_id)` 排序，不依赖输入数组顺序。
- 三角形绕序决定局部法线；窗棂棱柱沿 `window.normal` 挤出。
- `composite_order` 只接受 `transmission_then_reflection` 或 `reflection_then_transmission`。
- `bands_nm` 与 RGB 三通道一一对应，样本中为红、绿、蓝三个可见光波段。

出现索引越界、退化三角形、材质越界、扇区缺失/重复、光线与窗面平行等情况时，命令整体拒绝场景，输出结构化 JSON，并以非零整数退出。错误项包含 `code`、`sector_id`、`pane_id`、`reason` 和 JSON Pointer 路径。

## 渲染模型

### 多波段透射

每条相机射线与所有玻璃三角形求交。三个颜色通道分别作为 650/550/450 nm 波段独立累积：

```text
band_energy = irradiance[band]
            * (0.58 + 0.42 * incidence_cosine)
            * pane.transmittance
            * transmission_color[band]
            * deterministic_texture_modulation
```

没有跨通道平均，也没有把反射量当白光直接加到透射上。样本中正对光的蓝片保持 `B > G > R`，侧向红片保持 `R > B` 且不会黑成一片。

### 玻璃内部斑驳

每片玻璃的纹理种子由 `(scene.seed, sector_id, pane_id)` 经整数 hash 得到。三层 value-noise 使用固定整数格点 hash 和 smoothstep 插值，不使用 `np.random` 或 Python 未播种随机数。纹理同时调制：

- 透射颜色能量；
- 釉面反射率；
- 每片玻璃的稳定亮度相位。

因此同色相邻玻璃片可区分，结果也不依赖片列表遍历顺序。`textures.npy` 两次写出逐字节一致。

### 反射、透射与掠射抑制

釉面反射采用 Schlick 形式：

```text
F = R0 + (1 - R0) * (1 - cos(view,normal))^5
```

再受 `grazing_reflectance_cap` 限制。低角度掠射时，反射色度会确定性去饱和，避免未经滤波的彩色闪烁；这里没有随机采样。反射和透射按 `composite_order` 做能量保持的 source-over 合成，单通道始终在 `[0,1]` 内。最终线性颜色乘曝光、截断、平方根 gamma 编码到 8 位。

每片玻璃的 `reflection_fraction` 和 `transmission_fraction` 直接来自可见片元的线性能量积分，可用于判断灰绿斑来自反射层还是透射层。

### 窗棂、投影与软影

每片玻璃中只出现一次的三角形边被视为边界边；内部三角化对角线不会生成窗棂。边界边按 `lead_width` 展宽、按 `lead_depth` 沿窗法线挤出为矩形棱柱，相机射线与棱柱求交后产生遮挡。

投影落点由场景光线方向精确计算：

```text
wall_point = source_point + light_distance * light_direction
```

`projection_hits.json` 同时给出源点、落点、距离、深度层和偏移向量，美术可直接用场景 JSON 复算。

侧墙软影把窗面投影点与声明采样网格卷积。每次使用 `samples_per_axis²` 个固定偏移，没有逐帧随机噪声。隔离边缘的过渡宽度定义为：

```text
transition_width = 2 * max(penumbra_per_depth, tan(angular_radius_deg)) * lead_depth
                   / abs(dot(light_direction, window_normal))
```

因此宽度随玻璃条进深严格增加；`shadow_profiles.json.depth_trend` 给出稳定排序后的实测趋势和采样亮度。实际侧墙渲染还叠加了同一窗面内的其他窗棂遮挡。

## 分块与恢复

- 图像按 `render.chunk_size` 行优先切块，块坐标每次从块编号和场景尺寸重算；快照不携带可复用的块/扇区偏移。
- 每完成一块，生成一个完整的 `.npz` 快照，写入同目录临时文件、`fsync`，再用 `os.replace` 整体替换旧快照。
- 快照记录场景摘要、渲染器摘要、完成块前缀、每块 RGB/归属/统计摘要、恢复次数和总 payload 摘要。
- 启动时只有整个快照通过 magic、维度、块形状、场景摘要、渲染器摘要、逐块摘要和 payload 摘要校验才会续算。
- 快照损坏或场景摘要不一致时，旧快照被整体丢弃，从块 `0` 和扇区 `0` 重新计算。
- `SIGINT`/`SIGTERM` 只设置停止标志；当前块完成并原子提交后，写出 `recovery_report.json`，进程以 `0` 退出。
- `SIGKILL` 无法被捕获，因此保留最后一个已提交快照；重启后从该前缀续算。

`compare` 逐像素比较 RGB、16 位归属图和投影 JSON，并额外校验两张 PNG 的文件字节。差异项包含 `chunk_id`、`x`、`y`、`channel`、参考值和新值；不一致时退出码为 `3`。

## 退出码

| 退出码 | 含义 |
| --- | --- |
| `0` | 完整渲染成功，或可处理中断已写出恢复报告 |
| `2` | 场景 JSON 校验失败，整体拒绝 |
| `3` | 恢复结果或参考输出逐像素/投影点不一致 |
| `4` | 本地 I/O 或运行参数错误 |

## 实际验证记录

验证环境：Ubuntu 26.04、Python 3.14.4、NumPy 2.5.5、Pillow 13.0.0。代码使用 Python 3.10 兼容语法。

```text
python -m unittest discover -s tests -v
Ran 26 tests in 19.277s
OK
```

机器可读的实测摘要保存在 `artifacts/verification_summary.json`，其中记录了参考渲染与续算渲染的 PNG/投影摘要、蓝红片平均通道、软影深度趋势和结构化错误样例。

覆盖并实际通过：

- 索引越界、退化三角形、透光率/反射率越界、扇区缺失/重复、平行光，均返回结构化错误与退出码 `2`。
- 同参数纹理表逐字节一致，反转 `panes` 输入顺序后纹理表逐字节一致。
- 蓝片平均通道满足 `B > 1.25G` 且 `B > 2R`，红片满足 `R > 1.8B` 且 `R > 100`；玻璃区域不存在三通道同时接近饱和的像素。
- 每片玻璃反射/透射贡献比之和为 `1`，掠射反射有固定上限。
- 隔离窗棂边缘的过渡宽度随进深从 `0.0411` 增至 `0.2688` 世界单位。
- 优雅中断于第 2 块后恢复，RGB、16 位归属图和投影 JSON 与完整渲染逐字节一致。
- 在第 2 块提交后真实 `SIGKILL`，重启续算结果与完整渲染逐字节一致。
- 人为破坏快照、修改场景摘要后，均从零重算并仍与对应完整渲染逐字节一致。

# serene-render

`serene-render` 是一个从零实现的离线 CPU 软光栅验证组件，用于三层纱绢 + 两层棂条交替叠合的园林隔扇门。项目不访问网络、不使用容器，只依赖 NumPy 与 Pillow。

主要能力：

- 读取并严格校验 JSON 场景，非法输入输出结构化错误并以非零码退出。
- 由整数种子确定性生成纱线编织采样表，同时调制透光率与染色。
- 分层透射 / 散射 / 反射着色，输出显示空间预乘 Alpha RGBA。
- 棂条沿太阳方向光投影到相邻纱绢和地面，生成可复算的落点清单与柔和半影。
- 按块提交原子快照，只接受连续前缀；Ctrl+C 完成当前块后以 0 退出。
- 续算结果可与无中断基线逐像素、逐文件比较，差异包含块号、像素坐标与通道。
- 稳定报告不含耗时；耗时单独写入 `timing.json`。

## 快速开始

Windows 10/11：

```bat
py -3.10 -m venv .venv
.venv\Scripts\activate
python -m pip install -r requirements.txt
python render.py sample --output examples\garden_door.json
python render.py render --scene examples\garden_door.json --out out\garden_door
python -m unittest discover -s tests -v
```

Linux / macOS：

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements.txt
python render.py sample --output examples/garden_door.json
python render.py render --scene examples/garden_door.json --out out/garden_door
python -m unittest discover -s tests -v
```

也可使用模块入口：

```bash
python -m serene_render render --scene examples/garden_door.json --out out/garden_door
```

## 输出文件

| 文件 | 内容 |
| --- | --- |
| `render.png` | RGBA PNG，显示空间预乘 Alpha |
| `render_rgba.npy` | 逐像素 `uint8 [H,W,4]`，便于逐字节核对 |
| `layer_map.npy` | `int32 [H,W]`；`-2` 地面、`-1` 空白、`>=0` 层序号 |
| `layer_map.png` | 同一归属图的 RGBA 编码：`code = R + 256*G`，`B=255` 表示有表面 |
| `projection_points.json` | 棂条三角形到纱绢/地面的全部投影落点、边界状态与边缘亮度采样 |
| `report.json` | 稳定统计报告；不含耗时 |
| `checkpoint.json` | 每块提交后的完整原子快照，含场景摘要和块颜色摘要 |
| `recovery_report.json` | 中断/完成状态、已完成块和恢复原因 |
| `textures/` | 每层纱绢的透光调制表和染色调制表，均为确定性 `.npy` |
| `timing.json` | 仅运行耗时，不参与续算一致性比较 |
| `resume_diff.json` | `compare` 或 `--verify-baseline` 发现差异时生成 |

`layer_map.png` 的 16 位逻辑代码为：`0` 空白、`1` 地面、`layer_index + 2` 具体层。精确归属以 `layer_map.npy` 为准。

## 场景格式

完整示例见 `examples/garden_door.json`。最小结构如下：

```json
{
  "image": {"width": 320, "height": 240, "tile_size": 64},
  "camera": {
    "position": [3.35, 2.55, 7.2],
    "target": [0.0, 1.55, 0.0],
    "up": [0.0, 1.0, 0.0],
    "fov_y_deg": 38.0,
    "near": 0.05,
    "far": 40.0
  },
  "sun": {
    "direction": [0.46009571, -0.32006658, -0.82817228],
    "intensity": 1.12,
    "color": [1.0, 0.955, 0.84],
    "light_distance": 12.0,
    "angular_radius_deg": 0.55
  },
  "ground": {
    "y": 0.0,
    "bounds": [-6.0, 6.0, -6.0, 6.0],
    "albedo": [0.43, 0.38, 0.31],
    "render": true
  },
  "shadow": {
    "sample_density": 64,
    "penumbra_base": 0.8,
    "light_distance_scale": 18.0,
    "max_width": 1.2,
    "min_width": 0.0008,
    "strength": 0.78
  },
  "ambient": [0.085, 0.095, 0.115],
  "exposure": 0.95,
  "thickness_absorption": 0.65,
  "layers": [
    {
      "layer_index": 0,
      "id": "silk_inner",
      "kind": "silk",
      "thickness": 0.18,
      "vertices": [[-1.36, 0.09, -0.36], [0.0, 0.09, -0.36], [0.0, 0.0, -0.36]],
      "triangles": [[0, 1, 2]],
      "transmittance": 0.58,
      "tint": [0.91, 0.55, 0.30],
      "pattern_seed": 1301,
      "texture_resolution": [128, 128],
      "texture_scale": 0.42,
      "scatter_strength": 0.92
    }
  ]
}
```

约束与约定：

- `layers` 中每项是一层、一个平面三角网格。层号必须唯一且连续，可从 0 或 1 开始。
- 每层 `id` 与网格编号全局唯一；三角索引必须合法且非退化；顶点必须共面。
- `kind` 为 `silk` 或 `mullion`；样本场景为 `silk, mullion, silk, mullion, silk`。
- `sun.direction` 是光线传播方向，必须为单位向量，且不能平行于任何层平面或地面。
- 太阳源位置按 `source = -direction * light_distance` 计算，用于半影距离趋势。
- `sample_density` 同时控制软边灰度量化级数和报告中每条边缘剖面的采样点数。
- 层序号约定为从内侧到外侧；合成按层号升序，即从内向外。

## 数值模型

### 纹理

纹理表是 `(pattern_seed, texture_resolution)` 的纯函数，使用显式 `uint64` 哈希，没有未播种 RNG。采样使用层平面局部正交基与 `texture_scale` 做双线性查找。采样表同时产生：

- `T_m`：透光率调制；
- `C_m`：染色颜色调制。

因此遍历顺序、缓存状态和此前生成的其它层不会改变某层纹理字节。

### 纱绢着色

每个可见片元由相机射线与平面网格求交得到。有效透光率包含太阳入射角、视线入射角和层厚的确定性路径衰减：

```text
T_eff = T * T_m * exp(-thickness_absorption * thickness * path_factor)
```

着色由三部分组成：

- 透射太阳光：使用 `T_eff`、太阳方向余弦、纹理染色和软阴影因子；
- 纹理调制散射：`(1 - T_eff)`、环境光、纱绢染色与纹理；
- Fresnel / 环境反射。

最终在线性空间从内向外做预乘 Alpha 合成，再统一做 `1 - exp(-L * exposure)` 映射和 sRGB Gamma。输出 RGB 再乘最终 Alpha，因此 PNG 与 `.npy` 中的 RGB 是显示空间预乘值。

每层报告的 `transmission_fraction` 与 `reflection_fraction` 由该层实际着色亮度的透射项、反射/散射项积分后归一化得到。

### 棂条投影与半影

棂条点 `p` 沿 `d` 投到目标平面：

```text
q = p + t*d
t = dot(target_plane_point - p, target_plane_normal)
    / dot(d, target_plane_normal)
```

每个棂条层向光线传播方向上的最近一层纱绢投影，同时投影到地面。`projection_points.json` 保存每个三角形的源质心、全部落点和是否完全位于接收面内。阴影着色只用有限接收面内的点；地面之外直接按无遮挡处理，地面边界处再乘平滑渐隐，避免边界硬黑带。

软边宽度：

```text
width = clamp(
  penumbra_base * receiver_distance
  * (1 + light_distance / light_distance_scale),
  min_width, max_width
)
```

它对接收面距离和光源到棂条层距离均单调不减。有符号距离经过平滑映射后按 `sample_density` 量化；报告同时给出 10%–90% 过渡宽度与若干落点亮度。样本场景中纱绢上的最大相邻阴影因子跳变低于 0.14，而不是硬边的 0.78。

### 稳定性

- 没有逐帧随机抖动或随机采样。
- 片元先按层号、再按确定性的深度/三角形编号处理，不依赖 JSON 遍历顺序。
- 同一像素同深度时用三角形编号决胜，避免 Z-fighting 亮点。
- 纹理、投影、快照和报告均可由场景、种子和块号独立复算。

## 快照与中断恢复

每完成一块，程序构建完整快照并原子替换 `checkpoint.json`：

1. 写入同目录临时文件；
2. `flush` + `fsync`；
3. `os.replace` 原子覆盖；
4. 尽可能同步目录元数据。

快照包含场景规范 JSON 的 SHA-256、渲染器版本、图尺寸、块尺寸、块数、每块坐标、RGBA/层归属摘要、颜色摘要和 Base64 像素数据。

恢复规则：

- 只接受 `0..k-1` 的连续前缀；顺序、坐标、块偏移或索引异常会丢弃全部块。
- 场景摘要、渲染器版本、尺寸或块参数不一致时整份丢弃并从块 0 重算。
- Base64、长度、SHA-256、颜色摘要任意一项损坏时整份丢弃。
- Ctrl+C/SIGTERM 处理器只设置停止事件；当前块渲染完并提交快照后才退出，恢复报告写入 `recovery_report.json`，退出码为 `0`。
- SIGKILL 等不可处理终止最多丢失正在渲染且尚未提交的一块。

## 比较与退出码

无中断基线运行完成后，对续算结果执行：

```bash
python render.py render \
  --scene examples/garden_door.json \
  --out out/resumed \
  --verify-baseline out/baseline
```

或独立比较：

```bash
python render.py compare --reference out/baseline --candidate out/resumed
```

比较范围包括 RGBA 原始像素、层归属、投影落点、PNG 和稳定报告。差异记录示例：

```json
{
  "block_index": 1,
  "pixel": [70, 10],
  "channel": ["G"],
  "reference": [145, 121, 92, 221],
  "candidate": [145, 122, 92, 221]
}
```

| 退出码 | 含义 |
| --- | --- |
| `0` | 完成，或 Ctrl+C 后已保存当前块和恢复报告 |
| `2` | JSON/几何/材质/相机/光照校验失败，未开始渲染 |
| `3` | 渲染或输出组装错误 |
| `4` | 基线比较不一致 |

结构化校验错误写 stderr，包含 `field`、`layer_index`、`mesh_id` 和 `message`。

## 验证

实际运行结果、哈希、统计与命令记录见 [VALIDATION.md](VALIDATION.md)。33 项测试覆盖几何校验、纹理确定性、软阴影趋势、像素稳定性、快照恢复、损坏/场景变化丢弃和结构化差异输出。

```bash
python -m unittest discover -s tests -v
```

示例成果位于 `artifacts/garden_door_demo/`。

## 已知边界

- 场景层网格需为单个共面三角网格；这是“层平面”与法向阴影映射的前提。
- 棂条遮挡按光线传播方向上的最近纱绢接收面报告；地面为有限轴对齐矩形。
- 渲染器是验证用 CPU 实现，不是交互式实时渲染器；重点是可复算、可核对与恢复一致性。

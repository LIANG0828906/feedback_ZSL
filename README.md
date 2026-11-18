# 三层竹编灯罩离线软光栅验证器

这是一个只依赖 Python 3.10+、NumPy 与 Pillow 的确定性 CPU 渲染组件。它读取 JSON 场景，严格校验三层竹编几何与材质，按相机视线逐像素采样三角形片元，逐层计算透射、散射与反射，合成预乘 Alpha RGBA 图像，并输出层归属图、孔洞投影灰度、稳定统计和可恢复分块快照。

运行时不访问网络、显卡或外部素材。场景与几何由代码确定性构造。

## Windows 快速开始

```powershell
py -3.10 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
.\.venv\Scripts\python -m bamboo_render make-scene --out scenes\bamboo_three_layer.json
.\.venv\Scripts\python -m bamboo_render validate scenes\bamboo_three_layer.json
.\.venv\Scripts\python -m bamboo_render render scenes\bamboo_three_layer.json --out artifacts\run
```

可选安装命令行入口：

```powershell
.\.venv\Scripts\python -m pip install -e .
.\.venv\Scripts\bamboo-render validate scenes\bamboo_three_layer.json
```

运行全部验收测试与验收脚本：

```powershell
.\.venv\Scripts\python -m unittest discover -s tests -v
.\.venv\Scripts\python scripts\run_acceptance.py
```

`run_acceptance.py` 会生成默认 320×240 场景，完成一次完整渲染、一次 7 块中断、一次恢复续算，逐字节比较 RGBA、层归属、逐层预乘图、投影落点和快照，并写出 `artifacts/acceptance/summary.json`。

## 中断与恢复

```powershell
# 完成 7 个新块后提交快照并以 0 退出
.\.venv\Scripts\python -m bamboo_render render scene.json --out artifacts\resume --interrupt-after 7

# 自动读取并校验 snapshot.json，只补算未完成块
.\.venv\Scripts\python -m bamboo_render render scene.json --out artifacts\resume

# 显式丢弃旧快照，从块 0 重算
.\.venv\Scripts\python -m bamboo_render render scene.json --out artifacts\resume --restart

# 一条命令完成完整渲染、中断、恢复与逐字节验证
.\.venv\Scripts\python -m bamboo_render verify-resume scene.json --out artifacts\verify --interrupt-after 7
```

中断运行写出 `recovery_report.json`（`status=interrupted`），恢复运行将其更新为 `status=resumed`。报告不含时间戳、绝对路径和随机值。快照损坏或场景摘要不一致时，整个快照被丢弃，报告记录 `snapshot_discarded=true` 与稳定原因码。

## 场景格式

`make-scene` 生成的 `scenes/bamboo_three_layer.json` 是完整示例。关键约束如下：

- `schema_version` 必须为 `1`。
- `image.width/height` 为 1–4096，`chunk_size` 为两个正整数且不超过图像尺寸。
- `camera.position != camera.target`，`up` 不与视线平行，`0 < near < far`，FOV 在 `[1, 179]` 度。
- `light.position` 必须在最内层前方（更小 Z），且不得落入任一层竹丝的三角形挤出体积。
- `inner_wall` 必须位于最外层之后，平面法向非零，相机每个角点的射线都能在 near 之后命中。
- `layers[].layer` 为正整数且唯一；层按编号由内向外排序。
- 最内层 `air_gap=0`，其余层 `air_gap>0`。层平面 Z 由 `layer_origin_z` 与累计空气隙推导。
- 每层网格必须有非空顶点、三角形和逐三角形 `strand_ids`。三角形索引必须越界检查通过，三点不得重复，面积必须大于 `1e-12`。
- 网格必须落在该层推导出的平面上；`uvs` 可选，给出时必须是平面 XY 的仿射映射且位于 `[0,1]`。
- `weave_pitch > 0`，`0 < strand_diameter < weave_pitch`。
- `liner.transmittance`、`liner.reflectance`、`inner_wall.reflectance` 均在 `[0,1]`；所有颜色分量在 `[0,1]`。
- `shadow.sampling_density` 为 1–65 的整数，`penumbra_per_gap > 0`。

校验失败时，CLI 向 stderr 输出稳定 JSON，例如：

```json
{"error":{"code":"MESH_INDEX_OUT_OF_RANGE","layer":2,"reason":"triangle references a vertex outside the vertex array","triangle_index":0,"vertex_index":99999},"ok":false,"stage":"validation"}
```

退出码：`0` 成功或可恢复中断；`1` 比较发现差异；`2` 场景校验失败。

## 渲染模型

### 确定性纹理

- 使用 SplitMix64 整数哈希与分层 value noise，不使用未播种随机数。
- 纤维表与孔洞表按 `(seed, layer_number, resolution)` 直接生成，不依赖层遍历顺序。
- 每层有确定性的轻微色相/亮度身份偏移，并在透射率与散射颜色上同时叠加纤维斑驳，因此材质同色的相邻层仍可区分。
- 六角孔洞由三组相隔 60° 的周期竹丝脊线并集生成，边缘带半个纹理像素的抗锯齿。

### 逐层着色

对可见片元：

- `tau = hole + coverage * transmittance * fiber_modulation`
- 透射、Lambert 散射和 Fresnel 反射分别累计。
- 反射项没有窄高光核，避免交叠处产生随视角跳变的孤立亮点。
- 使用 `0.982 * (1 - exp(-radiance))` 的软肩部映射与 sRGB gamma，避免内层惨白。
- 层 Alpha 由竹丝覆盖率与孔洞衬里共同决定；所有层先输出预乘 RGBA，再按远到近进行标准 over 合成。

### 孔洞透视与软阴影

给定接收点 `Q`、光源 `L` 与源层平面 `Z_s`：

```text
scale = (Z_receiver - L.z) / (Z_s - L.z)
P     = L + (Q - L) / scale
```

在 `P` 周围按声明密度做确定性圆盘采样。半影半径：

```text
radius_receiver = penumbra_per_gap * (Z_receiver - Z_s)
```

因此空气隙越大，投影边缘越宽。若采样点超出源层几何范围，按透射率 1（无遮挡）处理，不生成硬黑边。相邻层投影与每层直达内壁的投影分别输出灰度图；内壁最终光斑是所有前方层透射率的乘积。

## 输出契约

完整渲染目录包含：

- `rgba.png`、`rgba.npy`：最终预乘 Alpha RGBA，形状 `(H, W, 4)`，`uint8`。
- `layer_premultiplied.npy`：逐层预乘 RGBA，形状 `(L, H, W, 4)`，层顺序为内到外。
- `layers/layer_XX_premultiplied.png`：供逐层核对。
- `ownership.npy`：形状 `(H, W, 4)`。通道依次为：
  - `R`：主贡献层号，内壁为 255，背景为 0；
  - `G`：活跃层位图，低 7 位为层序号位置，最高位表示内壁；
  - `B`：最前方覆盖层号；
  - `A`：255。
- `ownership_codes.png`：`ownership.npy` 的 `R` 通道灰度分类图。
- `ownership.png`、`ownership_weights.npy`：按各层实际合成权重归一化后的 RGB 归属图。默认调色为内层红、中层绿、外层蓝、内壁灰，便于直观看出每层贡献。
- `projection/*.png`、`projection/*.npy`：每对相邻层、每层到内壁、以及合并后的投影灰度。
- `projection_points.json`：稳定投影落点、目标 UV 与是否位于目标几何内。
- `projection_report.json`：采样密度、半影宽度、5 个亮度采样点及灰度 SHA-256。
- `texture_report.json`：各层纤维表/孔洞表 SHA-256、孔洞比例与层身份色调。
- `run_report.json`：逐层透射、反射、散射、焦散能量与占比，图像统计和输出哈希。
- `snapshot.json`：逐块提交的恢复快照。
- `recovery_report.json`：仅在发生中断、恢复或丢弃时出现。

`run_report.json` 中每层同时给出：

- `transmission_share` / `reflection_share` / `scatter_share` / `caustic_share`：四类能量总和的占比；
- `transmission_vs_reflection_share` / `reflection_vs_transmission_share`：只比较透射与反射时的占比。

这组字段可直接用于判断跳变来源。默认场景的反射占比低于 0.3%，且图像纯白像素比例为 0。

## 快照与一致性

- 每个块完整计算后，将 RGBA、逐层预乘图、归属图、投影灰度与 float64 能量累计写入内存快照。
- `snapshot.json` 是单一文件，先写同目录临时文件、`fsync`，再用 `os.replace` 整体替换。
- 快照记录场景规范 JSON 摘要、算法版本、图像/分块尺寸、块编号、精确块偏移、数组形状、dtype、逐数组 SHA-256 和颜色摘要。
- 恢复时任何摘要、哈希、形状、块编号、偏移或颜色摘要不一致，均整体丢弃，不使用旧快照中的任何块。
- 块按行优先处理；恢复后最终数组与一次性完整渲染逐块同源，因此输出逐字节一致。

`compare` 会比较 `rgba.npy`、`ownership.npy`、`layer_premultiplied.npy`、`projection_points.json` 和 `rgba.png`。像素差异报告包含 `chunk_id`、`x`、`y`、差异通道与左右值；存在差异时退出码为 1。

## 实际验证结果

当前工作区已执行：

- `python -m unittest discover -s tests -v`：25 项测试全部通过。
- 默认 320×240、64×48 分块、25 块场景完整渲染：约 2–3 秒。
- 首次运行提交 7 块后中断，退出码 0；恢复复用 7 块、补算 18 块。
- 一次性完整渲染与恢复渲染的 RGBA、归属图、逐层预乘图、投影落点、`snapshot.json` 与 `run_report.json` 逐字节一致。
- `compare.json`：`difference_count=0`、`pixel_difference_count=0`。
- 纯白像素比例：`0.0`；最大亮度：`0.76815137`。
- 相邻层半影宽度：`1->2 = 4.69908523 px`，`2->3 = 5.26559182 px`。
- 内壁半影宽度：`1->wall = 16.54344303 px`，`2->wall = 12.39383545 px`，`3->wall = 7.50602546 px`。
- 纤维/孔洞表、投影灰度与所有最终输出在重复运行中字节一致。

详细数值见 `artifacts/acceptance/summary.json`，逐像素结果见 `artifacts/acceptance/full/`。

当前执行环境为 Linux 上的 Python 3.14；实现只使用 `pathlib`、`os.replace` 等跨平台接口，不依赖 shell，因此可直接在 Windows Python 3.10+ 运行。

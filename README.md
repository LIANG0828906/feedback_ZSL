# 铸铝百叶幕墙确定性离屏渲染

本项目为一个以当代美术馆为舞台的解谜游戏交付铸铝幕墙百叶阵列参考图。全部几何、程序花窗、拉丝微表面、灯位、相机、分块与快照均由本地 Python 代码确定性构造；不依赖 GPU、外部模型、贴图、数据集或网络。

目标环境：Windows 10/11、Python 3.11、NumPy 1.26。Linux/macOS 也可直接运行。

## 快速开始

PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
.\.venv\Scripts\python -m gallery_louvers build-scene --out samples\scene.json
.\.venv\Scripts\python -m gallery_louvers validate samples\scene.json
.\.venv\Scripts\python -m gallery_louvers render samples\scene.json --out artifacts\delivery
```

Linux/WSL：

```bash
python3.11 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements.txt
python -m gallery_louvers build-scene --out samples/scene.json
python -m gallery_louvers validate samples/scene.json
python -m gallery_louvers render samples/scene.json --out artifacts/delivery
```

常用命令：

```bash
# 严格校验。失败时向 stderr 输出 NDJSON，每条包含 code/layer/leaf/path，退出码 2。
python -m gallery_louvers validate samples/scene.json

# 从已提交快照续算。--fresh 会整体丢弃旧进度。
python -m gallery_louvers render samples/scene.json --out artifacts/delivery

# 不中断运行与中断后续算的逐字节比较。任何图像差异会输出
# angle/block/pixel/channel 结构化记录，并以退出码 4 结束。
python -m gallery_louvers compare artifacts/acceptance/reference artifacts/acceptance/resumed

# 全部本地验收测试，包括真实 SIGINT、损坏快照、种子变更与续算逐字节一致。
python -m unittest discover -v
```

## 输出

每个运行目录包含：

- `renders/angle_XXX_rgba_premultiplied_linear.npy`：线性 HDR、预乘 Alpha 的 `float32 H×W×4` RGBA。
- `renders/angle_XXX_rgba.png`：显示空间（sRGB、Reinhard 压缩后）预乘 Alpha 的 RGBA8 PNG。
- `renders/angle_XXX_owner_id.npy`：逐像素归属，`int32 H×W`。叶片值为 `layer*10000 + leaf + 1`；`-1` 背景、`-2` 墙、`-3` 水磨石地面。
- `renders/angle_XXX_owner_layer.npy`：`uint8 H×W`；`0..3` 为叶片层，`255` 为接收面/背景。
- `renders/angle_XXX_owner.png`：层着色归属可视化；叶号在色相内做明度变化。
- `diagnostics/wall_lightmap.png`、`diagnostics/floor_lightmap.png`：相机无关的世界空间墙面/地面光斑图。
- `diagnostics/*_lightmap_track-{L,C,R}.png`：每盏灯的单独贡献，用于比较三灯重叠。
- `landing_points.json`：每灯、每层、每叶、每花窗中心到相邻外层、墙、地面的透视落点；越界点保留坐标、`in_bounds=false`、`contribution=0`。
- `report.json`、`report.md`：逐层反射/透射比例、半影趋势、接收面能量占比、图像指标、快照与复算信息。
- `snapshot.json`：当前已提交块的完整列表、场景/花窗/代码摘要、每块颜色摘要与校验和。
- `blocks/`：不可变分块浮点结果，采用带 SHA-256 的自描述 `.gbt` 容器。
- `patterns.bin`：可独立解码的确定性花窗与拉丝微表面图集。
- `manifest.json`：最终角度文件、落点和光斑图摘要。
- `recovery.json`：仅当发生 Ctrl+C/可处理终止时存在，记录已完成块与恢复命令。
- `resume_reset.json`：仅当发现无效快照、场景摘要变化或花窗种子变化并整体丢弃时存在。

`NPY` 中 RGBA 的 RGB 通道已经乘过 Alpha。PNG 为显示空间预乘 Alpha；若需要做后期合成，优先使用线性 NPY。

## 确定性规则

- 不使用 `random`、时间戳、进程号或未声明状态生成内容。
- 花窗仅由叶片 `seed`、`layer`、`leaf`、声明分辨率和 `seed_domain` 决定。可调用 `generate_leaf_pattern(...)` 或从 `patterns.bin` 按 `(layer, leaf)` 独立解码。
- 图集、块、快照与最终 JSON 均按固定键序和固定数组布局写出；PNG 使用内置、无时间元数据的编码器。
- 光线求交按层和叶号排序；像素按全局坐标计算，与分块边界、三角形遍历顺序无关。
- 多灯贡献总和固定按灯 ID 排序累加，因此传入灯的遍历顺序不能改变 IEEE 结果。
- 快照摘要还包含渲染器源码摘要；代码变更不会误用旧块。

## JSON 场景契约

`samples/scene.json` 由 `python -m gallery_louvers build-scene` 确定性生成，主要字段：

- `layers`：恰好四层，层号 `0..3` 从内向外；每层声明 `leaf_ids`、`z_center`、`gap_to_outer_m`。
- `leaves`：每片叶片声明 `layer`、`leaf`、整数 `seed`、`thickness`、非零 `brush_direction`、三角网格 `vertices/triangles`、UV 与材料参数。
- `receivers.wall`、`receivers.floor`：接收平面轴、坐标、法线、二维范围、反照率与粗糙度。
- `lights`：恰好三盏，声明位置、目标、强度、颜色、内外锥角、衰减和有限光源半径。
- `camera`：环绕中心、半径、仰角、角度样本、FOV、裁剪面与分辨率。
- `render`：分块尺寸、花窗/拉丝分辨率、半影采样密度、厚度边界、半影模型系数和层间空隙。

坐标约定：`x` 向右，`y` 向上，`z` 朝向主相机；单位为米。

## 业务错误码

| 错误码 | 含义 |
|---|---|
| `GLV-G001` | 三角网格索引越界 |
| `GLV-G002` | 三角形退化 |
| `GLV-G003` | 叶片厚度越界 |
| `GLV-G005` | 层序号缺失、重复或非法 |
| `GLV-G004` | 叶片编号缺失、重复或与本层声明不一致 |
| `GLV-G006` | 拉丝方向为零向量 |
| `GLV-G007` | 花窗种子缺失或非整数 |
| `GLV-G008` | 灯位/强度/锥角非法 |
| `GLV-G009` | 相机环绕参数非法 |
| `GLV-G010` | 墙面或地面接收参数非法 |
| `GLV-G011` | JSON 场景结构非法 |
| `GLV-G012` | 叶片网格包围盒或厚度不一致 |
| `GLV-G013` | 渲染/半影/分块配置非法 |
| `GLV-S001` | 快照损坏或与当前场景不匹配 |
| `GLV-S002` | 续算与参考结果比较不一致 |

校验器会尽可能收集多个问题，再以退出码 `2` 结束；每条告警都包含业务错误码和适用的层序号/叶序号。

## 着色与投影摘要

- 四层叶片按 `0→3`（内→外）以预乘 Alpha 合成。每层先计算背面/正面连续着色，再叠加层间耦合：
  `0.72 + 0.28 * exp(-gap/(thickness+0.04)) * |dot(N,V)|`。该项仅依赖连续几何，不含逐片元随机阈值，因此没有 z-fighting 跳变。
- 反射使用各向异性 GGX、Schlick Fresnel、拉丝切向/副切向粗糙度与平滑微表面表；高光经声明软膝压缩与上限约束，避免斜视角形成整条纯白条。
- 透射由花窗孔径调制，并保留环境透光与背光分量；背光面因此仍能读出花窗层次。报告逐层给出反射/透射能量占比。
- 光斑采用世界空间逆映射：从接收点向灯作射线，与各层平面求交，在叶面 UV 上采样花窗；厚度衰减为 `exp(-0.55*thickness/cosθ)`。层透射按 `1-∏(1-w_l)` 累加，等价于按层序做连续 Alpha 覆盖，不会因灯序改变。
- 半影宽度：`base + a*d(leaf,receiver) + b*d(light,leaf)`。采样核按 `render.shadow_sample_density` 声明密度做归一化盘采样，宽度随两个距离单调增加。
- 半影与光斑图完全在世界空间计算，不随相机环绕漂移；相机只决定如何观察接收面。

## 快照与恢复协议

1. 每块渲染完成后，先原子替换写入独立 `.gbt` 块文件，再原子整体替换 `snapshot.json`。快照包含全部已完成块、场景摘要、花窗摘要、代码摘要、颜色摘要和自校验和。
2. 进程被强制终止时，未进入快照的块即使文件存在也不会被使用；恢复位置完全由快照连续前缀计算，不扫描旧块编号。
3. 快照 JSON 损坏、校验和不符、块校验失败、层/块偏移不连续、场景摘要变化、花窗种子变化或 Renderer 代码变化时，`blocks/renders/diagnostics/snapshot` 整体丢弃，从角度 0、块 0 重算。
4. `SIGINT`、`SIGTERM` 与 Windows `SIGBREAK` 被转换为停止请求：处理完当前块并提交快照后写出 `recovery.json`，退出码 `0`。
5. `compare` 比较全部角度的线性 RGBA、归属图、PNG、落点、光斑图；不一致时输出角度、块号、像素坐标、通道、期望值和实际值，退出码 `4`。

## 已知边界

- 花窗由连续孔径纹理调制，不生成每百万孔的显式边缘几何；参考图针对 480×360 离屏预览与游戏材质核对，不面向特写微距。
- 渲染使用 CPU NumPy。位级复现以相同 Python/NumPy/CPU 数值库为前提；场景内容摘要与图像比较不跨架构承诺 bit-identical。
- 三盏射灯均参与墙面与地面采样；若某条射线在到达接收面前未穿过任何叶片层，透射率按无遮挡 `1` 处理。

实际验收命令、结果和摘要见 `VALIDATION.md`。

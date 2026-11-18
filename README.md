# fogvol：舞台雾离线体积渲染验证组件

`fogvol` 是一个纯本地、确定性的舞台雾体渲染器。它读取 JSON 场景，
按相机视线对多倍频烟雾体素做吸收/散射积分，输出 RGBA、逐像素光学
深度、不透明度、线性散射累计和稳定排序的统计报告。渲染支持分块、
原子快照、强制终止后续算，以及续算结果与一次性渲染的逐字节核对。

不依赖网络、外部贴图或第三方体数据，仅依赖 `numpy` 与 `Pillow`。

## 快速开始

Linux/macOS：

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m fogvol make-scene --out samples/fogport.json
.venv/bin/python -m fogvol render --scene samples/fogport.json --out out/fogport --fresh
```

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python -m pip install -r requirements.txt
.\.venv\Scripts\python -m fogvol make-scene --out samples/fogport.json
.\.venv\Scripts\python -m fogvol render --scene samples/fogport.json --out out/fogport --fresh
```

运行全部测试：

```bash
.venv/bin/python -m unittest discover -v
```

中断与续算：

```bash
# 本次完成 7 个新块后正常中断，写恢复报告并返回 0
python -m fogvol render --scene samples/fogport.json --out out/resume --interrupt-after-chunks 7

# 同一命令再次运行，只从已提交快照续算
python -m fogvol render --scene samples/fogport.json --out out/resume
```

实际 `Ctrl+C` 使用同一安全路径：当前块完成并提交快照后停止，写
`recovery_report.json`，返回 0。`SIGKILL`/任务管理器强制终止不会写
退出报告，只会留下最近一次已原子替换成功的 `render_snapshot.npz`。

## 场景格式

完整示例见 `samples/fogport.json`。核心字段：

| 字段 | 含义 |
| --- | --- |
| `seed` | 非负整数，驱动所有多倍频噪声相位 |
| `volume.dimensions` | `[nx, ny, nz]`，均为正整数 |
| `volume.origin` / `voxel_size` | 体素世界坐标原点与尺寸，尺寸均大于 0 |
| `volume.density_range` | `[min, max]`，密度声明量程 |
| `volume.density` | 可选扁平数组或同维度数组；缺省时由 `seed` 生成 |
| `volume.noise` | 倍频数、基频、lacunarity、gain、地面/顶部分层参数 |
| `camera.position/target/up/fov_deg` | 透视相机参数 |
| `camera.width/height/aspect` | 正整数尺寸，`aspect` 必须等于 `width/height` |
| `lights[]` | 舞台灯 ID、类别、位置、朝向、颜色、强度、内外锥角、范围 |
| `render.step` | 正整数；一个声明积分区间的长度 |
| `render.substeps` | 正整数；每个声明区间内的确定性求积点数 |
| `render.max_samples` | 正整数；单条光线的总采样上限 |
| `render.chunk_size` | `[块宽, 块高]`，均为正整数 |
| `render.tonemap` | `linear`、`reinhard` 或 `aces` |
| `render.channel_max` | 输出通道上限，位于 `(0, 1]` |

`samples/fogport.json` 默认不内嵌密度，由种子现场生成。需要把体素也
写入 JSON 时使用：

```bash
python -m fogvol make-scene --out samples/fogport_embedded.json --embed-density
```

渲染器对“内嵌体素”和“种子生成体素”执行同一套长度、有限性、量程
校验。所有位置、方向、颜色、强度、锥角、相机参数均验证为有限值。
灯的严格内部位置 `origin < p < origin + dimensions*voxel_size` 被拒绝。

### 业务错误

校验或运行失败时，只向 stderr 输出一个 JSON 文档，并以非零码退出：

```json
{
  "ok": false,
  "error": {
    "code": "DENSITY_LENGTH_MISMATCH",
    "path": "volume.density",
    "message": "体素数量 7 与维度乘积 53760 不一致"
  }
}
```

输入校验发生在创建输出目录和渲染之前，因此不会留下部分 RGBA 或
深度图。主要业务码包括：

- `DENSITY_LENGTH_MISMATCH`、`DENSITY_SHAPE_MISMATCH`
- `NONFINITE_DENSITY`、`DENSITY_OUT_OF_RANGE`
- `INVALID_FOV`、`INVALID_ASPECT`、`ASPECT_MISMATCH`
- `LIGHT_INSIDE_VOLUME`、`INVALID_LIGHT_CONE`
- `INVALID_INTEGER`（步长、采样上限、分块尺寸等）
- `RESUME_PIXEL_MISMATCH`、`STEP_CONVERGENCE_FAILED`

## 渲染规则

### 确定性密度

`fogvol/density.py` 用整数种子构造每一倍频的晶格值和相位，再在体素
世界坐标上直接求多倍频值噪声。没有逐体素状态，也没有未播种随机数。
近地面采用高密度翻滚层，顶部采用低密度分层薄雾。同一场景多次生成
逐字节相同；报告写入均值、最小/最大值及其体素坐标和世界坐标。

### 视线积分

1. 像素中心为 `(x+0.5, y+0.5)`，先构造透视相机光线。
2. 与雾体 AABB 求交；相机在雾外时从交点进入，在雾内时从相机开始。
3. 实际数值采样间距 `integration_step = step / substeps`。第一个节点
   位于 `ray_box_entry + 0.5*integration_step`。
4. 用三线性插值读取密度，雾体 AABB 外密度严格为零。
5. 每个节点把密度换算为消光 `sigma_t` 和散射 `sigma_s`；使用当前
   相机透射率及区间解析透射因子累计散射。
6. 每盏灯计算聚光灯锥、距离/范围衰减、Henyey-Greenstein 相位，
   并沿“采样点至灯”的路径再次积分雾内光学深度。
7. 相机路径累计 `optical_depth = integral(sigma_t dt)`；
   `opacity = 1 - exp(-optical_depth)`。

每个声明区间内使用 4 个默认求积节点，是为了在整数步长下抑制与体素
采样相位相关的棋盘/台阶状纹理。PNG 元数据和报告同时记录
`step`、`substeps`、`integration_step` 及起点规则，不隐瞒实际间距。

### 色调映射与输出

线性散射累计先乘曝光，再按场景选择 ACES、Reinhard 或线性映射，最
后做 `1/gamma` 编码并钳制到 `[0, channel_max]`。RGBA 的 RGB 是编码
亮度，A 是光学不透明度。`rgba.npy` 保留编码后的 `float64` 数据。

输出文件：

| 文件 | 内容 |
| --- | --- |
| `rgba.png` | 8 位 RGBA，内嵌步长、起点规则、摘要等元数据 |
| `rgba.npy` | `H x W x 4` 浮点编码结果 |
| `radiance.npy` | 色调映射前的线性散射累计 |
| `optical_depth.npy` | 逐像素光学深度 `float64` |
| `opacity.npy` | 逐像素不透明度 `float64` |
| `optical_depth.png` | 16 位深度可视化，比例 `8192` 写在报告/元数据 |
| `opacity.png` | 16 位不透明度可视化 |
| `report.json` | 稳定键序统计报告 |

## 快照与恢复

每个块完成后，完整缓冲区、完成掩码和 JSON manifest 一起写入临时
NPZ，`fsync` 后用 `os.replace` 原子替换 `render_snapshot.npz`。快照
manifest 包含：

- 场景 SHA-256 摘要和渲染签名；
- 已完成块编号、分块布局、步长、子步数、采样上限；
- 每块平均 RGB、平均/最大亮度摘要；
- 缓冲内容摘要与累计恢复次数。

续算时块偏移只由当前场景重新计算；快照只提供完成掩码和已提交数据。
场景摘要、相机/渲染签名、块布局或步长任一不同，或者 NPZ/缓冲校验
失败，整个快照会被删除并从零开始。

只要本次运行使用过快照，完成前会再在内存中做一次一次性完整渲染，
逐数组并逐 PNG 比较 RGBA、光学深度、不透明度和线性累计。若不同，
写 `resume_diff.json`，包含 `chunk_id`、`pixel`、`channels`、首差值，
退出码为 3，不写最终图像。

报告只含相对文件名，不含时间戳和绝对路径。

## 报告字段

`report.json` 按键名排序，主要内容：

- `density`：均值、极值、极值体素/世界坐标、声明量程；
- `convergence`：声明步长、半步步长、实际积分间距、平均/最大相对
  偏差、容差、通过状态、最差像素及其块号；
- `lighting`：每盏灯的积分亮度与贡献占比；
- `sample_points`：每盏灯峰值及全图高亮点的 RGBA、亮度、透明度、
  光学深度、逐灯亮度和主控灯；
- `tonemapping`：钳制通道数、钳制像素数、通道上限、编码范围；
- `smoothness`：浓雾区相邻亮度梯度分位数和块边界梯度；
- `resume`：恢复次数、复用块数、快照加载状态、一次性渲染核对结果。

## 本地验收结果

以 `samples/fogport.json` 实测（Python 3.14、NumPy 2.5.5、
Pillow 13.0.0）：

- 体素数 `53,760`，图像 `160 x 120`；
- 声明步长 `1`，子步数 `4`，实际积分间距 `0.25`；
- 半步步长 `0.5`，实际间距 `0.125`；
- 平均相对亮度偏差 `0.010350422`，容差 `0.06`，通过；
- 最差像素 `[111, 0]`，块 `r000_c005`；
- 密度均值 `0.213954312`，最大 `0.836964602`（体素 `[0, 0, 24]`）；
- 地灯贡献占比 `0.298816626`，侧灯 `0.701183374`；
- 被钳制像素 `0`，编码最大通道 `0.532128909`。

正常中断 7/64 块后恢复：`recovery_count = 1`，复用 7 块；最终
`rgba.png`、`rgba.npy`、`optical_depth.npy`、`optical_depth.png`、
`opacity.npy`、`opacity.png`、`radiance.npy` 与不中断运行逐字节一致。
实测 `SIGKILL` 后仅保留快照（无恢复/最终报告），重启续算同样通过
一次性渲染核对。

16 项自动化测试覆盖：

- 非法密度长度/NaN/越界、相机、灯内位置和整数步长；
- 密度重复生成逐字节一致、地面/顶部统计关系；
- 地灯与侧灯瞄准变化导致光片质心移动；
- 薄到中等雾密度下同一灯亮度单调；
- 步长收敛、分块划分不影响连续积分、通道上限；
- 正常中断、仅凭快照恢复、损坏/场景错配快照丢弃；
- 篡改快照时输出结构化块/像素/通道差异；
- 报告无时间戳、无绝对路径且键序稳定。

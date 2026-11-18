# 哥特式玫瑰窗离线软光栅验证器

这是一个只依赖 Python + NumPy 的确定性离线渲染组件，用于在合入正式关卡前核对彩绘玻璃的透射、铅条投影、软阴影、釉面反射和分块恢复行为。

## 环境与安装

- Windows 10/11（也支持 Linux/macOS）
- Python 3.10+
- NumPy 1.24+
- 不需要 GPU、网络、外部素材或容器

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.txt
```

Linux/macOS：

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
```

## 快速运行

```powershell
# 1. 生成确定性样例（仓库中已包含 scenes/rose_window.json）
.\.venv\Scripts\python.exe -m rosewindow sample --output scenes\rose_window.json

# 2. 校验并完整渲染
.\.venv\Scripts\python.exe -m rosewindow render --scene scenes\rose_window.json --out-dir out

# 3. 中断后使用同一命令恢复；已提交块不会重算
.\.venv\Scripts\python.exe -m rosewindow render --scene scenes\rose_window.json --out-dir out

# 4. 两次完整输出逐字节比较
.\.venv\Scripts\python.exe -m rosewindow verify --reference out_baseline --candidate out
```

入口也可写为：

```bash
python render_rose_window.py --scene scenes/rose_window.json --out-dir out
python -m rosewindow --scene scenes/rose_window.json --out-dir out
```

`--fresh` 会丢弃当前快照并从块 0 重算。`--chunk-delay-ms` 是测试钩子，用于在每块提交后制造可终止窗口，不参与图像计算。

## 输出

成功完成后，输出目录包含：

| 文件 | 内容 |
| --- | --- |
| `rgb.png` | 最终 RGB 图像，固定压缩参数、无时间戳，字节可复现 |
| `rgb.npy` | `uint8 (H,W,3)` 原始 RGB，用于逐像素比较 |
| `ownership.npy` | `int32 (H,W)` 可见玻璃归属图，0 表示非玻璃 |
| `ownership.png` | 归属图可视化；不同片序号使用固定伪彩色 |
| `ownership_map.json` | 1 基片序号到 `(quadrant, piece_index)` 的映射 |
| `shadow_projections.json` | 每条去重铅条边的玻璃端点、落点、深度端点、偏移和中点 |
| `report.json` | 稳定统计报告；不含时间、路径、耗时等运行态字段 |
| `run.json` | 本次运行的恢复状态、计算块和耗时 |
| `recovery_report.json` | 仅在可处理中断后存在，记录已提交块和剩余块 |

`report.json` 中的关键字段：

- `texture.piece_digests`：每片玻璃斑驳表的 SHA-256。
- `transmission.forward_blue_probe`、`lateral_red_probe`：代表片的平均 RGB、入射余弦和合成占比。
- `reflection.piece_contributions`：每片玻璃的透射能量、反射能量、占比、平均每通道贡献。
- `lead_shadow.profile.depths`：同一采样密度下不同铅条深度的投影灰度剖面和过渡宽度。
- `chunks.summaries`：每块的 RGB/归属摘要、均色和像素数。

## 场景 JSON 契约

完整样例见 `scenes/rose_window.json`。顶层字段：

```json
{
  "schema_version": 1,
  "name": "rose-window-validation-01",
  "seed": 20261118,
  "quadrants": [0, 1, 2, 3],
  "image": {
    "width": 640,
    "height": 400,
    "chunk_size": 32,
    "gamma": 2.2
  },
  "camera": {
    "position": [2.4, -0.55, 2.0],
    "target": [0.0, 0.05, 2.0],
    "up": [0.0, 1.0, 0.0],
    "vertical_fov_degrees": 92.0
  },
  "window": {
    "center": [0.0, 0.0, 0.0],
    "normal": [0.0, 0.0, 1.0],
    "surround_outer_radius": 1.68
  },
  "wall": {
    "z": 4.0,
    "width": 4.3,
    "height": 4.2,
    "center_xy": [-0.35, 0.0],
    "block_size": [0.46, 0.28],
    "color": [0.315, 0.295, 0.262]
  },
  "light": {
    "direction": [0.34, -0.16, 1.0],
    "color": [1.0, 0.955, 0.885],
    "intensity": 1.78,
    "angular_radius": 0.022
  },
  "glass_pieces": []
}
```

每片玻璃必须包含：

```json
{
  "quadrant": 0,
  "piece_index": 0,
  "mesh": {
    "vertices": [[0.0, 0.0, 0.0], [0.1, 0.0, 0.0], [0.0, 0.1, 0.0]],
    "triangles": [[0, 1, 2]]
  },
  "transmission_color": [0.075, 0.205, 0.93],
  "transmittance": 0.7,
  "lead_width": 0.018,
  "lead_depth": 0.03,
  "enamel_reflectance": 0.05
}
```

约束如下：

- `quadrant`、`piece_index` 为非负整数；`(quadrant, piece_index)` 必须唯一。
- 若提供 `quadrants`，其元素必须唯一，且每片玻璃的象限必须已声明。
- 三角形索引必须是有效整数；重复顶点索引或面积小于 `mesh_epsilon` 的三角形会被拒绝。
- `transmittance`、`enamel_reflectance`、`transmission_color` 通道必须在 `[0,1]`。
- `lead_width`、`lead_depth` 必须非负。
- 方向光不得与 XY 墙平面平行；墙为 `z = wall.z`。

校验在任何输出文件创建之前完成。失败时 stderr 输出单行结构化 JSON，例如：

```json
{"ok":false,"error":{"code":"RW_E_MESH_INDEX","quadrant":0,"piece_index":5,"message":"..."}}
```

退出码：

| 码 | 含义 |
| --- | --- |
| `0` | 完成，或可处理中断已完成当前块提交并写出恢复报告 |
| `2` | 场景校验失败，未输出图像 |
| `3` | 非校验类渲染/IO 错误 |
| `4` | 与参考输出比较不一致；结构化差异已写出 |

## 实现要点

### 纹理

每片玻璃的纹理种子由 `(scene.seed, quadrant, piece_index, channel_salt)` 经 SplitMix64 整数哈希得到，生成两级循环平滑值噪声。生成过程不使用 `np.random` 或全局随机状态，并按稳定片序号存储，因此同一参数、不同遍历顺序得到逐字节相同结果。透射表和反射表分别调制颜色与釉面反射率。

### 透射与反射

逐片计算：

```text
T_linear = color * transmittance * mottling * (ambient + light_intensity * cos(theta))
F        = Schlick(F0 * reflection_texture)
out      = T_linear * (1 - F) + environment_color * intensity * F
```

反射和透射不是简单相加，权重 `(1-F, F)` 保持能量关系。合成后的高光若超过 1，按三通道整体缩放以保色相，随后使用 `1-exp(-x*exposure)` 与 gamma 编码。最终缓冲为 `uint8`，因此不会通道溢出；报告同时检查正对光源蓝片未粉化、侧向红片仍有可辨认色相。

### 铅条与软阴影

每片网格中只被一个三角形使用的边视为片边界，跨片按端点去重。铅条沿边界生成带宽度和深度的挤出体，参与玻璃层遮挡。每条边按方向光投影到 `z=wall.z`：

```text
landing = glass_xy + light_xy * (wall_z - glass_z) / light_z
```

投影纹理在每个深度切片上使用确定性的分层采样积分，半影宽度由方向光角半径、墙距离和铅条深度共同决定，再做双线性采样。报告以同一最长探针边和同一采样密度输出多组灰度剖面，可直接核对投影偏移和过渡宽度趋势。

### 分块快照与恢复

- 块顺序、块坐标和块数量始终由当前场景重新计算，不读取快照中的偏移。
- 每完成一块，把完整 RGB、归属、贡献累计、已完成块集合和每块颜色摘要写入临时文件，`fsync` 后以单次 `os.replace` 整体替换 `snapshot.npz`。
- 快照包含场景摘要、渲染器快照版本、尺寸、块大小、逐块颜色摘要及全载荷 SHA-256。
- 场景摘要不匹配时整体丢弃；ZIP 损坏、载荷校验失败、块号越界、摘要不一致时同样整体丢弃并从块 0 重算。
- `SIGINT`/`SIGTERM` 只设置中断标志。当前块完成后才提交快照、写 `recovery_report.json` 并退出 `0`；`SIGKILL` 不保证报告，但已提交的原子快照仍可恢复。
- 完成后的 `report.json` 不含运行态数据，因此完整运行与恢复运行的报告应逐字节相同。

## 验证

```powershell
.\.venv\Scripts\python.exe -m unittest discover -s tests -t . -v
```

测试覆盖：业务码校验、纹理顺序无关性、透射色相、反射占比、软投影确定性、深度趋势、归属图、PNG 确定性、结构化差异、`SIGINT`、损坏快照丢弃和 `SIGKILL` 恢复逐字节一致。

本机实际结果和哈希记录见 `VALIDATION.md` 与 `artifacts/validation_summary.json`。

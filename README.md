# TerrainViz

纯 NumPy 的确定性 CPU 地形可视化管线，覆盖自定义高度场加载、分块四叉树、屏幕误差 LOD、相邻块接缝修补、法线缓存、材质双线性混合、离屏颜色/深度/误差热力图、原子快照和协作式取消。运行时不使用 GPU、OpenGL 或其他第三方库。

## 环境

- 目标环境：Windows 10/11 + Python 3.11。
- 唯一运行时第三方依赖：`numpy>=1.26`。
- 推荐安装：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
python -m pip install numpy
python -m pip install -e .
```

开发机实际验证环境为 WSL2 Ubuntu 26.04、Python 3.14.4、NumPy 2.5.5；代码未使用平台专属 API。详细的 Windows/本地验证记录见 [VALIDATION.md](VALIDATION.md)。

## 快速运行

```powershell
terrainviz generate --out .\demo_out\samples --seed 20261120
terrainviz render --terrain .\demo_out\samples\mountain.thf --material .\demo_out\samples\mountain.matidx --out .\demo_out\mountain --threshold 1.5 --seam-tolerance 1.0 --snapshot .\demo_out\mountain\snapshot.json
terrainviz demo --out .\demo_out
terrainviz snapshot --snapshot .\demo_out\snapshots\mountain.json
```

未安装包时，在仓库根目录用 `$env:PYTHONPATH="src"`（PowerShell）或 `set PYTHONPATH=src`（cmd）后执行 `python -m terrainviz ...`。

`demo` 输出：

- `samples/plain.thf`、`mountain.thf`、`mixed.thf` 及同名 `.matidx`。
- `runs/<terrain>/color.npy`、`depth.npy`、`error_heatmap.npy`。
- `snapshots/<terrain>.json`。
- `logs/baseline_trajectory.jsonl`、`logs/demo_trajectory.jsonl`。
- `report.json`，包含层级表、预算用量、误差上界、接缝状态、法线复用清单和确定性检查。

## 数据格式

所有整数与浮点均使用小端序。

高度场 `.thf`：

| 字段 | 类型 | 说明 |
|---|---|---|
| magic | 8 bytes | `THF1\r\n\x1a\n` |
| version | uint32 | 当前为 1 |
| width, height | uint32 | 样本数，均至少为 2 |
| elevation_type | uint32 | 1=`float32`，2=`float64` |
| spacing | float64 | 格距，必须为正 |
| quantization_step | float64 | 高程量化步长，必须为正 |
| valid_min, valid_max | float64 | 有效高程范围 |
| payload_sha256 | 32 bytes | 原始高程负载 SHA-256 |
| reserved | 8 bytes | 必须为 0 |
| payload | float32/float64[] | 行优先原始高程 |

头部固定 96 字节。缺失、版本错误、尺寸/类型/范围非法、负载长度或哈希不符均以 `E_HEIGHT_INVALID` 中止。

材质 `.matidx`：

- 64 字节头：`TMAT1\r\n\x00`、版本、宽、高、item size、负载 SHA-256、保留字段。
- 负载为行优先 `uint8` 材质索引。
- 渲染时按世界坐标双线性采样四邻域索引，并混合固定调色板颜色。

## 管线与误差模型

1. `HeightField` 校验并保存原始高程，并提供按有效范围映射到 `[0,1]` 的归一化视图；几何与误差预算仍使用世界单位，材质/无材质着色使用归一化高程。`Terrain` 按 `block_size` 个四边形切块，再建立块级四叉树。叶节点真值来源为 `heightfield`，父节点为 `derived_from_heightfield`。
2. 每块对每个候选 LOD 计算粗网格相对原始格点的最大垂直偏差，再与 `quantization_step` 相加，得到世界空间误差上界。
3. 使用针孔相机焦距和块包围盒距离将误差投影到像素：

```text
projected_px = error_world * (viewport_height / (2*tan(fov_y/2))) / distance
```

4. 在不超过屏幕阈值的候选层级中选择最粗层级；没有候选满足时回退到 level 0 并在报告中标记 `fallback=true`。level 0 为全分辨率，每升一级步长乘 2。
5. 相邻块层级不同时，先按容差迭代降低较粗块层级。最终在共享边取两侧顶点位置的并集，两侧都使用高度场真值高程，生成匹配顶点边界；报告 `pre_repair_deviation` 与 `post_repair_deviation`。
6. 法线由中心差分估算，边界使用单边差分；零梯度固定为 `(0,1,0)`。缓存键包含块高程、格距和块材质字节，因此只重算实际受影响的块。
7. NumPy 光栅器输出 `uint8` 颜色、`float32/float64` 深度和逐像素预算热力图。固定输入连续渲染逐字节一致；切换精度后报告颜色像素差与深度差统计。

## 错误码

- `E_HEIGHT_INVALID`：高度场/材质头、尺寸、哈希、类型、有限性或有效范围错误。
- `E_CRACK_UNRESOLVED`：接缝经允许的迭代后仍超出容差；详情包含 `seam_id`、两侧块编号、两侧边和偏差。
- `E_SNAPSHOT_INVALID`：快照缺失、字段缺失、版本错误、内容哈希不符或与源文件不匹配。
- `E_BUSY`：同一地形已有渲染任务。
- `E_CANCELLED`：在 LOD/法线/块边界检查点响应协作式取消。
- `E_INVALID_ARGUMENT`：块尺寸、格距、相机、阈值等参数非法。

## 快照与恢复

快照保存视点矩阵、屏幕阈值、块层级表、材质版本、法线清单、误差摘要、接缝摘要、缓冲区哈希和树构建信息。写入使用同目录临时文件、`fsync` 和 `os.replace` 原子提交。

`load_snapshot` 校验格式、版本和内容哈希；`snapshot_changes` 识别视点、层级、材质和阈值变化。`recover_terrain_from_disk` 只依据已提交快照、高度场文件及可选材质文件重建地形，并校验高度内容摘要。

进程被强制终止时，本工具不承诺生成退出报告；恢复入口只使用磁盘上最后一次原子提交的快照和源文件。

## 测试

```powershell
$env:PYTHONPATH="src"
python -m unittest discover -s tests -v
```

测试覆盖非法头、四叉树重建、LOD 预算确定性、接缝闭合与失败定位、法线默认值/增量复用、颜色/深度确定性、精度差异、快照篡改、`E_BUSY`、取消时保留快照，以及跨目录逐字节复现。

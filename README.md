# volverify

离线、确定性、可恢复的体积雾渲染验证组件。输入是 JSON 场景、三维规则密度体、
相机、点光源/方向光和传输函数分段表；输出 RGBA 帧、逐像素光学深度、传输函数
扫描表、可疑像素定位、渲染统计与结构化差异报告。

组件不依赖显卡、外部素材或网络。密度体和传输函数由项目内的确定性生成器产生。

## 快速开始

Python 3.10+，安装 NumPy 与 Pillow：

```bash
python -m pip install numpy Pillow
```

Windows 可先安装对应 Python 版本的 NumPy/Pillow wheel，再执行相同命令。本项目
开发验证环境为 Python 3.14.4、NumPy 2.5.5、Pillow 13.0.0。

生成确定性样本并验证：

```bash
python -m volverify sample --out samples/demo
python -m volverify validate \
  --scene samples/demo/valid/scene.json \
  --report samples/demo/validation.json
python -m volverify render \
  --scene samples/demo/valid/scene.json \
  --out samples/demo/out-valid
```

批量处理会逐帧继续，坏帧不会中断整批：

```bash
python -m volverify batch \
  --manifest samples/demo/batch_manifest.json \
  --out samples/demo/batch-out
```

中断后续算：

```bash
python -m volverify resume \
  --scene samples/demo/valid/scene.json \
  --out samples/demo/out-valid \
  --compare-full samples/demo/full-baseline
```

比较两次结果：

```bash
python -m volverify compare \
  --scene samples/demo/valid/scene.json \
  --left samples/demo/full-baseline \
  --right samples/demo/out-valid \
  --report samples/demo/diff.json
```

运行测试：

```bash
python -m unittest discover -s tests -v
```

## 输出

每个帧目录包含：

- `frame.rgba.png`：8 位 RGBA，RGB 为 straight alpha，alpha 是不透明度。
- `frame.depth.f32`：C 顺序、小端 `float32` 逐像素光学深度。
- `frame.stats.json`：帧摘要哈希、采样数、收敛偏差、光源贡献、可疑像素、
  分块完成情况和输入哈希。
- `transfer_scan.json`：越界钳制、每个分段的求值结果和边界连续性检查。
- `suspicious_pixels.json`：不透明度跳变像素对、坐标、差值及两侧密度体采样区间。
- `recovery_report.json`：仅在优雅中断或快照输入不匹配时写出。
- `resume_report.json`：从已有持久化块继续时写出。
- `diff.json`：增量/全量比较或 `compare` 时写出。
- `.volverify-state/`：块数组、SHA-256 清单和累计摘要。

PNG 的 `volverify` 文本元数据记录实际使用的步长范围、光线起点规则、像素中心
方向规则、深度文件格式和收敛结果。

## 场景格式

完整可运行样例由 `sample` 命令生成。核心结构如下：

```json
{
  "schema_version": 1,
  "frame_id": "valid",
  "volume": {
    "dims": [56, 44, 36],
    "bounds": {"min": [-0.6, -0.45, -0.4], "max": [0.6, 0.45, 0.4]},
    "density_range": [0.0, 1.0],
    "data_file": "volume.f32",
    "dtype": "float32",
    "order": "C",
    "sha256": "..."
  },
  "camera": {
    "position": [0.0, 0.0, 2.15],
    "look_at": [0.0, 0.0, 0.0],
    "up": [0.0, 1.0, 0.0],
    "fov_y_degrees": 48.0,
    "aspect_ratio": 1.3333333333333333,
    "width": 64,
    "height": 48
  },
  "lights": [
    {
      "id": "key",
      "type": "point",
      "position": [-0.34, 0.42, 0.62],
      "color": [1.0, 0.72, 0.46],
      "intensity": 3.2,
      "phase_g": 0.16,
      "min_distance": 0.03
    },
    {
      "id": "sun",
      "type": "directional",
      "direction": [-0.22, -0.38, -0.9],
      "color": [0.62, 0.76, 1.0],
      "intensity": 2.2,
      "phase_g": 0.1
    }
  ],
  "transfer": {
    "density_range": [0.0, 1.0],
    "transition_width": 0.04,
    "segments": [
      {
        "range": [0.0, 0.25],
        "absorption": [0.14, 0.42],
        "emission": [[0.0, 0.0, 0.0], [0.025, 0.035, 0.055]]
      }
    ]
  },
  "sampling": {
    "reference_step": 0.02,
    "refinement_factor": 2.0,
    "convergence_levels": 3,
    "convergence_tolerance": 0.02,
    "suspicious_opacity_jump_threshold": 0.02
  },
  "render": {
    "chunk_size": [22, 17],
    "exposure": 3.0,
    "gamma": 2.2
  }
}
```

字段约定：

- `dims` 是 `[nx, ny, nz]`，三轴长度可不同。
- `data_file` 可按 `float32/float64`、`C/F` 顺序存储；文件长度必须严格等于
  `nx * ny * nz * dtype.itemsize`。
- `look_at` 与 `direction` 二选一；`aspect_ratio` 可省略，若给出必须与
  `width / height` 一致。
- 分段的 `absorption` 可为常数或 `[start, end]` 线性端点；`emission` 可为常数
  RGB 或 `[start_rgb, end_rgb]`。
- 分段必须连续覆盖 `transfer.density_range`。边界值不同时必须设置
  `transition_width > 0`，用 smoothstep 过渡；否则整帧拒绝。
- `lights` 按 `id` 排序后积分，场景中光源书写顺序不会改变输出。

## 积分与采样规则

对每个像素中心发射透视光线，与密度体外包围盒求交：

1. 起点取 `max(0, AABB_entry)`，相机在体内时从 `t=0` 开始。
2. 从体积入口按 `reference_step` 切区间，区间中点为采样点。
3. 余量不足一个步长时，最后一个区间使用剩余长度；这使每条光线完整覆盖
   `[entry, exit]`，并避免分块边界改变采样相位。
4. 三次线性插值密度；密度体外不产生吸收、自发光或光源贡献。
5. `sigma_t = density * transfer.absorption`，
   `self_source = density * transfer.emission`。
6. 点光源使用 `intensity / max(distance, min_distance)^2` 和
   Henyey-Greenstein 相位；方向光使用固定入射方向与相位。
7. 每个区间按 `T * source * dt` 累积辐射亮度，并按
   `T *= exp(-sigma_t * dt)` 更新透射率；`optical_depth = sum(sigma_t * dt)`。
8. `alpha = 1 - exp(-optical_depth)`；RGB 使用固定指数/伽马映射。

收敛验证至少运行参考步长及两级细分，比较线性亮度。相对偏差定义为
`mean(|Y_coarse - Y_fine| / Y_fine)`（仅统计有信号的像素）；末级偏差必须
不超过容差，且随步长缩小不得上升。失败时不写图像。

## 可疑像素

对每个像素计算不透明度。检查横向和纵向相邻像素对；当绝对不透明度差超过
`suspicious_opacity_jump_threshold` 时，输出：

- 两像素坐标、不透明度和差值；
- 两像素的 `[t_enter, t_exit]`；
- 采样经过的体素网格最小/最大索引；
- 峰值密度、峰值位置对应的体素索引。

这样可以直接回到密度体检查断层所在区域。

## 快照、恢复与差异

- 每完成一个块，先原子写入块数组，再原子更新 `manifest.json` 和累计摘要。
- 强杀后重启只读取清单中已校验的块；未写清单的孤立块不会使用。
- 输入指纹、体数据哈希、分块布局哈希或块数组哈希不匹配时，旧状态整体移动到
  `invalidated-state-*`，从块 0 重新计算，不复用旧行偏移。
- `SIGINT`/`SIGTERM` 写 `recovery_report.json`，返回 `128 + signal`。
- `--compare-full` 或 `compare` 对分块、像素坐标、RGBA/深度差值给出结构化报告。

如需在自动化测试中稳定制造“处理中途”状态，可设置
`VOLVERIFY_CHUNK_DELAY_MS`。它只延迟块间切换，不影响输出内容。

## 业务码与退出码

常见稳定业务码：

| 业务码 | 含义 |
| --- | --- |
| `SCENE_JSON_INVALID` | JSON 语法或根对象错误 |
| `VOLUME_DIMS_INVALID` | 维度非法 |
| `VOLUME_TRUNCATED` | 体素文件短于 `dims * dtype` |
| `VOLUME_LENGTH_MISMATCH` | 体素文件存在多余字节 |
| `VOLUME_HASH_MISMATCH` | 声明哈希与实际字节不符 |
| `VOLUME_NONFINITE` | 密度含 NaN/Inf，带首批索引 |
| `VOLUME_RANGE_VIOLATION` | 密度超出声明量程，带首批索引 |
| `CAMERA_FOV_INVALID` | 视场角不在 `(0, 180)` |
| `CAMERA_ASPECT_INVALID` | 宽高比非法或与图像尺寸不一致 |
| `LIGHT_INSIDE_VOLUME` | 点光源严格位于密度体内部 |
| `TF_INTERVAL_OVERLAP` | 分段区间重叠 |
| `TF_INTERVAL_GAP` | 分段区间存在间隙 |
| `TF_RANGE_UNCOVERED` | 分段表未覆盖整个密度量程 |
| `TF_BOUNDARY_DISCONTINUITY` | 边界跳变且没有过渡宽度 |
| `TF_TRANSITION_INVALID` | 过渡宽度非法或覆盖多个段 |
| `CONVERGENCE_FAILED` | 细分不收敛或不单调，整帧拒绝 |
| `SNAPSHOT_INPUT_MISMATCH` | 持久化状态与当前输入不符，整体作废 |
| `SNAPSHOT_CORRUPT` | 快照清单或块文件损坏 |
| `INCREMENTAL_RESULT_MISMATCH` | 增量/全量像素或深度不一致 |

退出码：`0` 成功；`1` 未预期内部错误；`2` 输入/收敛拒绝或批次含拒绝；
`4` 差异报告发现不一致；`130`/`143` 分别为 SIGINT/SIGTERM 优雅中断。

## 已验证结果

本次在本地确定性 64x48 图像、56x44x36 密度体上实测：

- 收敛偏差：`2.273304e-3`（0.02 -> 0.01）和 `9.668527e-4`
  （0.01 -> 0.005），容差 `0.02`，单调收窄并通过。
- 最终参考步长总采样数：`29336`。
- 可疑像素：`669` 个相邻像素对，涉及 `736` 个像素。
- 光源贡献占比：`key=0.246304`、`rim=0.356230`、`sun=0.134471`、
  自发光 `0.262994`。
- 两次全量运行的 PNG、深度、统计、扫描表和可疑像素文件逐字节一致。
- 真实 `SIGKILL` 在 2/9 块后终止，续算结果与全量基线 `identical`。
- 真实 `SIGTERM` 返回 `143` 并写恢复报告；续算与全量基线一致。
- 改变分块尺寸并逆序光源后，图像和深度逐字节不变。
- 自动化测试共 12 项，全部通过。

更详细的数学解释、快照状态机和验证命令见 `TECHNICAL.md`。

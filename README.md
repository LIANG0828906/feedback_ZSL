# SoftRacer：夜间篝火透明粒子与软阴影离线验证器

这是一个从零实现、无第三方运行时依赖的 C++17 离线软光栅组件。它使用确定性的 JSON
场景生成、CPU 逐像素渲染、原子分块快照和 SHA-256 摘要，用于验证透明粒子层间合成、
透射累积、多光源地面软阴影、半影趋势以及中断续算一致性。

工程不读取模型、纹理、GPU 或网络资源。内置篝火场景由代码确定性构造，同一输入在同一
工具链上重复运行会得到逐字节一致的 RGBA、归属图、PNG、阴影落点和统计报告。

## 构建

### Windows 10/11 + Visual Studio 2022

在 “x64 Native Tools Command Prompt for VS 2022” 中执行：

```bat
cmake -S . -B build -G "Visual Studio 17 2022" -A x64
cmake --build build --config Release
```

可执行文件位于 `build\Release\softracer.exe`。如已安装 CTest：

```bat
ctest --test-dir build -C Release --output-on-failure
```

或直接运行验收脚本：

```bat
python tests\run_tests.py --binary build\Release\softracer.exe --work build\acceptance
```

### Linux（本次实际验证环境）

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
python3 tests/run_tests.py --binary ./build/softracer --work build/acceptance
```

本工作区实际使用 GCC 15.2、CMake 3.30.5 完成编译和验收；代码路径同时包含 MSVC 与
Windows 原子替换、控制台中断处理分支。

## 命令

```text
softracer generate --out <scene.json>
softracer validate --scene <scene.json> [--out <report-dir>]
softracer render --scene <scene.json> --out <output-dir> [--frame N]
                  [--snapshot <snapshot.bin>] [--block-delay-ms N]
softracer compare --reference <dir> --candidate <dir> [--out <difference.json>]
```

测试钩子：

- `--crash-after-new-commits N`：提交 N 个新分块后调用 `_Exit(86)`，模拟不可捕获的强制终止。
- `--block-delay-ms N`：每块提交后等待 N 毫秒，便于发送 `Ctrl+C`。

典型流程：

```bash
./build/softracer generate --out scene.json
./build/softracer render --scene scene.json --out result
./build/softracer render --scene scene.json --out result
./build/softracer compare --reference result --candidate result \
  --out result/difference_report.json
```

退出码：

- `0`：成功，或收到可处理 `Ctrl+C` 后完成当前块并写出恢复报告。
- `2`：命令行、JSON 或场景业务校验失败。
- `3`：比较发现逐像素或阴影落点差异。
- `4`：I/O 或内部运行错误。
- `86`：仅用于测试钩子的强制终止模拟，不属于正常业务退出码。

## 场景格式

完整示例可通过 `softracer generate` 得到。核心字段如下：

```json
{
  "image": {
    "width": 480,
    "height": 320,
    "tile_width": 80,
    "tile_height": 80,
    "pixel_samples": 2
  },
  "render": {
    "layer_count": 22,
    "default_frame": 0,
    "shadow_samples": 9,
    "softness_scale": 0.055,
    "exposure": 1.35,
    "ambient": [0.012, 0.014, 0.021]
  },
  "ground": {
    "center": [0, 0, 0],
    "normal": [0, 1, 0],
    "half_extent": [5.5, 5.5],
    "albedo": [0.22, 0.19, 0.15]
  },
  "particles": [
    {
      "id": 1000,
      "layer": 0,
      "center": [0.05, 0.52, 0.0],
      "size": [0.24, 0.31],
      "orientation": 0.4,
      "transmittance": 0.9,
      "color": [0.64, 0.6, 0.59]
    }
  ],
  "lights": [
    {
      "id": 1,
      "position": [0, 1.65, 0],
      "color": [1, 0.46, 0.14],
      "intensity": 6,
      "radius": 0.12
    }
  ],
  "camera_path": [
    {
      "timestamp": 0.0,
      "position": [0, 1.55, 4.7],
      "target": [0, 1.25, 0],
      "up": [0, 2, 0],
      "fov_degrees": 43
    }
  ]
}
```

粒子四边形由中心、宽高和绕 Y 轴朝向确定。`transmittance` 是垂直入射透射率，必须位于
`[0,1]`。可选 `color` 必须是 `[0,1]` 的三分量。`radius` 被校验并保留为场景属性；
当前软阴影由声明的 `shadow_samples` 和几何半影模型确定。

`render.shadow_probes` 为可选数组，元素格式为：

```json
{"id": "probe-a", "light_id": 1, "ground_uv": [0.0, -1.2]}
```

未声明时，验证器生成固定地面网格探针，并为每个光源自动增加一个实际阴影落点探针。

## 校验与结构化错误

加载场景时收集全部可继续检查的错误，不只返回第一个错误。错误报告示例：

```json
{
  "status": "error",
  "report_version": 1,
  "error_count": 1,
  "errors": [
    {
      "code": "PARTICLE_TRANSMITTANCE_OUT_OF_RANGE",
      "path": "particles[7].transmittance",
      "message": "transmittance must be in [0, 1]",
      "element": {
        "collection": "particles",
        "index": 7,
        "field": "transmittance"
      }
    }
  ]
}
```

主要业务码包括：

- `JSON_PARSE_ERROR`、`MISSING_REQUIRED_FIELD`、`INVALID_TYPE`、`INVALID_VECTOR`
- `PARTICLE_ID_INVALID`、`PARTICLE_ID_DUPLICATE`、`PARTICLE_LAYER_MISSING`
- `LAYER_INDEX_OUT_OF_RANGE`、`PARTICLE_QUAD_DEGENERATE`
- `PARTICLE_TRANSMITTANCE_OUT_OF_RANGE`、`PARTICLE_COLOR_OUT_OF_RANGE`
- `LIGHT_ID_DUPLICATE`、`LIGHT_COLOR_OUT_OF_RANGE`、`LIGHT_INTENSITY_INVALID`
- `GROUND_NORMAL_DEGENERATE`、`GROUND_EXTENT_INVALID`、`GROUND_ALBEDO_OUT_OF_RANGE`
- `CAMERA_TIMESTAMP_NOT_INCREASING`、`CAMERA_VIEW_DEGENERATE`
- `CAMERA_FRAME_INDEX_OUT_OF_RANGE`、`INDEX_OUT_OF_RANGE`
- `SHADOW_SAMPLE_DENSITY_INVALID`、`TILE_OUT_OF_RANGE`

层号必须覆盖 `0..layer_count-1` 且不缺失；同层允许多个粒子。粒子编号全局唯一。
每个相机时间戳必须严格大于前一个时间戳。

## 渲染语义

### 透明粒子

渲染前按 `(layer, id)` 做全序排序，绝不按相机深度或屏幕遍历顺序排序。因此相机环绕时，
同一像素的合成次序保持不变，不会发生透明四边形前后顺序翻转。每个子像素按该固定次序
执行预乘 Alpha source-over：

```text
C_out = C_particle * a + C_dst * (1 - a)
A_out = a + A_dst * (1 - a)
```

入射角修正为：

```text
T_effective = T ^ (1 / max(abs(dot(normal, ray)), 0.18))
a = 1 - T_effective
```

`pixel_samples × pixel_samples` 子像素结果取平均。归属图在每个子样本上按
`a * (1 - A_before)` 积累粒子贡献，最后选择最大贡献者；相同权重时选择较小粒子编号。
因此归属只取决于场景、相机和确定性图像，不依赖分块边界。

### 地面与软阴影

对每个 `(粒子, 点光源)`，将四边形中心和四个角沿光线中心投影到地面，构造凸包落点。
若中心落点超出地面范围，该片对整个地面无遮挡贡献。地面点的分层透射为：

```text
tau_layer = sum(-log(1 - alpha_particle * coverage))
T_ground  = product(exp(-tau_layer))
```

`coverage` 由凸包有符号距离和声明的 `shadow_samples` 过渡查找表计算。半影过渡宽度：

```text
width = softness_scale * (layer_to_ground_distance + light_to_layer_distance)
```

两个距离项系数相同且为正，因此固定另一项时宽度随任一项严格单调增加。报告同时输出实际
落点的半影宽度，以及固定另一距离后的三档控制样本。

多个点光源按 `light.id` 排序并线性相加，使用相同的累加次序。探针报告中的光源占比按
各自地面线性亮度贡献归一化，最后一个占比吸收浮点残差；`share_sum` 必须在 `1e-6`
容差内等于 1。叠加亮度等于单光源贡献之和，不会因输入 JSON 中光源顺序改变。

## 输出

每次成功渲染在输出目录写入：

- `render.png` / `render.rgba8`：最终 RGBA8 图像，Alpha 恒为 255。
- `smoke_premultiplied.png` / `smoke_premultiplied.rgba8`：线性预乘 Alpha 烟雾层。
- `ownership.png` / `ownership.u32le`：逐像素 32 位粒子归属。
- `shadow_landings.json`：每个粒子、光源的投影落点、有效性与半影宽度。
- `report.json`：分块统计、探针、半影趋势、快照摘要和最终 SHA-256。
- `scene.input.json`：本次渲染使用的原始场景副本。
- `snapshot.bin`：最后一个已提交的完整快照。

归属 PNG 的通道编码为：

```text
R = id 的 bit 0..7
G = id 的 bit 8..15
B = id 的 bit 16..23
A = id 的 bit 24..31
```

原始文件 `ownership.u32le` 直接存储小端 `uint32`；`4294967295`（`0xFFFFFFFF`）表示
无粒子，其余值就是场景中的 `particle.id`。因此编号可以合法地从 0 开始。

## 快照与续算

快照是单个 `snapshot.bin` 文件，不是可追加的旧块偏移集合。每完成一个分块：

1. 在内存中生成包含全部已完成块的完整快照。
2. 写入同目录临时文件并 `fsync` / `_commit`。
3. 使用原子重命名整体替换 `snapshot.bin`。

快照包含格式版本、图像与分块尺寸、场景原始内容摘要、渲染配置摘要、已完成块列表、每块
最终颜色和归属摘要、分块统计、最终 RGBA/烟雾 RGBA/归属数据，以及整个主体的 SHA-256。

加载时要求 magic、版本、尺寸、场景摘要、渲染摘要、块编号、块几何、payload 长度和所有
摘要完全匹配。任何一项失败都会删除整个快照并从第 0 块重算，不会沿用旧偏移。场景内容
摘要基于输入 JSON 原始字节，因此光照、相机、粒子或格式内容的任何变化都会使旧快照失效。

`Ctrl+C` 只设置中断标志；当前块继续渲染并提交完整快照，然后写出
`recovery_report.json` 并以 0 退出。`SIGKILL`、任务管理器强制结束等不可处理终止不保证
恢复报告，但已提交快照仍可安全加载。

`softracer compare` 对最终 RGBA 和归属图逐像素逐通道比较，并在
`difference_report.json` 中输出分块编号、块坐标、像素坐标、通道名、参考值和候选值。
阴影落点文件逐字节比较。差异存在时退出码为 3。

## 实际验证结果

本工作区运行日期：2026-11-18，Linux/GCC 15.2 环境。内置样本为 480×320、80×80 分块
（24 块）、110 个粒子、22 层、2 个点光源、16 个相机样本。

验收命令：

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
python3 tests/run_tests.py --binary ./build/softracer --work build/acceptance
```

实际结果：

```text
all acceptance checks passed
full_render_sha256=8ea498ed46a6d139b0e97c1a790da658c4b4454c7ac9f04ee9829d61d00cf40a
```

已自动断言：

- 两次完整运行的 `report.json`、RGBA、归属图、落点 JSON 和 PNG 逐字节一致。
- 第 0 帧与第 7 帧的粒子全序摘要相同，图像摘要不同，证明相机环绕不改变合成次序。
- 粒子数组反转、光源数组反转后，RGBA、烟雾、归属图和阴影落点逐字节不变。
- 强制终止于 4/24 块后续算，最终报告、RGBA、归属图和落点与一次性完整渲染逐字节一致。
- 快照中部字节损坏后，运行日志显示整体丢弃，重算结果仍与完整渲染逐字节一致。
- 修改场景曝光后，旧快照被整体丢弃，重算结果与修改后场景的新渲染一致。
- `Ctrl+C` 在 3/24 块后完成当前块、退出码 0、写出恢复报告；续算结果与完整渲染一致。
- 非法编号、退化四边形、透射率越界、层缺失/越界、时间戳非递增、退化地面和
  非法光源向量均返回非零码及带 `element` 的结构化错误。
- 多光源探针 `share_sum = 1`，叠加超出量为 0；半影控制样本随两个距离项严格递增。

完整验收脚本同时解码并 CRC 校验自写 PNG，确认 PNG 像素与原始 RGBA 文件一致。

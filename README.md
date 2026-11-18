# SilkLight 离线丝网灯箱软光栅验证器

这是一个不依赖外部图片、模型、GPU 或网络的 C++17 离线渲染与验收组件。样本场景、四层三角网格、网目参数、两盏条形灯、背板和相机全部由任务代码确定性生成。渲染结果包括：

- 预乘 Alpha RGBA 原始图与 PNG 预览；
- 逐像素色版归属原始图与 PNG；
- 摩尔纹遮蔽、网目投影、层贡献占比等结构化报告；
- 每块完成后原子替换的完整续算快照；
- Ctrl+C 恢复报告、SIGKILL 后恢复、逐字节比较及结构化差异。

## 构建与运行

Linux/macOS 或 WSL：

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j
./build/silklight sample --out artifacts/scene.json
./build/silklight render --scene artifacts/scene.json --out-dir artifacts/render --fresh
```

Windows 10/11 + Visual Studio 2022：

```powershell
cmake -S . -B build -G "Visual Studio 17 2022" -A x64
cmake --build build --config Release
.\build\Release\silklight.exe sample --out artifacts\scene.json
.\build\Release\silklight.exe render --scene artifacts\scene.json --out-dir artifacts\render --fresh
```

JSON 解析器内置在 `src/json.*`，不需要预装 nlohmann/json。CMake 会检测并链接 OpenMP 运行库；核心像素循环有意保持串行，以保证不同线程配置下仍可逐字节复现。

## CLI

```text
silklight sample   --out scene.json [--seed N]
silklight validate --scene scene.json
silklight halftone --scene scene.json --out-dir DIR
silklight render   --scene scene.json --out-dir DIR [--fresh]
                   [--stop-after-blocks N] [--crash-after-blocks N]
                   [--tile-delay-ms N] [--quiet]
silklight compare  --expected DIR --actual DIR [--out diff.json]
silklight self-test [--work-dir DIR]
```

退出码：

| 退出码 | 含义 |
|---:|---|
| 0 | 渲染完成，或 Ctrl+C 已提交当前块并可续算 |
| 1 | 参数/命令错误 |
| 2 | `E_SCENE_INVALID`，未创建输出、未进入渲染 |
| 3 | 运行时错误 |
| 4 | `E_RESUME_MISMATCH`，续算结果与参考结果不一致 |
| 5 | 内置自测失败 |
| 137 等 | 测试用强制终止，不保证退出报告 |

`--stop-after-blocks N` 用于可恢复中断测试：提交第 N 个新块后写 `recovery.json` 并以 0 退出。`--crash-after-blocks N` 在提交后调用 `_Exit(137)`，用于模拟无退出报告的强杀。真实 SIGINT/SIGKILL 测试见 `tests/integration.py`。

## 场景 JSON 契约

最小结构如下，字段别名在解析器中也做了兼容：

```json
{
  "version": 1,
  "name": "four-layer-backlit-silkscreen-validation",
  "image": {
    "width": 384,
    "height": 256,
    "blockSize": 32,
    "exposure": 1.0
  },
  "screens": [
    {
      "layerIndex": 1,
      "separationId": "CYAN-A",
      "vertices": [[-66, -41, 0], [66, -41, 0], [66, 41, 0], [-66, 41, 0]],
      "indices": [0, 1, 2, 0, 2, 3],
      "inkTransmittance": 0.72,
      "color": [0.10, 0.66, 0.78],
      "mesh": {
        "lineCount": 46,
        "angleDegrees": 15.0,
        "seed": 20261219
      }
    }
  ],
  "airGaps": [1.2, 1.6, 0.9],
  "lights": [
    {
      "position": [-38, -30, -26],
      "direction": [1, 0, 0],
      "length": 44,
      "intensity": 2.6,
      "color": [1.0, 0.94, 0.82]
    },
    {
      "position": [32, 26, -29],
      "direction": [1, 0, 0],
      "length": 38,
      "intensity": 2.25,
      "color": [0.88, 0.95, 1.0]
    }
  ],
  "backboard": {
    "origin": [0, 0, 18],
    "normal": [0, 0, 1],
    "width": 160,
    "height": 112
  },
  "camera": {
    "position": [10, -5, -150],
    "target": [0, 0, 1],
    "up": [0, 1, 0],
    "fovY": 38,
    "near": 0.1,
    "far": 1000
  },
  "moire": {
    "thresholdDegrees": 3.0,
    "maskStrength": 0.82
  }
}
```

兼容性：

- 层数组可用 `screens` 或 `layers`；层序号可用 `layerIndex`/`layer`/`index`。
- 三角索引可用 `indices` 扁平数组、`triangles` 三元组数组或 `faces`。
- 网目参数可写在 `mesh` 内，也可写为 `meshLineCount`、`meshAngleDegrees`、`seed`。
- 灯可用 `position`，也可用 `start`/`end`；强度可为标量或 RGB。
- 背板可把几何放在 `backboard.plane` 中。
- 相机可用固定 `position`/`target`，也可用 `orbit.center/radius/azimuthDegrees/elevationDegrees`。

### 校验与业务码

在任何输出目录创建或渲染前完成校验。以下情况输出带 `layer`、`separationId`、`path`、`message` 的 `E_SCENE_INVALID` 诊断，并以退出码 2 结束：

- 三角形索引越界、三角形退化或网格为空；
- 透射率不在 `[0,1]`、颜色分量越界；
- 色版编号重复、缺失，或层序号重复/缺失；
- 四条网目的层序不是 1..4，或色版编号不是四个唯一值；
- 空气隙不是三个有限非负值；
- 灯不是两盏、位置/强度非法，或灯条端点/内部穿越任一丝网三角形；
- 背板原点/法线/尺寸非法；
- 相机位置、目标、上方向、FOV、近远裁剪面非法；
- 摩尔纹阈值/遮蔽强度越界。

非法场景不会沿用旧快照或旧输出。样本可稳定复现：

```bash
./build/silklight sample --out artifacts/scene.json
./build/silklight validate --scene artifacts/scene.json
```

## 网目采样表

每层由 `seed + lineCount + angleDegrees` 生成一张 256×256 的确定性采样表：

- 表内有 8×8 个网点单元，每单元 32×32 子纹素；
- `detail` 保存二值网点，`smooth` 是固定半径盒滤后的抗闪版本，`edge` 调制套色边缘；
- 世界空间采样坐标按该层平面基向量和 `lineCount` 计算，相位仅由整数化角度和种子决定；
- 采样不依赖三角形遍历顺序、线程调度或未播种随机数；
- 色版编号只产生固定的微小边缘色盐，不改变相位，用于区分同色系相邻色版；
- 同一参数两次生成会写出逐字节相同的 `detail`、`edge` 表，`halftone` 命令可导出 `.slht` 和清单。

采样点在平面局部坐标 `(u,v)` 上满足：

```text
spacing = backboard.width / lineCount
gu = dot(point-origin, basisU) / spacing + phaseU(seed, angle)
gv = dot(point-origin, basisV) / spacing + phaseV(seed, angle)
```

因此任一三角片元上的采样值都可由场景描述、层平面和上述公式独立复算。

## 着色、归属与稳定性

每个相机像素对四层网格分别求最近交点，按 `layerIndex` 升序逐层由内向外合成。每层：

- 以透视入射角修正有效透射率，斜入射时透射更低；
- 以网点 `coverage` 在“孔洞透过”和“油墨滤色”之间插值；
- 用套色颜色做通道滤波，再叠加低强度菲涅尔反射；
- 使用逐通道 Reinhard 色调映射和 sRGB 编码，避免内层正对背光时纯白过曝；
- RGBA 按预乘形式输出，PNG 与 `.raw` 使用相同字节。

抗闪控制使用固定像素足迹与空气隙计算过滤权重，网目表同时提供未滤和滤后样本；多层交点处采用确定性的平滑权重，不抽取未播种随机样本，因此同一相机参数重复运行逐像素一致。

归属图原始格式为一字节/像素：`0` 为背景，`1..4` 为最终贡献的层序号。合成时只有产生可见覆盖或反射贡献的层才更新归属；重叠像素归属最外层最后贡献者，与升序合成次序一致。PNG 使用固定色板。

报告中的 `layerContributions` 同时输出每层：

- `transmittedEnergy`、`reflectedEnergy`；
- `transmissionRatio`、`reflectionRatio`（两者和为 1）；
- 平均网点覆盖率与有效采样数。

## 摩尔纹遮蔽

只比较相邻层对。角度差按 180° 周期性归一化：

```text
diff = min(abs(a-b) mod 180, 180 - (abs(a-b) mod 180))
weight = maskStrength * (1 - diff / threshold)
```

当 `diff < threshold` 且强度大于 0 时触发。对投影响应中的网点孔径乘积 `p = apertureA * apertureB` 做：

```text
masked = p + weight * (meanA * meanB - p)
```

该加权只作用于背板投影的叠印项，直接观察到的各层网点不动，因此降低粗大摩尔纹的低频幅度的同时保留各色版自身网点。报告对每盏灯给出遮蔽前后的低频谱幅度，并汇总：

- 层对编号、色版编号、角度差、阈值、权重；
- `amplitudeBefore`、`amplitudeAfter`、`amplitudeReductionPercent`；
- 每盏灯分量及两组网点覆盖率。

角度差不低于阈值时 `triggered=false`，前后幅度相同。

## 网目投影

对每层每盏灯选取固定的网目孔洞样本，沿“灯上最近点 → 孔洞中心”的透视射线求：

- 相邻外层丝网平面的命中点；
- 背板平面的命中点及背板局部 `boardUV`。

实际像素着色使用反向等价路径：从背板点向灯点追踪，穿过各层网目时采样孔径与色版滤波。因此投影会随空气隙、层高度和两盏灯位置分别偏移。超出背板范围的点在报告中标记 `insideBackboard=false`，着色按透射率 1（无遮挡）处理，不产生硬黑边。

四层平行丝网会沿公共法线按 `airGaps` 的累计值重定位，网格自身平面位置只作为初始姿态；因此单独修改空气隙也会实际改变相邻层和背板落点，而不只是改变报告文字。

`report.json` 的 `projection.points` 每项包含 `layer`、`separationId`、`light`、`holeUV`、`holeWorld`、`boardUV`、`boardPoint`、`insideBackboard`、`adjacentLayer`、`adjacentPoint`、`adjacentUV`。报告同时写明复算方法。

## 快照与中断续算

输出目录中的 `snapshot.json` 是一份完整文档，不依赖旧块偏移或旁路临时数据：

- `payload.sceneDigest` 和规范化场景摘要；
- 图像尺寸、分块尺寸；
- 已完成块编号列表；
- 每块的 RGBA、归属图、层指标、块颜色摘要和归属摘要；
- 外层 `checksum` 对规范化 payload 做摘要校验。

每次完成一块后，新快照先写入临时文件、`fsync`，再用原子重命名整体替换旧快照。Windows 使用 `MoveFileExW(MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)`。续算按块编号查回 `BlockRect` 后拷贝，不使用“已完成数量 × 偏移”的隐含假设。

加载时若出现下列任一情况，整份快照丢弃并从零重算：

- 场景摘要不同（包括网目种子、线数、角度、网格等任何内容变更）；
- 图像/分块配置不同；
- 外层校验和不符、JSON 损坏、块编号重复/越界；
- 块长度、块几何、RGBA/归属摘要、指标数量不符。

Ctrl+C/SIGTERM 只设置可处理中断标志；当前块继续算完，随后提交快照、写 `recovery.json`，以退出码 0 结束。SIGKILL/进程崩溃没有退出报告，下次启动只读已提交快照。

## 比较与差异

```bash
./build/silklight compare --expected artifacts/full --actual artifacts/resumed
```

比较范围：

- `render_rgba.raw` 每个 RGBA 通道；
- `owner_map.raw` 每个像素归属；
- 报告中的投影落点坐标。

失败时 `compare` 写 `resume_diff.json` 并以 4 退出。差异项包含 `tile`（块编号）、`pixel`（`[x,y]`）、`channel`（`r/g/b/a` 或 `separation`）、`expected`、`actual`；投影差异包含点索引、层、灯和 `boardUV` 坐标。

## 已执行验证

环境：Ubuntu 26.04/WSL，GCC 15.2，C++17 Release。以下结果来自本地确定性样本，不访问网络。

`./build/silklight self-test --work-dir artifacts/self-test`：

- 样本合法，四个网目表两次生成逐字节一致；
- 相位只随种子/角度变化，线数变化不改变相位；
- 索引越界、退化三角形、透射率越界、色版重复、色版缺失、灯条落在网面内六类非法样本均返回 `E_SCENE_INVALID`，层序号正确；
- 完整渲染两次 RGBA 与归属图逐字节一致；
- 7 块后“可处理中断”提交快照并退出 0；
- 续算结果与一次性完整渲染逐字节一致，差异数 0；
- 损坏快照被整体丢弃后从零重算，结果仍逐字节一致；
- 修改网目种子后旧场景快照被识别为 `snapshot-scene-mismatch`，从零重算并与新场景完整渲染一致；
- 单独修改空气隙或灯位后，投影 `boardUV` 均发生可测位移；
- 纯白像素占比 0，彩色像素占比 0.6161，过曝/色相检查通过；
- 四层透射/反射占比均为有限非负值且每层两项和为 1。

本样本的摩尔纹结果：

```text
层对 1-2: 1.4°, 触发, 幅度 0.2545 -> 0.1432, 下降 43.7%
层对 2-3: 58.6°, 不触发, 幅度不变
层对 3-4: 30.0°, 不触发, 幅度不变
```

投影清单共 128 点，其中 110 点落在背板内，71 点命中相邻外层丝网，18 点超出背板并按无遮挡处理。

`tests/integration.py` 的真实进程测试：

```text
SIGINT: 退出码 0，存在 recovery.json，续算逐字节一致
SIGKILL: 退出码 -9，无报告要求，仅靠已提交快照续算逐字节一致
```

可在 POSIX 上重复：

```bash
python3 tests/integration.py --exe build/silklight --work-dir artifacts/process-integration
```

## 源文件

- `src/main.cpp`：CLI、信号中断、快照循环、比较、内置自测；
- `src/scene.*`：场景解析/校验、确定性样本；
- `src/halftone.*`：网目采样表；
- `src/render.*`：软光栅、层合成、摩尔纹、投影与报告；
- `src/snapshot.*`：校验和快照、Base64、原子替换；
- `src/png.*`：无外部依赖的 RGBA PNG 编码；
- `src/json.*`：内置 JSON 解析与规范化输出；
- `tests/integration.py`：真实 SIGINT/SIGKILL 验收。


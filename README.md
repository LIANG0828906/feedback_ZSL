# 木构藻井离线软光栅验证器

这是一个从零实现的 C++17/CMake 离线软光栅工程，用于验证古建筑木构藻井场景中的：

- 确定性木纹与透光率调制；
- 斗拱反射、木板透射和层间交叠着色；
- 点光源透视投影、软半影和接收面落点；
- 分块提交快照、Ctrl+C 可处理中断、SIGKILL 后续算；
- 输出字节一致性与结构化差异报告。

工程不依赖 GPU、容器、网络或外部模型/纹理。`include/` 中已放下 STB Image 头文件。

## 快速开始

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j

python3 tools/make_sample.py --out samples/coffered_scene.json
./build/coffered_raster \
  --scene samples/coffered_scene.json \
  --out out/run \
  --fresh
```

Windows 10/11 x64（Visual Studio 2019 或更高）：

```powershell
cmake -S . -B build -G "Visual Studio 16 2019" -A x64
cmake --build build --config Release
ctest --test-dir build -C Release --output-on-failure
```

实际运行结果：

```text
{"ok":true,"image":"rgba.png","ownership":"owner.bin",
 "projection":"projection.json","report":"report.json",
 "completedBlocks":24,"diff":"not_requested"}
```

## 命令行

```text
coffered_raster --scene <scene.json> [options]
  --out <dir>                      输出目录，默认 out
  --fresh                          丢弃已有快照，从块 0 重算
  --verify-baseline <dir>          完成后与基准目录逐字节比较
  --dump-textures                  输出每块木板的颜色 PNG 与浮点采样表
  --tile-delay-ms <n>              为真实中断测试增加块间节流
  --test-interrupt-after-block <n> 模拟可处理中断
```

退出码：

- `0`：渲染完成，或可处理中断已完成当前块、提交快照并写出恢复报告；
- `1`：文件、快照写入或内部错误；
- `2`：场景校验失败；
- `3`：基准比较失败，已写出 `diff_report.json`。

## 输出

| 文件 | 内容 |
| --- | --- |
| `rgba.png` | 预乘 Alpha 的 RGBA 图像，样本背景不透明，最终 `A=255` |
| `ownership.png` | 逐像素构件归属色图 |
| `owner.bin` | `COFFOWN1` 头 + 每像素小端 `int32` 归属码 |
| `projection.json` | 每个点光源的投影落点、UV、是否越界、半影采样剖面 |
| `report.json` | 场景摘要、纹理摘要、逐层反射/透射贡献、归属查询 |
| `progress.snapshot.json` | 最近一次已提交的完整分块快照 |
| `recovery.json` | Ctrl+C 等可处理中断后写出的恢复报告 |
| `diff_report.json` | `--verify-baseline` 的结构化差异 |
| `validation.json` | 场景校验失败时的结构化告警 |
| `textures/` | `--dump-textures` 生成的确定性颜色纹理和浮点采样表 |

`owner.bin` 的归属码与颜色映射见 `report.json` 的 `ownershipIndex`；`ownershipQueries` 给出若干坐标及其归属，便于复算。

归属图取层号升序中首个命中的层；层内取交叠融合权重最大的构件，权重平手时取较小归属码。该规则只依赖场景与固定合成顺序，不依赖三角形遍历顺序。

## 场景格式

层号 `0` 是最内侧，层号递增向外；样本中相机和光源在下方，外层木板位于更高的 `z`，因此每个斗拱沿光线方向投到更高层木板，最外层再投到接收面。层合成严格按层号升序执行。

```json
{
  "schema": "coffered-ceiling-soft-raster/v1",
  "name": "sample",
  "render": {
    "width": 192,
    "height": 128,
    "tile_size": 32,
    "texture_resolution": 64,
    "projection_samples": 3,
    "shadow_map_resolution": 256,
    "penumbra_scale": 0.012,
    "exposure": 0.82,
    "ambient": 0.14,
    "background": [0.002, 0.003, 0.004]
  },
  "layers": [{
    "layer": 0,
    "z": 0.04,
    "thickness": 0.04,
    "boards": [{
      "id": "B0N",
      "seed": 1101,
      "transmittance": 0.48,
      "color": [0.76, 0.43, 0.17],
      "mesh": {
        "vertices": [
          [-0.52, 0.22, 0.04],
          [0.52, 0.22, 0.04],
          [0.52, 0.52, 0.04]
        ],
        "triangles": [{
          "indices": [0, 1, 2],
          "uv": [[0.25, 0.75], [0.75, 0.75], [0.75, 0.25]]
        }]
      }
    }],
    "brackets": [{
      "id": "K0N",
      "reflectance": 0.34,
      "thickness": 0.04,
      "color": [0.2, 0.25, 0.23],
      "mesh": {
        "vertices": [
          [0.0, 0.15, 0.02],
          [0.26, 0.15, 0.02],
          [0.26, -0.15, 0.02]
        ],
        "triangles": [{"indices": [0, 1, 2], "uv": [[0, 0], [1, 0], [0, 1]]}]
      }
    }]
  }],
  "lights": [{
    "id": "L_WARM",
    "position": [-0.52, -0.34, -0.56],
    "color": [1.0, 0.79, 0.57],
    "intensity": 1.35
  }],
  "receiver": {
    "z": 1.24,
    "min_x": -1.48,
    "max_x": 1.48,
    "min_y": -1.08,
    "max_y": 1.08,
    "color": [0.29, 0.31, 0.30]
  },
  "camera": {
    "position": [0, 0, -1.82],
    "target": [0, 0, 0.34],
    "up": [0, 1, 0],
    "fov_deg": 58,
    "near": 0.02,
    "far": 12
  }
}
```

网格三角形可省略 `uv`，此时使用默认三角形 UV。木板三角网格必须非空；每层必须同时包含至少一个木板和一个斗拱。构件编号全局唯一且非空。

## 校验规则

校验失败时写出 `validation.json`，每条告警包含 `code`、`message`，适用时包含 `layer`、`componentId`、`kind`，进程退出码为 `2`。已覆盖：

- 网格索引越界；
- 三角形面积为 0 或非有限；
- 木板透光率、斗拱反射率不在 `[0,1]`；
- 构件编号缺失或全局重复；
- 木板种子不是无符号 32 位整数；
- 斗拱/层厚度非正；
- 点光源落在任一层平面 `z` 的 `1e-6` 邻域内；
- 相机、接收面、颜色、强度、纹理/投影/分块参数非法。

## 实现要点

### 确定性木纹

木纹只使用固定 FNV-1a、SplitMix64、整数格点值噪声和固定四层 FBM。没有 `std::random_device`、时间、线程调度或未排序容器参与纹理生成。颜色纹理与透光率调制表按 `(seed, componentId, resolution)` 生成，并在报告里记录 SHA-256。`coffered_texture_probe` 可在不加载场景的情况下独立复算某块纹理：

```bash
./build/coffered_texture_probe \
  --seed 2201 --id B1N --resolution 64 \
  --color 0.72 0.415 0.19 --table /tmp/B1N.bin
```

### 层内交叠与层间合成

- 每像素对所有三角形做双面 Möller-Trumbore 求交。
- 同一层内按几何位置稳定排序，不依赖三角形数组顺序。
- 近共面片元用 `exp(-Δt / overlapScale)` 融合，其中
  `overlapScale = (层厚 + 构件厚度) / max(cosθ, 0.12)`；不会因为遍历顺序任意选中一个片元。
- 各层严格按层号升序做前向合成：
  `C += T_accum * C_layer`，`A += T_accum * opacity`，`T_accum *= transmit`。
- 木板透光率同时受入射角与木纹采样表调制；背光时仍有环境散射木纹层次。
- 斗拱使用受限漫反射与小高光，最终统一 tone map 和 sRGB 编码，避免正面过曝。

### 投影与半影

对每个 `(光源, 斗拱层, 目标层/接收面)` 建立独立遮挡图。目标平面落点使用透视公式：

```text
P = L + ((z_target - L.z) / (V.z - L.z)) * (V - L)
```

其中 `V` 是斗拱构件质心。`projection.json` 输出每个光源、每个目标、每个斗拱构件的落点、平面 UV、是否越界；越界项显式标记 `treatedAsUnoccluded=true`。

半影半径：

```text
r = penumbra_scale
    * (1 + 3 * |z_target - z_occluder|)
    * (1 + 1.5 * |z_occluder - z_light|)
```

因此接收距离和光源到遮挡层距离任一增加，`penumbraWidth=2r` 都严格增加。查找时使用场景声明采样密度的 Vogel 圆盘采样，加上双线性遮挡图采样。目标外采样返回 `1.0`（完全无遮挡）；接收面遮挡图只写入接收面 Bounds 内，因此不会在边缘生成硬黑边。

### 快照与中断

快照是单个原子覆盖的 JSON：

- `sceneDigest`：输入语义 JSON 的 SHA-256；
- `sceneSummary`：尺寸、层、木板种子、材质、光源；
- 每个已完成块：块号、偏移、尺寸、RGBA/归属摘要、平均 RGBA、逐层能量统计；
- 块 payload：base64 RGBA 与归属码；
- `checksum`：除自身外整个快照的 SHA-256。

启动时只读取该快照。场景摘要、宽高、分块尺寸、块号序列、偏移、payload 长度、RGBA/归属摘要、快照总校验任一不一致，都会删除快照并从块 `0` 重算，不沿用旧块偏移。

`SIGINT`/Ctrl+C 只设置信号标志。当前块完整渲染并提交快照后，写出 `recovery.json` 并以 `0` 退出。`SIGKILL` 不执行退出路径；下次启动从最后持久化快照继续。续算后的 `rgba.png`、`owner.bin`、`projection.json` 与不中断运行逐字节一致。

### 结构化差异

```bash
./build/coffered_raster --scene samples/coffered_scene.json \
  --out out/candidate --verify-baseline out/reference
```

差异报告列出最多 128 条具体差异，并给出总差异数。像素差异包含：

```json
{
  "block": 1,
  "x": 51,
  "y": 19,
  "channel": "R",
  "expected": 134,
  "actual": 133
}
```

归属图差异使用 `channel="ownerCode"`；投影 JSON 不一致使用 `channel="projectionBytes"`。

## 已执行验证

运行：

```bash
cmake --build build --target test
```

实际结果：

```text
1/1 Test #1: deterministic_validation ......... Passed
100% tests passed, 0 tests failed out of 1
```

覆盖项与实际结果：

- 不中断、重复、三角形重排、改变光源位置后重复运行：图像/归属/投影逐字节一致；
- 解码最终 PNG：单个木板构件亮度方差 547.5（阈值 9），斗拱最大亮度 155.3、平均 134.1（阈值 250/225）；
- 12 块木板纹理重复生成与独立探针复算：SHA-256 和浮点表逐字节一致；
- 9 类结构/参数错误：全部退出 `2`，结构告警含层号与构件编号；
- 投影落点：72 项，26 项越界且标记为无遮挡；随层距向外偏移；
- 半影：同一遮挡层的 `penumbraWidth` 随接收距离严格增加，采样剖面含中间灰度；
- Ctrl+C：第 3 块提交后退出 `0`，续算 `diffStatus=match`；
- SIGKILL：第 3 块快照后终止，无恢复报告，续算 `diffStatus=match`；
- 损坏快照：块偏移归零、整体重算，最终字节一致；
- 木纹种子变化：场景摘要失效、快照整体丢弃，输出差异被结构化报告并退出 `3`。

基线输出 SHA-256：

```text
14e6975517c7f67284982f9a08dd0c73da49106ac55a58f97c33fbc8289cca11
3c33563371e6d1f983eaeef93123941dd731adf1a0b9b04808738e3c2275ab5c  owner.bin
66e9bde3bb5474c3c5e65998ef1caffc61b0f3d4f2f363c373b4d60706534e67  projection.json
```

## 目录

```text
src/            C++17 实现
include/        stb_image.h / stb_image_write.h
tools/          确定性样本构造器
samples/        样本场景与参考输出
tests/          Linux/WSL 端到端验收脚本，Windows 可用 CTest + Python 运行
```

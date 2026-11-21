# Font Atlas Workbench

本地 C++17 字体轮廓光栅化工作台。项目把混合二次/三次贝塞尔轮廓展开为方向一致的规范化折线，在目标像素尺寸和子像素偏移下做环绕数采样，分块并行生成抗锯齿覆盖率，按分块坐标稳定合成图集，并支持预算隔离、内容哈希校验和强制终止恢复。

工程不依赖网络、账号或外部字体。`samples/` 已包含固定种子生成的字体、配置和基线哈希；`out/` 用于运行输出。

## 已实现

- 严格 JSON 字体解析：`line`、`quad`、`cubic`、`close`、复合字形、Unicode 映射和轮廓方向；错误定位到字形、轮廓、段或复合引用序号。
- 非零环绕与奇偶填充共用同一展平几何；孔洞、重叠轮廓和分块边界使用全局采样坐标，不依赖块内相对坐标。
- 8 位覆盖率、`samplesPerAxis` 子像素采样、可配置量化步长；非 8 位范围步长返回 `E_QUANT_INVALID`。
- 预算规划：按采样点、覆盖率缓冲、扫描线/采样批内存估算；先按 `degradeSequence` 缩小采样批、拆分分块，仍超限则在任何分块启动前返回 `E_BUDGET_EXCEEDED`。
- 固定并行度调度：可运行任务按提交序号稳定取用；覆盖率按分块索引和坐标提交，不受线程完成顺序影响。
- 单块整体光栅化真值比对；超过 `seamTolerance` 返回 `E_SEAM_UNRESOLVED`，诊断包含字形、块、相邻块和像素坐标。
- 图集 shelf 布局、PGM 位图、`atlas.json` 度量表和逐字形位图；布局失败在任何图集写入前返回 `E_ATLAS_OVERFLOW`。
- 分块内容文件与状态快照均采用临时文件、刷盘和原子替换；快照保存配置、分块表、内容哈希、已完成索引和哈希链。
- `font_atlas` CLI、确定性样本生成器、JSONL 轨迹和可选 Qt 6.5 工作台。

## 构建

要求 CMake 3.24+、C++17 编译器和标准线程库。

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j 4
ctest --test-dir build --output-on-failure
```

Qt 界面为可选目标。安装 Qt 6.5+ 的 `Widgets` 与 `OpenGLWidgets` 后配置：

```bash
cmake -S . -B build -DQt6_DIR=/path/to/Qt/6.5.*/lib/cmake/Qt6
cmake --build build --target font_atlas_gui
```

未检测到 Qt 时，核心库、CLI 和测试仍可完整构建。

## 本地样本与验证

仓库内 `samples/` 是种子 `42` 的生成结果，包含两套字形几何（`font.json`、`font_alt.json`）、24/48/96 px 配置、非零/奇偶配置、预算拒绝配置、含孔洞复合字形、内容冲突样本和基线覆盖率哈希。

```bash
# 可重复生成同一份样本
./build/font_atlas generate --out samples --seed 42

# 两种填充规则、分块/整体一致性、并行确定性、预算隔离、快照恢复
./build/font_atlas verify --samples samples --out out/verify

# 完整图集与轨迹
./build/font_atlas raster \
  --font samples/font.json --config samples/config.json --out out/full

# 量化步长变化误差摘要
./build/font_atlas compare-quant \
  --font samples/font.json --config samples/config.json \
  --old 1 --new 8 --out out/quant.json

# 强制终止恢复：子进程提交 2 块后直接退出
set +e
./build/font_atlas raster \
  --font samples/font.json --config samples/config.json \
  --out out/crash --state out/crash/state --hard-exit-after 2
set -e
./build/font_atlas raster \
  --font samples/font.json --config samples/config.json \
  --out out/crash --state out/crash/state --resume
```

`raster` 输出：

- `atlas.pgm`：8 位覆盖率图集。
- `atlas.json`：图集尺寸、图像哈希，以及每个字形的偏移、步进、bearing、包围盒和内容哈希。
- `trace.jsonl`：字形、分块、采样/内存预算、复用/重算计数和内容哈希。
- `result.json`：本次运行摘要和隔离失败列表。
- `state/<glyph>/snapshot.json` 与 `state/<glyph>/chunks/*.cov`：原子恢复状态。

`font_atlas_gui` 提供文本/字形输入、尺寸、子像素、填充规则、并行度设置，`QOpenGLWidget` 字形预览、分块网格、已完成块着色、资源占用和协作式取消。取消或失败只停止新提交，不覆盖最后一次成功发布的图集。

## 字体描述

顶层字段为 `version`、`name`、`unitsPerEm`、`ascent`、`descent` 和 `glyphs`。字形示例：

```json
{
  "name": "ring",
  "unicode": "U+004F",
  "advance": 1000,
  "contours": [
    {
      "direction": "cw",
      "start": {"x": 100, "y": 400},
      "segments": [
        {"type": "cubic", "control1": {"x": 100, "y": 621},
         "control2": {"x": 279, "y": 800}, "to": {"x": 500, "y": 800}},
        {"type": "quad", "control": {"x": 720, "y": 800},
         "to": {"x": 900, "y": 400}},
        {"type": "close"}
      ]
    }
  ],
  "components": [
    {"glyph": "base", "dx": 20, "dy": 10, "scale": 0.9}
  ]
}
```

- 段类型可为 `line`/`quad`/`quadratic`/`cubic`/`close`，也接受单字母别名。
- 控制点和端点可用 `{x,y}` 或 `[x,y]`。
- 可选 `points` 数组允许 `control`/`to`/`from` 使用数组下标。
- `direction` 接受 `cw`/`ccw`，最终坐标空间面积符号不符时会反转轮廓。
- 复合变换支持 `dx`、`dy`、`scale`、`scaleX`、`scaleY` 或六值 `matrix`；引用成环、引用缺失和变换退化均为 `E_FONT_INVALID`。

## 错误码

| 错误码 | 触发条件 |
| --- | --- |
| `E_FONT_INVALID` | 控制点/段类型错误、Unicode 重复、复合引用成环或缺失 |
| `E_QUANT_INVALID` | 量化步长为 0 或超出 8 位表示范围 |
| `E_BUSY` | 同一字形正在运行 |
| `E_BUDGET_EXCEEDED` | 声明的降级序列耗尽后仍超出单任务/全局预算 |
| `E_SEAM_UNRESOLVED` | 分块合成结果超过整体真值容差 |
| `E_ATLAS_OVERFLOW` | 图集放不下字形或度量越界 |
| `E_SNAPSHOT_MISMATCH` | 快照版本、配置、内容哈希或哈希链不符 |
| `E_CANCELLED` | 协作式取消在分块检查点生效 |

## 验证结果

当前 Linux/GCC 15.2/CMake 3.30 环境实测：

- `ctest --test-dir build --output-on-failure`：2/2 通过。
- `font_atlas verify`：13 项检查全部通过。
- 24/48/96 px、两种填充规则的 6 个字形覆盖率均与基线哈希一致，分块与整体真值零差异。
- 并行度 1 和 4 的图集哈希相同。
- 预算拒绝配置下部分字形返回 `E_BUDGET_EXCEEDED`，成功字形继续打包，失败字形未进入图集。
- 进程以退出码 77 在提交 2 个分块后强制终止；恢复后 `Oacute` 复用 2 块、重算 18 块，最终图集哈希与完整运行相同：`4f4bc6cc7981c5a3c012149b9e429fe4a8235aba65325527bdaa5a8cdf57a72f`。
- 量化 1→8 的样例总绝对误差为 376/8769 像素，逐字形最大误差为 1。

当前环境的用户目录内含 Qt 6.10.2。GUI 目标已实际编译，并以 `QT_QPA_PLATFORM=xcb` 启动 `--smoke-test`，报告 `OpenGL 4.5 Core`；满足 OpenGL 3.3+ 要求。若 Qt 安装包自带 OpenGL 头/库但系统 CMake 未发现，可同时传入 `OPENGL_INCLUDE_DIR`、`OPENGL_opengl_LIBRARY` 和 `OPENGL_glx_LIBRARY`。

更多实现细节见 `docs/TECHNICAL.md`，完整验收记录见 `artifacts/acceptance-verification.md`。


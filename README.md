# 敦煌藻井离线软光栅验证组件

这是一个不依赖 GPU、运行时不联网的 C++17 离线渲染/验证程序，用于核对三层木格彩画、半透明绢纱、三盏窟内油灯和悬吊斗拱的观感。工程包含确定性场景生成器、严格 JSON 校验、分块软光栅、面积光软阴影、快照续算和逐像素差异报告。

## 快速开始

依赖：

- CMake 3.20+
- MSVC 2022 或 MinGW（C++17）
- libpng 1.6.x 开发文件
- Python 3.10+，仅用于生成样本和集成测试

Windows / Visual Studio 2022 示例：

```powershell
cmake -S . -B build -G "Visual Studio 17 2022" -A x64 `
  -DCMAKE_PREFIX_PATH=C:/path/to/libpng
cmake --build build --config Release

python tools/generate_samples.py --output-dir samples
.\build\Release\caisson_render.exe `
  --scene samples\scene.json --output out
```

MinGW 示例：

```powershell
cmake -S . -B build -G "MinGW Makefiles" `
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_PREFIX_PATH=C:/path/to/libpng
cmake --build build
```

Linux 本机验证命令：

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_PREFIX_PATH=/path/to/libpng-prefix
cmake --build build -j
python3 tools/generate_samples.py --output-dir samples
./build/caisson_render --scene samples/scene.json --output out
python3 tests/integration_test.py \
  --exe build/caisson_render --work-dir build/test_work
```

也可在构建后运行 `ctest --test-dir build --output-on-failure`。

## 命令行

```text
caisson_render --scene <scene.json> [--output <directory>]
               [--stop-after-blocks <n>] [--block-delay-ms <n>]
               [--discard-snapshot] [--verify-against <directory>]
```

- `--scene`：输入 JSON 场景。
- `--output`：输出目录，默认 `out`。
- `--stop-after-blocks`：提交指定总数块后安全停机，用于确定性中断测试。
- `--block-delay-ms`：每块提交后的测试用延迟，不进入报告。
- `--discard-snapshot`：忽略并删除旧快照。
- `--verify-against`：将本次结果与基准目录逐像素比较；不一致时输出结构化差异并以 4 退出。

退出码：`0` 成功或安全中断，`2` 场景/命令行校验失败，`3` I/O 或运行时失败，`4` 续算差异。

## 输出

| 文件 | 含义 |
|---|---|
| `image.png` | 可直接观看的 RGBA8 图，RGB 为合成在窟内暗背景上的颜色，A=255 |
| `premultiplied.png` | 最终逐层合成得到的预乘 Alpha RGBA8 |
| `ownership.png` | 逐像素归属图：R=最近层号，G=可见层数，B=主贡献层号，A=覆盖率；255 表示斗拱 |
| `shadow_landings.csv` | 每盏灯、每个斗拱顶点投影到每层平面的落点毫米坐标 |
| `render_report.json` | 稳定统计：场景摘要、输出哈希、逐层透射/反射/漫反射能量、软阴影剖面和趋势、色彩统计 |
| `textures/` | 每层木格 RGB8 纹样采样表、薄纱 U8 透射采样表及清单 |
| `snapshot.json`、`blocks/` | 渲染中的提交快照，成功完成后删除 |
| `recovery_report.json` | 安全中断时的恢复报告，续算成功后删除 |

报告不含时间戳、耗时或绝对路径；字段顺序固定。相同输入重复运行的输出逐字节一致。

## 实际验证结果

本机使用 GCC 15 / CMake 3.30 / libpng 1.6.57 / Python 3.14 完成以下验证：

- 集成测试：`PASS`，覆盖确定性双跑、输入列表乱序等价、六类业务错误、软阴影响应、真实 SIGINT、`SIGKILL` 强杀续算、损坏快照丢弃、场景摘要变化丢弃和结构化像素差异。
- Debug + AddressSanitizer + UndefinedBehaviorSanitizer 完整渲染：无报错、无泄漏。
- 样本场景摘要：`fcc84d0e2a26cf3dba0151ce8b81513503ceabd25b58daf601760bc496a16905`
- `image.png` SHA-256：`7a1b8e0ef107e5350bd31e266a75c8a8a41f7239517033de7065a7ad37a396e2`
- `premultiplied.png` SHA-256：`903604eeb9d00dc4f5f2be6080fee27356625815b21db649d424767c3bb06c3f`
- `ownership.png` SHA-256：`e623487c28e9e12b797438b49f858809bc451247af7f5da17bb4e0892f845fa7`
- `shadow_landings.csv` SHA-256：`f8e9c66d7f238ed5417f4097cc0b2fcba369290fa1fe297ba4692f9801194b39`
- `render_report.json` SHA-256：`07f04ad326b9058c129d15d7ab130f91fd76c4ddd7e44dd934a373442e24e7c9`
- 色彩统计：`saturated_pixels=0`，`saturated_channel_values=0`，最大通道值 `250`，非灰像素 `32034`。
- 软阴影趋势：斗拱到受影面平均距离 `840 mm` 时平均过渡宽度 `522 mm`；距离 `2683 mm` 时平均过渡宽度 `881 mm`，报告判定单调不减。
- 逐层透射/反射占比（透射、反射）：层 1 为 `97.9228% / 2.0772%`，层 2 为 `95.6599% / 4.3401%`，层 3 为 `92.9036% / 7.0964%`。

完整数据由 `out/render_report.json` 给出。样本包含 3 层、4 象限、3 盏面积油灯和 2 个不同悬挂深度的斗拱，彩画直径为 3 m。

## 文档

- `docs/SCENE_FORMAT.md`：JSON 字段、约束、业务错误码和摘要规则。
- `docs/ALGORITHM.md`：光栅、着色、软阴影、合成、快照和差异检测实现。


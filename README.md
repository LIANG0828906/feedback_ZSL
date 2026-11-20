# 矢量填充判定流水线

这是一个本地、确定性的 C++17 组件，用于解析多条闭合矢量子轮廓、计算非零环绕数与奇偶规则覆盖，
并显式输出容差带造成的环绕数上下界、规则敏感像素、不确定像素、降级状态及异常注入结果。
核心几何、光栅化、SHA-256 与 JSON 解析均不依赖网络或第三方运行时库。

## 当前实现

- 路径入口支持直线、二次贝塞尔、三次贝塞尔、子轮廓方向标记、孔洞角色和双填充规则声明。
- 贝塞尔曲线使用 de Casteljau 自适应展平，离散弦段作为单调段参与射线穿越计算。
- 每个采样像素输出 `winding[lower, upper]`、`crossings[lower, upper]`、最近边界距离、
  两规则结论和判定来源。
- 同时生成非零、奇偶两套覆盖率图、逐像素差异图、填充位图、像素来源图和逐像素轨迹。
- 容差预算包含顶点合并、交点归类和采样距离阈值；预算分析会重算不确定区间并输出新旧下界差异。
- 入口注入在 JSON 路径校验和光栅化之前执行；注入失败不会创建输出目录或写任何位图。
- 输出用临时文件加原子重命名发布；采样块取消时不发布半成品，已有位图保持不变。
- Qt 预览器是可选构建项，核心不依赖 Qt。

## 构建

要求 CMake 3.20+、支持 C++17 的编译器。

### Windows / Visual Studio 2022

```powershell
cmake -S . -B build -G "Visual Studio 17 2022" -A x64
cmake --build build --config Release
.\build\Release\fill_pipeline.exe --help
```

可选 Qt 5.15 / Qt 6 Widgets 预览器：

```powershell
cmake -S . -B build -G "Visual Studio 17 2022" -A x64 `
  -DFILL_PIPELINE_BUILD_QT_PREVIEW=ON `
  -DCMAKE_PREFIX_PATH=C:\Qt\6.5.3\msvc2019_64
cmake --build build --config Release
.\build\Release\fill_preview.exe .\out\fill_bitmap.ppm
```

### Linux / GCC 或 Clang

```bash
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build --parallel
ctest --test-dir build --output-on-failure
```

## 路径 JSON 格式

坐标按世界坐标保存，默认 y 轴向上。`direction` 必须与几何有向面积一致；不一致时返回
`E_FILL_RULE_CONFLICT`。每条子轮廓必须显式回到起点，或声明 `"closed": true` 由入口补一条闭合直线段。

```json
{
  "fillRule": "nonzero",
  "budget": {
    "vertexMerge": 0.001,
    "intersectionClassify": 0.001,
    "sampleDistance": 0.01,
    "totalLimit": 2.0
  },
  "contours": [
    {
      "start": [0, 0],
      "direction": "ccw",
      "role": "outer",
      "segments": [
        {"type": "line", "to": [100, 0]},
        {"type": "quadratic", "control1": [110, 20], "to": [100, 40]},
        {"type": "cubic", "control1": [80, 60],
         "control2": [20, 60], "to": [0, 40]},
        {"type": "line", "to": [0, 0]}
      ]
    },
    {
      "start": [20, 20],
      "direction": "cw",
      "role": "hole",
      "segments": [
        {"type": "line", "to": [20, 60]},
        {"type": "line", "to": [80, 60]},
        {"type": "line", "to": [80, 20]},
        {"type": "line", "to": [20, 20]}
      ]
    }
  ]
}
```

兼容字段别名包括 `subpaths`、`fill_rule`、`orientation`、`control1/control/controlPoint1`、
`to/end/point` 等。点可写成 `[x, y]` 或 `{"x": ..., "y": ...}`。

解析规则：

- `fillRule` 缺失或非法：`E_PATH_INVALID`。
- 子轮廓方向、段类型、起点、终点或控制点缺失：`E_PATH_INVALID`，并带 `contour`/`segment`。
- 坐标非有限：`E_PATH_INVALID`，并带定位。
- 未闭合或显式 `"closed": false`：`E_PATH_INVALID`。
- 零面积自然轮廓：`E_ZERO_AREA_CONTOUR`。
- 方向标记与几何方向矛盾，或 `role:"hole"` 在非零规则下与父轮廓同向：
  `E_FILL_RULE_CONFLICT`，`relatedContours` 列出涉及子轮廓。
- 任一容差非有限、为零、为负，或三项之和超过 `totalLimit`：`E_TOLERANCE_INVALID`。

## 环绕数与不确定区间

对每个像素中心，向世界坐标左侧投射水平射线：

1. 展平后的每条单调弦段若明确穿越射线，按 y 方向贡献 `+1/-1`。
2. 穿越位置落入 `intersectionClassify` 容差带，或端点落入容差带时，不猜测归类，而是把
   对应贡献记入可能区间。
3. 最近边界距离小于 `sampleDistance` 时，同样扩展上下界并标记不确定。
4. `windingLower` 是所有可能负贡献的最坏情况，`windingUpper` 是所有可能正贡献的最坏情况。
5. 非零规则只有在 `[lower, upper]` 完全在零的一侧时才是确定实心；奇偶规则只有在穿越数
   区间两端奇偶性相同时才是确定结论。
6. 两规则均确定且结论不同时标记 `rule_sensitive`；任一区间不确定时标记 `uncertain`。

不确定像素在覆盖率图和填充图中写 `128`，在 `pixel_source.pgm` 中写 `170`；规则敏感像素
写 `85`。不确定像素绝不会被静默写成实心 `255` 或镂空 `0`。

## 容差预算与收紧顺序

三项容差共享一个总预算，且顺序固定为：

1. `intersection_classify`
2. `sample_distance`
3. `vertex_merge`

该顺序是编译期固定值，不随路径或参数改变。`result.json` 的 `budgetAnalysis.impacts`
分别给出将每项减半后的不确定像素数、覆盖面积偏差和差值；
`budgetAnalysis.toleranceComparison` 给出旧/新预算下的下界变化像素数、变化直方图及上下界差异总和。

## 命令行

```bash
# 生成确定性样本、基线与 manifest
./build/fill_pipeline generate-samples --out samples --width 64 --height 64

# 渲染
./build/fill_pipeline render \
  --input samples/nested_hole.json \
  --out out/nested_hole

# CLI 预算覆盖路径内预算
./build/fill_pipeline render --input path.json --out out \
  --budget 0.001,0.001,0.01,2.0

# 入口注入；失败返回码 2，且不创建 out
./build/fill_pipeline render --input path.json --out out --inject nonfinite

# 提交门禁
./build/fill_pipeline submit --result out/nested_hole/result.json
```

注入名称与独立业务码：

| `--inject` | 业务码 |
| --- | --- |
| `overlap-edge` | `E_INJECT_OVERLAP_EDGE` |
| `degenerate-segment` | `E_INJECT_DEGENERATE_SEGMENT` |
| `zero-area` | `E_INJECT_ZERO_AREA_CONTOUR` |
| `hole-direction` | `E_INJECT_HOLE_DIRECTION` |
| `nonfinite` | `E_INJECT_NONFINITE_COORD` |

注入错误均带 `injected:true`、`injectionClass`、`contour`、`segment` 和 `relatedContours`。

退出码：

- `0`：渲染成功或提交成功。
- `1`：参数或 I/O 错误。
- `2`：路径、容差、规则冲突或渲染参数错误。
- `4`：协作式取消。
- `9`：提交被降级结果门禁拒绝，业务码 `E_DEGRADED_RESULT` 或
  `E_RESULT_NOT_COMMITTABLE`。

## 输出文件

| 文件 | 内容 |
| --- | --- |
| `result.json` | 规则、预算分析、诊断、指标、哈希、降级与提交门禁 |
| `trace.jsonl` | 无时间戳的确定性运行轨迹及近重合/退化/孤立/歧义事件 |
| `pixel_trace.jsonl` | 每个像素的上下界、穿越上下界、规则结论、来源与边界距离 |
| `coverage_nonzero.pgm` | 非零规则覆盖率图，`0/128/255` |
| `coverage_evenodd.pgm` | 奇偶规则覆盖率图，`0/128/255` |
| `coverage_diff.pgm/.ppm` | 逐像素规则差异与不确定区域 |
| `fill_bitmap.pgm/.ppm` | 按声明规则生成的填充位图 |
| `pixel_source.pgm` | `0` 一致、`85` 规则敏感、`170` 不确定 |
| `normalized_path.json` | 规范化顶点、段、控制点、方向、有向面积与包围盒 |

`contentHash` 是对固定顺序的输出文件内容做 SHA-256 聚合；同一路径和参数下图像、轨迹和
`result.json` 逐字节一致。日志不含时间戳、运行时长、绝对输出目录或随机数。

## 样本与回归

随附生成器产生 9 个样本：孔洞、三层嵌套、同向嵌套、偶奇嵌套、部分重合、贝塞尔混合、极短退化段、
近重合边和宽松容差不确定区。`samples/manifest.json` 保存 PGM 基线覆盖率哈希。

Linux 或 WSL：

```bash
bash scripts/run_regression.sh build regression-out
```

Windows PowerShell：

```powershell
.\scripts\run_regression.ps1 -BuildDir .\build -WorkDir .\regression-out
```

回归脚本会：

- 生成样本并渲染两次，逐文件比较；
- 将覆盖率 PGM 与 manifest 基线哈希比较；
- 逐一验证五类入口注入在写出任何图像前被拒绝；
- 验证取消返回 `E_CANCELLED` 且上一份 `fill_bitmap.pgm` 未变化；
- 验证可提交结果成功，规则敏感降级结果返回 `E_DEGRADED_RESULT`。

## 实际验证结果

当前环境为 Linux、GCC 15.2、CMake 3.30.5。`ctest` 与 9 样本回归均通过；
Windows PowerShell 脚本已提供，但当前执行环境无法直接运行 Windows/MSVC 验证。
当前环境未安装 Qt，因此可选 `fill_preview` 未编译执行；PPM/PGM 产物已由回归逐字节验证。

64×64 网格代表性结果：

| 样本 | 交点 | 近重合对 | 退化段 | 规则敏感 | 不确定 | 状态 |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| `nested_hole.json` | 0 | 0 | 0 | 0 | 0 | ok |
| `nested_same_direction.json` | 0 | 0 | 0 | 912 | 0 | degraded |
| `evenodd_nested.json` | 0 | 0 | 0 | 0 | 0 | ok |
| `three_contours.json` | 0 | 0 | 0 | 0 | 106 | ok |
| `overlap_pair.json` | 2 | 0 | 0 | 400 | 0 | degraded |
| `bezier_hole.json` | 0 | 0 | 0 | 0 | 122 | ok |
| `degenerate_short.json` | 0 | 0 | 1 | 0 | 0 | ok |
| `near_coincident.json` | 9 | 4 | 0 | 3100 | 0 | degraded |
| `tolerance_ambiguity.json` | 0 | 0 | 0 | 748 | 1160 | degraded |

回归摘要：

```text
baseline ok: 9 samples
regression ok: 9 samples, baseline hashes match, outputs deterministic,
cancellation preserved bitmap
```

## 数值边界

- 贝塞尔求交基于自适应展平后的单调弦段，精度由交点归类容差和展平递归上限控制，不是解析根求解。
- 采样为像素中心点采样，包围盒自动适配到 `[1, width-1] × [1, height-1]`。
- `isolated` 的含义是“在本次确定性采样网格上未产生任何射线穿越贡献”，用于诊断不影响
  环绕数的水平/切向段。
- 规则敏感或不确定像素合计超过 `--degrade-ratio`（默认 `0.05`）时，结果标记降级且
  `committable:false`；提交接口拒绝降级结果。

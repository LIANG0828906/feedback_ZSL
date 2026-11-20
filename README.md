# Glyph Atlas Workbench

本地、确定性的字形轮廓光栅化与位图图集工作台。输入为 JSON 字体描述、目标尺寸、子像素偏移、分块尺寸、并行度和资源预算；输出为抗锯齿覆盖率图、字形位图、RGBA 图集、度量表、哈希清单、快照和逐字节稳定的轨迹日志。

实现不读取系统字体，不访问网络，不依赖外部服务，也不使用 Docker。运行依赖仅为 Python 3.11+、NumPy 和 Tkinter（仅 GUI 需要）。

## 快速开始

```bash
# 环境自检（Python、NumPy、Tkinter、临时目录可写）
python -m glyph_atlas doctor

# 生成两套确定性本地样本、基线、轨迹和快照
python -m glyph_atlas generate --output samples/generated --seed 1947

# 运行配置并复核图集/度量/清单哈希
python -m glyph_atlas run --config samples/generated/mixed_nonzero/config.json
python -m glyph_atlas verify --config samples/generated/mixed_nonzero/config.json

# 预算耗尽、恢复和快照校验的专项验收工件
python scripts/run_acceptance.py --output samples/acceptance

# 测试
python -m unittest discover -v

# GUI（需要 Tkinter 与图形显示）
python -m glyph_atlas gui --config samples/generated/mixed_nonzero/config.json
```

若一次运行因预算中止，先增大配置中的对应预算，再执行：

```bash
python -m glyph_atlas run --config path/to/config.json --resume
```

## 实际环境核查

本次交付所在执行环境为 WSL2/Linux，不是 Windows。实测结果：

- Python `3.14.4`：可用，高于要求的 3.11。
- NumPy `2.5.5`：可导入。
- Tkinter：`ModuleNotFoundError: No module named 'tkinter'`，当前 Linux 环境不可导入。
- 临时目录 `/tmp`：可写。
- 无 Docker、无外部服务、无网络字体依赖。

因此本环境完整验证了无界面的解析、光栅化、调度、预算、快照、图集、CLI 和测试；GUI 代码已完成但无法在此环境实际打开窗口。Windows 目标环境应使用 Python 3.11 官方安装包，并执行：

```powershell
python -m tkinter
python -m glyph_atlas doctor
```

`doctor` 的 `ok` 应为 `true`。Windows Python 官方安装包通常附带 Tkinter；若 `python -m tkinter` 失败，需要修复/重装 Python 的 Tcl/Tk 组件。

## 目录

```text
glyph_atlas/
  font.py         字体 JSON 解析、曲线扁平化、复合字形展开和错误定位
  raster.py       统一覆盖率扫描转换、子像素采样和量化
  scheduler.py    固定并行度调度、预算检查、稳定提交
  snapshot.py     版本化、哈希链、原子替换快照
  atlas.py        货架布局、度量表、清单和哈希一致性检查
  png.py          无外部依赖的确定性 RGBA PNG 编解码
  pipeline.py     多目标运行、基线、轨迹和输出装配
  generator.py    确定性本地字体/配置/样本生成器
  gui.py          Tkinter 字形、分块、完成块着色和资源占用界面
  cli.py          generate/run/verify/gui/doctor
tests/            16 个自动化测试
scripts/          repeatable acceptance runner
samples/generated/ 两套字形集、四种尺寸/偏移组合及基线
samples/acceptance/ 预算耗尽、恢复、版本/哈希链拒绝的轨迹与结果
```

## 字体描述格式

坐标采用字体单位，y 轴向上。轮廓由显式点表和段表组成；一条轮廓从 `start` 开始，段的 `to` 指向下一个端点，最后隐式闭合并回到起点。点可以写为 `[x, y]` 或 `{"x": ..., "y": ...}`。

```json
{
  "format": "glyph-font/1",
  "name": "local-font",
  "units_per_em": 1000,
  "default_fill_rule": "nonzero",
  "curve_tolerance": 0.5,
  "glyphs": [
    {
      "name": "O",
      "unicode": 79,
      "advance_width": 700,
      "fill_rule": "nonzero",
      "contours": [
        {
          "direction": "counterclockwise",
          "points": [[100, 0], [100, 100], [0, 0], [0, 100], [100, 200]],
          "segments": [
            {"type": "quadratic", "control": 1, "to": 2},
            {"type": "cubic", "control1": 3, "control2": 4, "to": 0}
          ]
        }
      ]
    }
  ]
}
```

实际文件把所有字形放在 `glyphs` 数组内。复合字形示例如下：

```json
{
  "name": "Aring",
  "unicode": 197,
  "advance_width": 700,
  "components": [
    {"glyph": "A"},
    {
      "glyph": "ring",
      "transform": {"a": 0.42, "b": 0, "c": 0, "d": 0.42, "e": 350, "f": 725}
    }
  ]
}
```

解析器支持：

- `line`、`quadratic`/`quad`、`cubic` 段。
- 圈向声明 `clockwise`/`counterclockwise`；实际方向由有向面积复算并保留。
- 二次/三次贝塞尔自适应 de Casteljau 扁平化。
- 多层复合引用和六参数仿射变换。
- 轮廓孔洞；非零环绕依赖有向轮廓，奇偶规则不依赖方向。
- `blank` 显式空白字形。

点索引越界、控制点缺失、段缺失、不可达点、缺失引用、复合循环、自引用和重复 Unicode 映射都会在载入时返回 `E_GLYPH_INVALID`，并携带 `glyph`、`contour`（以及可用的 `segment`/`point`/`component`）定位信息。

## 运行配置

```json
{
  "format": "glyph-atlas-config/1",
  "font": "font.json",
  "glyphs": ["space", "O", "A", "Aring", "Oacute"],
  "targets": [
    {"name": "ui18", "pixel_size": 18, "subpixel_x": 0.1875, "subpixel_y": 0.15625},
    {"name": "ui32", "pixel_size": 32, "subpixel_x": 0.25, "subpixel_y": 0.5}
  ],
  "tile": {"width": 10, "height": 10},
  "parallelism": 2,
  "samples": {"x": 4, "y": 4},
  "quantization_step": 1,
  "fill_rule": "nonzero",
  "budget": {
    "max_task_sampling_points": 2000000,
    "max_task_memory_bytes": 67108864,
    "max_total_sampling_points": 20000000,
    "max_total_memory_bytes": 536870912
  },
  "atlas": {"width": 512, "height": 256, "padding": 2},
  "coverage_tolerance": 0,
  "output_dir": "output",
  "snapshot_dir": "snapshots",
  "temp_dir": ".tmp"
}
```

`glyphs` 可以是 `"all"` 或字形名数组。目标尺寸按每 em 像素数解释，`pixel_size / units_per_em` 是字体单位到像素的缩放。`budget` 的四项均可省略或设为 `null`。

## 光栅化与并行分块

核心扫描转换位于 `glyph_atlas/raster.py`，整字形真值和分块任务共用同一函数：

1. 轮廓先转换到全局像素坐标，再按目标 `subpixel_x`/`subpixel_y` 平移。
2. 每个像素使用 `samples.x × samples.y` 个子采样点；横纵采样都包含半个子像素偏移。
3. 每个子扫描线收集所有非水平边的交点，按 x 稳定排序；根据 `<= sample.x` 的累计有向边数计算环绕数。
4. 非零规则取 `winding != 0`，奇偶规则取左侧穿越次数为奇数；因此孔洞在跨块时仍按完整轮廓判定。
5. 每像素覆盖率为内部子采样数除以总子采样数。
6. 覆盖率先映射到 `[0,255]`，然后按 `quantization_step` 做 `floor(v/step + 0.5) * step` 量化并截断。

分块像素矩形严格为半开区间 `[x, x+w) × [y, y+h)`，所以相邻块没有重叠像素或缺失像素。块内采样使用全局坐标，不使用局部坐标重新起算。提交顺序固定为行优先的 `(tile_y, tile_x)`，与工作线程完成顺序无关。

调度器使用配置指定的固定 `ThreadPoolExecutor(max_workers=parallelism)`。`set_parallelism()` 拒绝任何运行时改动；同一字形重复 `submit_glyph()` 返回 `E_BUSY`。

每个字形完成后，会把分块拼接覆盖率与同一 `make_render_plan` 下的整体单块光栅化比较。配置 `coverage_tolerance=0` 时，本样本的所有字形、尺寸、子像素偏移和孔洞复合字形差值均为 0；超差返回 `E_TILE_MISMATCH`，并给出块坐标、像素坐标和差异摘要。

## 资源预算

每块提交前计量：

- `sample_points = tile_width * tile_height * samples_x * samples_y`
- `memory_bytes`：包含交点、采样缓冲、覆盖率缓冲和固定开销的确定性保守估算

同时检查单块和累计四项预算。检查发生在块提交之前；失败块不进入覆盖率数组，也不写入快照。错误 `E_BUDGET_EXCEEDED` 包含 `task`、`coord`、本次用量、已提交用量、已预留用量、限制名和限制值。磁盘快照保留最后一次连续提交的块，调用方增大预算后用 `--resume` 从该前缀继续。

## 快照与恢复

每个“字形 × 目标”对应一个快照。每次连续块提交后都以临时文件、`fsync` 和 `os.replace` 原子替换快照。快照包含：

- `format`、`version`、字体哈希、完整渲染签名。
- 分块表、块坐标/尺寸、已完成块索引。
- 每块覆盖率 Base64、采样数、内存估算、量化误差和内容 SHA-256。
- 前一链哈希、本块链哈希和 `head_hash`。
- 填充规则、采样网格、量化步长和预算配置。
- 成功运行后附加覆盖率、图集、度量表和快照头哈希。

载入时验证版本、格式、字体哈希、渲染签名、采样/量化、填充规则、块计划、覆盖长度、内容哈希和完整哈希链。任一不匹配均返回 `E_SNAPSHOT_MISMATCH`。恢复只解码磁盘快照中的已完成块，不依赖进程内状态，也不会重复累加已提交块。`outputs.glyph_coverage_sha256` 和 `outputs.glyph_snapshot_head` 也用于校验输出与快照一致。

## 图集与哈希

图集使用确定性货架布局，按配置字形顺序放置。每个目标输出：

- `atlas.png`：RGBA8，RGB 为白色，alpha 为量化覆盖率。
- `coverage/*.pgm`：每个字形的量化覆盖率字节图（P5）。
- `metrics.json`：图集矩形、advance、bearing、包围盒、Unicode、覆盖率和快照头。
- `manifest.json`：覆盖率组合哈希、PNG/度量文件哈希、字形数和每字形快照头。

`verify` 会同时校验 PNG、PGM 覆盖率和度量文件哈希、度量表中的源覆盖率哈希，以及度量表逐字形覆盖率哈希重新组合后是否等于清单中的覆盖率快照哈希。

## 轨迹与确定性

`trace.jsonl` 记录字形提交、开始/恢复、块提交、块完成、预算失败、取消和字形完成。块事件包含坐标、状态、采样点、内存、用量、内容哈希和链哈希。事件在执行后按“字形提交序号 + 事件阶段 + 任务序号 + 坐标”稳定排序并重编号，不记录时间戳，因此不受线程完成顺序影响。

生成器不采样系统时间或随机系统字体。相同 seed 和操作序列下，字体、配置、基线、轨迹、快照、图集、度量表和清单逐字节一致；该性质由 `test_generated_artifacts_are_byte_identical` 在两个独立目录中验证。

## 业务码

| Code | 含义 |
| --- | --- |
| `E_GLYPH_INVALID` | 点/段/控制点/引用缺失、循环或自引用、非法映射 |
| `E_BUSY` | 同一字形重复提交 |
| `E_BUDGET_EXCEEDED` | 单块或累计预算超出 |
| `E_TILE_MISMATCH` | 分块覆盖率与整体真值超差 |
| `E_ATLAS_OVERFLOW` | 图集空间不足或度量越界 |
| `E_SNAPSHOT_MISMATCH` | 快照版本/签名/哈希链/输出哈希不匹配 |
| `E_CANCELLED` | 用户取消，保留已提交快照 |
| `E_CONFIG_INVALID` | 配置或参数非法 |

## GUI

Tkinter 工作台提供：

- 输入文本并按 Unicode 映射为字形集。
- 修改目标尺寸和子像素 X/Y。
- 启动和取消后台光栅化。
- 覆盖率字形预览与分块网格。
- 已完成分块绿色着色。
- 已用采样点、内存和块进度显示。

取消后不会提交正在执行的块；已提交块已在磁盘快照中，可换用 CLI `--resume` 或下一次运行继续。

## 验收结果

自动化测试：

```text
Ran 16 tests
OK
```

覆盖内容：

- 混合二次/三次曲线、复合字形、孔洞和两种填充规则。
- 解析错误定位和复合自引用拒绝。
- 分块结果与整体真值零差异。
- 预算中断、磁盘恢复、取消恢复和已完成块不重复。
- 快照版本与覆盖率哈希链篡改拒绝。
- `E_BUSY`、运行时并行度提高拒绝、图集溢出和 `E_TILE_MISMATCH`。
- 输出/清单哈希一致性和两目录逐字节确定性。

专项验收工件见 `samples/acceptance/results.json`。本次实测：

- 预算失败：`E_BUDGET_EXCEEDED`，提交块 `[0]`，下一块坐标 `[0,1]`，已用 `1600` 采样点，限制 `1600`。
- 恢复后覆盖率为 `b101bbe66fafb1464f2779092b28be82ae82a299e6cdddc5c3b2c85afb818828`。
- 干净运行覆盖率相同，`coverage_equal_after_resume=true`。
- 恢复与干净运行的整体/分块最大差值均为 `0`。
- 篡改快照版本和覆盖率内容均返回 `E_SNAPSHOT_MISMATCH`。
- 轨迹文件：`budget_trace.jsonl`、`resume_trace.jsonl`、`clean_trace.jsonl`。

正式样本摘要见 `samples/generated/summary.json`。其中：

- `mixed_nonzero`：18px 与 32px，非零环绕，包含 `O` 孔洞、`A` 孔洞、`Aring` 多层孔洞复合、`Oacute` 复合。
- `evenodd_composite`：24px 与 40px，奇偶规则，包含 `B` 双孔和 `Bdieresis` 复合。


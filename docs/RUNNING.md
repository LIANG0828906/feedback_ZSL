# GlyphForge 本地运行与验收说明

## 1. 环境

目标环境：

- Windows 10 及以上
- Python 3.11（64 位）
- pip
- 无需 GPU、Docker、容器、外部服务、系统字体或网络字体文件

依赖只包括：

- PySide6：图形界面与 Qt 绘图
- NumPy：子像素采样、覆盖率累加和数组处理

依赖版本使用安装时 pip 可获得的最新稳定版，约束记录在 `requirements.txt`：

```text
PySide6>=6.8,<7
numpy>=2.0,<3
```

本次实际验证环境为 WSL2 Linux、Python 3.14.4、NumPy 2.5.5、PySide6/Qt 6.12.0，Qt 使用 `offscreen` 平台完成离屏窗口绘制与冒烟测试。代码只使用跨平台标准库、NumPy 与 PySide6 API；交付环境没有可用的原生 Windows 会话，因此 Windows GUI 未在当前机器实机启动。

## 2. Windows 安装

在项目根目录打开 PowerShell：

```powershell
py -3.11 -m venv .venv
.venv\Scripts\Activate.ps1
python -m pip install --upgrade pip
python -m pip install -r requirements.txt
```

若 PowerShell 禁止执行激活脚本，可继续直接使用虚拟环境解释器：

```powershell
.venv\Scripts\python.exe -m pip install -r requirements.txt
.venv\Scripts\python.exe -m glyphforge --help
```

验证 Qt 与 NumPy：

```powershell
python -c "import numpy; from PySide6.QtWidgets import QApplication; print(numpy.__version__); print('Qt OK')"
```

有桌面的 Windows 会使用默认 `windows` Qt 平台插件。无显示环境的 Linux 可设置 `QT_QPA_PLATFORM=offscreen` 做无头验证。

## 3. 生成确定性样本

```powershell
python -m glyphforge generate --out samples
```

生成器不使用随机数、当前时间、系统字体或网络。`--seed` 是样本集标识参数，同一版本代码和同一操作序列下输出逐字节一致。

生成内容：

- `samples/fonts/mixed.json`：直线、二次/三次贝塞尔、孔洞、复合引用和 Unicode。
- `samples/fonts/holes.json`：同向/反向孔洞与曲线极值。
- `samples/fonts/composites.json`：多个复合重音字形。
- `samples/configs/*.json`：多种尺寸、子像素偏移、量化步长、填充规则和分块尺寸。
- `samples/runs/<case>/`：`atlas.png`、`metrics.json`、`report.json`、`trace.jsonl`、`coverage/*.pgm`。
- `samples/manifest.json`：每个字形的覆盖率基线哈希、真值哈希、量化摘要和图集哈希。

覆盖的样本维度：

- 像素尺寸：16、20、24、28、32、36
- 子像素偏移：`(0,0)`、`(0.25,0)`、`(0.5,0.25)`、`(0,0.5)`、`(0.25,0.5)` 等
- 量化步长：`1/255`、`1/128`、`1/64`
- 填充规则：`nonzero`、`evenodd`
- 分块尺寸：4、5、6、7、8，刻意切过曲线极值和孔洞边缘

## 4. 一键验收

```powershell
python -m glyphforge validate --samples samples --out artifacts/validation
```

验收执行以下检查：

1. 复算 `manifest.json` 的全部基线，逐字形比较分块覆盖率哈希和整字真值哈希。
2. 每组样本再跑一次，比较 `trace.jsonl`、覆盖率 PGM、图集 PNG 和度量/报告文件是否逐字节一致。
3. 构造单任务采样预算拒绝：`O` 被拒绝，`eight` 继续完成，图集只含 `eight`。
4. 在分块检查点取消，生成原子快照；重启后复用已完成分块、重算其余分块，并与干净运行比较覆盖率/图集哈希。
5. 修改快照分块内容哈希，确认返回 `E_SNAPSHOT_MISMATCH`。
6. 用 `1/10` 与 `1/255` 两种量化步长重算，检查每字形新旧量化误差摘要和非零误差差值。

报告写入 `artifacts/validation/validation-report.json`，相关轨迹位于：

- `artifacts/validation/cases/*/trace.jsonl`
- `artifacts/validation/determinism/*/trace.jsonl`
- `artifacts/validation/budget-isolation/trace.jsonl`
- `artifacts/validation/snapshot-resumed/trace.jsonl`
- `artifacts/validation/quantization-change/trace.jsonl`

本次结果：16/16 检查通过。快照开始时可复用分块数为 3，恢复时重算 26 块；预算拒绝的 `O` 未进入图集；量化步长变化产生非零误差差值。

## 5. 单元与集成测试

```powershell
python -m unittest discover -s tests -v
```

Linux 无头环境：

```bash
QT_QPA_PLATFORM=offscreen python -m unittest discover -s tests -v
```

当前结果：22 个测试全部通过，覆盖：

- 缺失点、未闭合轮廓、悬空复合引用的 `E_GLYPH_INVALID` 定位
- 轮廓方向规范化与复合字形包围盒
- `nonzero` / `evenodd` 孔洞语义
- 零量化步长的 `E_QUANT_INVALID`
- 分块覆盖率与整字真值完全一致
- 批大小降级、预算拒绝明细、重复提交 `E_BUSY`
- 稳定提交序和并行波次上限
- 快照取消恢复、内容哈希/版本篡改拒绝
- 图集溢出不产生 `atlas.png`/`metrics.json`
- 预算拒绝字形隔离、量化误差差值和双跑逐字节一致
- Qt 离屏窗口创建、图集/分块网格绘制

## 6. CLI 运行

### 6.1 正常光栅化

```powershell
python -m glyphforge run `
  --font samples/fonts/mixed.json `
  --text "AOBÁ" `
  --config samples/configs/mixed-24-nonzero-q255.json `
  --out artifacts/manual-run
```

输出：

```text
artifacts/manual-run/
  atlas.png
  metrics.json
  report.json
  snapshot.json
  trace.jsonl
  coverage/<glyph>.pgm
```

`trace.jsonl` 每行均为稳定键序的规范 JSON，不写线程完成时间或绝对路径。包含：

- 字体哈希、文本、量化步长、填充规则、预算与并行度
- 字形计划
- 每块提交序号、波次、等待轮数、预算计量、内容哈希
- 复用量、重算量、整字覆盖率和真值哈希
- 量化摘要和步长变更差值

### 6.2 强制终止与快照恢复

诊断用协作式停止：

```powershell
python -m glyphforge run `
  --font samples/fonts/holes.json `
  --text "8OB" `
  --config samples/configs/holes-28-nonzero-q255.json `
  --out artifacts/recovery `
  --stop-after-tiles 1
```

该命令在分块检查点停止，返回 `E_CANCELLED`，保留 `artifacts/recovery/snapshot.json`，不写新的退出报告。

恢复：

```powershell
python -m glyphforge run `
  --font samples/fonts/holes.json `
  --text "8OB" `
  --config samples/configs/holes-28-nonzero-q255.json `
  --out artifacts/recovery `
  --resume
```

恢复只读取磁盘快照中的已完成分块。量化步长、填充规则、预算、字体哈希、字形顺序或分块表变化时，快照签名不一致，返回 `E_SNAPSHOT_MISMATCH`，不会把旧覆盖率混入新配置。

### 6.3 量化步长变更比较

先用旧配置运行并保留 `report.json`，再修改配置中的 `quant_step`：

```powershell
python -m glyphforge run `
  --font samples/fonts/mixed.json `
  --text "AOB" `
  --config samples/configs/mixed-24-nonzero-q255.json `
  --out artifacts/quantized `
  --compare-report artifacts/manual-run/report.json
```

每个成功字形会重新采样和量化，报告中的 `comparison` 字段包含旧步长、新步长、旧/新绝对误差、误差差值和总覆盖率变化。

## 7. 图形工作台

```powershell
python -m glyphforge ui `
  --font samples/fonts/mixed.json `
  --config samples/configs/mixed-24-nonzero-q255.json `
  --text "AOBÁ"
```

界面能力：

- 输入任意 Unicode 文本；重复字符只按首次出现提交唯一字形
- 切换像素尺寸、子像素 X/Y、填充规则、量化步长和分块尺寸
- “启动光栅化”和“恢复快照”
- 分块检查点协作式取消
- 图集预览；字形覆盖率预览；分块网格；完成块绿色、复用块蓝色、失败/拒绝块红色
- 显示并行度、配置预算、峰值任务占用、复用/重算块数和快照完成块数
- 轨迹/事件日志

取消不会替换 `last_result`，因此界面继续显示最后一次完整发布的图集。新运行失败或取消时不会把半合成字形写入当前图集。

界面运行会写入 `artifacts/ui-last/`，用于人工检查轨迹和产物。

## 8. 字体 JSON 格式

最小轮廓示例：

```json
{
  "format": "glyphforge.font/1",
  "units_per_em": 1000,
  "ascender": 900,
  "descender": -150,
  "glyphs": {
    "A": {
      "advance": 600,
      "unicode": [65],
      "contours": [
        {
          "start": {"x": 80, "y": 0},
          "direction": "ccw",
          "segments": [
            {"type": "line", "to": {"x": 520, "y": 0}},
            {"type": "quadratic", "control": {"x": 450, "y": 500}, "to": {"x": 300, "y": 700}},
            {"type": "cubic", "control1": {"x": 280, "y": 700}, "control2": {"x": 150, "y": 700}, "to": {"x": 80, "y": 0}}
          ]
        }
      ]
    }
  },
  "unicode_map": {"65": "A"}
}
```

复合字形：

```json
{
  "Aacute": {
    "advance": 600,
    "unicode": [193],
    "components": [
      {"glyph": "A"},
      {"glyph": "acute", "transform": [1, 0, 0, 1, 40, 0]}
    ]
  }
}
```

`transform` 使用 `[a,b,c,d,e,f]` 仿射矩阵。轮廓必须显式回到 `start`；缺失点、非有限数、零面积、未闭合、循环或悬空复合引用均返回 `E_GLYPH_INVALID`，不会返回部分字形。

## 9. 调度、预算与覆盖率口径

### 确定性调度

1. 字形按文本首次出现顺序编号，分块按 `(row, col)` 排序并分配连续提交序号。
2. 调度器预先静态切分波次，每波任务数不超过配置的 `parallelism`，累计 `total_bytes` 不超过 `global_memory_bytes`。
3. 同一波内的线程完成顺序不写入结果或轨迹；合成阶段始终按分块坐标排序。
4. 后一波只在前一波全部结束后开始，避免线程完成时序影响预算准入和日志。

### 单任务预算

每块计量：

```text
sample_count             = width * height * supersample^2
coverage_buffer_bytes    = width * height * 4
temporary_scanline_bytes = effective_batch_size * 64
total_bytes              = coverage_buffer_bytes + temporary_scanline_bytes
```

若超限且降级序列含 `shrink_batch`，批大小逐次减半，最低到 `min_sampling_batch_size`；仍超限时在 `reject` 阶段拒绝启动。拒绝错误包含字形、分块坐标、原/有效批大小、采样数、各类字节数、超出项和降级序列。

### 边界无缝

每个像素的采样点由全局图像坐标决定：

```text
sample = glyph_origin + integer_pixel + (subsample + 0.5) / supersample
```

分块只是裁剪需要扫描的像素范围，不改变采样点、环绕数或填充规则。因此孔洞边缘、曲线极值和分块接缝处不会发生覆盖翻转或重复累加。拼接时另做严格区域占用检查，拒绝重叠或缺失块。

整字单块真值使用同一采样域独立计算。默认 `coverage_tolerance=0`，逐像素字节差超限时记录 `E_COVERAGE_MISMATCH`，包含字形、分块坐标、最近接缝坐标、首个超差像素和超差像素数。

## 10. 快照格式

快照使用规范 JSON，包含：

- 格式与版本
- 字体哈希、字形顺序、分块表
- `pixel_size`、子像素偏移、`supersample`、`quant_step`、`fill_rule`
- 全局/单任务预算和降级序列
- 已完成分块覆盖率 Base64、内容 SHA-256 和预算摘要
- 排序后的 `completed_indices`
- 从运行配置开始逐块链接的 SHA-256 `hash_chain`

保存流程是：同目录临时文件写入、`fsync`、`os.replace` 原子重命名。载入时先校验版本、运行配置、每块形状/字节数/内容哈希，再重算整条哈希链和终值。任一不符即返回 `E_SNAPSHOT_MISMATCH`。

## 11. 输出与业务码

业务错误统一为：

```json
{
  "code": "E_BUDGET_EXCEEDED",
  "message": "...",
  "details": {}
}
```

命令行退出码：

- `0`：成功
- `1`：验收检查失败
- `2`：配置、字体、快照等业务错误
- `3`：存在字形级 `E_COVERAGE_MISMATCH`
- `4`：存在字形级 `E_BUDGET_EXCEEDED`
- `5`：`E_ATLAS_OVERFLOW`
- `130`：协作式取消/诊断停止

## 12. 已知边界

- 为保证跨线程和跨重复运行的严格确定性，调度采用静态波次；某块提前完成不会立即释放槽位给下一波。
- 真值比对会额外执行一次整字形光栅化，因此耗时约为分块计算与真值计算之和。
- 图集采用确定性的货架式布局，不做旋转、位图去重或复杂最优装箱。
- 快照校验能检测未同步更新的内容/链篡改；它不是带密钥的签名格式。

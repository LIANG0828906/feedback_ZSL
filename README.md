# GlyphForge

本地、确定性的矢量字形轮廓光栅化与图集工作台。它读取 JSON 字体描述，支持二次/三次贝塞尔混合轮廓、复合字形、轮廓孔洞、非零环绕与奇偶填充，按像素尺寸和子像素偏移生成覆盖率字节图、PNG 图集、度量表、轨迹日志和可恢复快照。

核心目标是同时满足三个约束：

- **调度确定**：静态波次调度按提交序号运行，并行度与预算只取配置值；分块完成后按 `(row, col)` 坐标定序合成，不依赖线程完成顺序。
- **预算强制**：逐块计量采样点、覆盖率缓冲和临时扫描线内存；按声明序列缩小批大小，仍超限则拒绝启动，并将整个字形隔离在图集之外。
- **覆盖率口径一致**：所有分块共用同一像素/子像素采样域，分块只裁剪扫描范围，不重采样；完成后与整字形单块真值逐像素比较。

## 快速开始

目标运行环境为 Windows 10 及以上、Python 3.11、pip。完整安装与运行说明见 [docs/RUNNING.md](docs/RUNNING.md)。

```powershell
py -3.11 -m venv .venv
.venv\Scripts\activate
python -m pip install --upgrade pip
python -m pip install -r requirements.txt
python -m glyphforge generate --out samples
python -m glyphforge validate --samples samples --out artifacts/validation
python -m glyphforge ui --font samples/fonts/mixed.json --config samples/configs/mixed-24-nonzero-q255.json --text "AOBÁ"
```

不使用 Docker、外部服务、系统字体或网络字体资源。样本生成器完全在本地确定性地构造字体描述、配置、基线哈希和轨迹。

## 交付内容

- `glyphforge/parser.py`：字体描述校验、曲线展平、轮廓方向规范化、复合引用与 Unicode 映射。
- `glyphforge/raster.py`：环绕数扫描、抗锯齿子像素采样、量化误差摘要、覆盖率哈希与严格拼接。
- `glyphforge/scheduler.py`：预算计量、降级/拒绝、稳定提交序、配置并行度波次调度。
- `glyphforge/snapshot.py`：分块覆盖率、版本、原子写入和 SHA-256 哈希链校验。
- `glyphforge/atlas.py`：整字提交后的货架式图集打包、度量表和确定性 PNG/PGM 编码。
- `glyphforge/pipeline.py`：端到端编排、整字真值比对、失败隔离、量化新旧误差差值和 JSONL 轨迹。
- `glyphforge/samples.py`：三套本地字体、六组尺寸/偏移/量化/填充规则样本与基线。
- `glyphforge/ui.py`：PySide6 工作台、图集预览、分块网格与完成着色、资源计量、取消与快照恢复。
- `samples/`：已生成的字体、配置、运行结果、覆盖率和基线 `manifest.json`。
- `artifacts/validation/`：本次实际验收报告、预算隔离、快照恢复和量化变更轨迹。
- `tests/`：解析、光栅、调度、预算、快照、图集隔离、确定性和 Qt 离屏冒烟测试。

## 验收状态

最近一次本地验收：

- `python -m unittest discover -s tests -v`：22 个测试全部通过。
- `python -m glyphforge validate ...`：16 项检查全部通过。
- 六组样本的分块覆盖率哈希均与整字形真值哈希一致，双跑轨迹和覆盖率文件逐字节一致。
- 预算测试中 `O` 被 `E_BUDGET_EXCEEDED` 拒绝，后续 `eight` 正常完成，图集只包含 `eight`。
- 快照测试中复用 3 个已完成分块、重算 26 个未完成分块，恢复后覆盖率与图集哈希不变；篡改后返回 `E_SNAPSHOT_MISMATCH`。
- 量化测试从 `1/10` 切换到 `1/255`，重算并输出非零新旧误差差值。
- Qt 离屏冒烟测试实际创建窗口、绘制图集和分块网格；取消路径保留上一次已发布图集。

详细结果与环境记录见 [docs/RUNNING.md](docs/RUNNING.md)。

## 业务码

| 业务码 | 行为 |
| --- | --- |
| `E_GLYPH_INVALID` | 字体解析失败；带字形、轮廓序号/字段路径，不输出部分轮廓 |
| `E_QUANT_INVALID` | 量化步长非有限或不在 `(0, 1]` |
| `E_BUSY` | 同一字形在一次调度提交中重复出现 |
| `E_BUDGET_EXCEEDED` | 分块预算拒绝；带字形、分块坐标与完整计量明细 |
| `E_COVERAGE_MISMATCH` | 分块结果超出整字真值容差；带接缝和超差像素数 |
| `E_ATLAS_OVERFLOW` | 图集空间或度量边界不足；不写出新图集 |
| `E_SNAPSHOT_MISMATCH` | 快照版本、运行配置、分块哈希或哈希链不符 |

`E_BUDGET_EXCEEDED` 与 `E_COVERAGE_MISMATCH` 是字形级结果：报告和轨迹记录错误，其他字形继续处理，失败字形不进入图集。CLI 分别以退出码 4 和 3 表示这两类字形级错误。

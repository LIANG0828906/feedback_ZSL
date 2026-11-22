# OCR 文字检测识别数据处理管线与结果验证工具

面向游戏截图与互动媒体素材的离线 OCR 数据处理管线：确定性样本生成、多版本
结果文件加载/导出、文本行区间状态机、增量重识别、可暂停/可重启的持久化
检查点，以及独立结果验证工具。全程离线，不依赖任何外部 OCR 服务。

## 环境准备

- Python 3.10 及以上（Windows 与 Linux 均可；本仓库在 Linux + Python 3.14 上
  完成开发与验证，Windows 下未实际验证，以下命令为通用说明）。
- 依赖：NumPy、Pillow（运行），pytest（测试）。

安装（Windows PowerShell 示例）：

```powershell
py -m venv .venv
.venv\Scripts\Activate.ps1
pip install numpy pillow pytest
```

Linux/macOS：

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install numpy pillow pytest
```

## 快速开始

```bash
# 1. 生成确定性样本（固定种子：图像 + 真值 + v1/v2/v3 结果描述样本）
python tools/generate_samples.py --out data --seed 42

# 2. 运行管线（可并行；识别并行、检查点写入串行）
python tools/run_pipeline.py --data data --workers 4 --buffer-limit 2

# 3. 独立验证（仅从持久化区间结果与真值指纹出发）
python tools/validate_results.py data

# 4. 导出指定 schema 版本并生成 E_LOSSY 降级清单
python tools/run_pipeline.py --data data --export 1.0

# 5. 测试
python -m pytest tests/ -q
```

暂停/恢复演示：`python tools/run_pipeline.py --data data --max-intervals 5`
（提交 5 个区间后退出，退出码 2，不生成完整导出）；再次运行同一命令即从
已提交区间继续，直至完成。

## 目录结构

- `ocr_pipeline/generator.py` — 固定种子样本生成器（多分辨率、单/多行、倾斜、
  低对比度、稀疏/密集），真值含四点包围框、文本、字符数、平均笔画宽。
- `ocr_pipeline/schemas/` — v1.0/v2.0/v3.0 三个 schema 的加载器与导出器；
  版本差异覆盖字段命名、坐标表达（像素整数 vs 归一化浮点）、逐字符置信度
  可选性、语言与阅读顺序标记位置。
- `ocr_pipeline/coords.py` — 像素整数 ↔ 归一化浮点双向确定性换算（声明精度 +
  可选取舍去向）。
- `ocr_pipeline/engine.py` — 确定性本地检测识别引擎（纯函数，无外部服务）。
- `ocr_pipeline/state.py` — 区间状态机（待识别/识别中/已提交/失败）。
- `ocr_pipeline/scheduler.py` — 增量重识别调度与全量一致比对。
- `ocr_pipeline/checkpoint.py` — 原子、串行的区间检查点存储。
- `ocr_pipeline/pipeline.py` — 主编排：续跑、取消、线程池并行、导出。
- `ocr_pipeline/validator.py` — 独立结果验证工具。
- `tools/` — CLI：样本生成、管线运行、结果验证。
- `tests/` — 离线 pytest 测试（41 项）。

## 错误码

| 代码 | 含义 | 携带信息 |
| --- | --- | --- |
| E_VER | 未知 schema 版本或必填字段缺失 | 字段路径 |
| E_COORD | 坐标换算越出图像边界 | 文本行编号、越界点序号 |
| E_MAP | 检测识别映射校验失败（阅读顺序冲突/包围框退化等） | 图像标识、文本行编号 |
| E_STATE | 非法区间状态迁移 | 当前状态、目标状态（原状态保持不变） |
| E_DIFF | 增量与全量识别不一致 / 验证发现不一致 | 图像标识、文本行编号、字段名 |
| E_LOSSY | 导出降级警告（非异常，列入清单） | 字段路径、原值、降级处置方式 |

## 关键设计

- **内部结果模型**：像素整数四点包围框为规范坐标；三个版本加载后映射为同一
  模型并保留 `source_version`。
- **坐标往返**：归一化浮点按声明精度（默认 6 位小数）量化，取舍去向可参数化
  （half_up/half_even/floor/ceil）；默认精度下像素→归一化→像素逐点相等。
- **确定性引擎**：识别结果是（图像、真值、参数）的纯函数，与缓存和提交历史
  无关，因此增量重识别与空缓存全量识别逐行一致，并由 `E_DIFF` 比对器证明。
- **提交边界**：已提交区间冻结合并阈值、置信度下限与源版本快照；参数调整只
  失效未提交区间。
- **检查点**：每提交一个区间即原子写入（临时文件 + `os.replace`，全局锁串行
  化）；重启时内容哈希、源版本、参数版本、算法版本任一不匹配即整体作废快照
  并全量重识别。强制终止后只能依据已持久化且版本匹配的区间恢复，不会声称
  能生成完整导出结果。
- **并发边界**：识别可在线程池并行（最大并行度与每线程图像缓冲上限可配），
  提交按（图像、区间）规范顺序串行落盘；并行与串行产出逐字节一致的区间记录
  与相同提交顺序。

## 实际验证结果（本机，Linux，Python 3.14）

- `pytest`：41 项测试全部通过，覆盖多版本往返、坐标往返、E_VER/E_COORD/
  E_MAP/E_STATE/E_DIFF/E_LOSSY、增量=全量、检查点作废与续跑、并行=串行。
- 样本：种子 42 生成 8 张图像、29 个真值区域、23 条检测文本行。
- 暂停/恢复：5/14 区间提交后退出（退出码 2），重启续跑至 14/14，验证通过。
- 导出：v1.0 产生 23 条 E_LOSSY 警告（逐字符置信度丢弃），v2.0/v3.0 无降级；
  重复导出逐字节一致；导出产物可被对应版本加载器读回并还原等价模型。
- 独立验证工具：`OK: 8 images, 23 lines validated`。

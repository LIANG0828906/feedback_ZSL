# MSHDR Workbench

面向游戏与互动媒体素材的多光谱高动态范围合成、引导滤波增强与目标色域重建工作台。项目完全在本地运行，不依赖网络、GPU、容器或外部服务。

## 已实现能力

- 固定种子生成 3 个场景；每个场景含 4 个光谱波段、3 档 HDR 曝光、含噪低对比度细节层、元数据和真值清单。
- 严格输入校验：缺失元数据/波段数不匹配为 `E_META`，损坏像素载荷为 `E_DECODE`，位深/版本漂移为 `E_VER`，曝光倒置为 `E_ORDER`。
- 可配置阶数的确定性波段配准、逆 gamma 相机响应线性化、可靠样本加权曝光融合。
- 以融合辐亮度为引导的引导滤波、阈值局部对比度增强，并通过真值声明的噪声残留与边缘保真度上/下限。
- 波段响应函数与 CIE CMF 积分得到 XYZ，Bradford 白点适配后转换到目标线性 RGB；感知意图使用向中性轴压缩而非通道裁剪，并输出越界掩码与色域边界坐标。
- 固定像素块四态状态机、已提交段元数据/参数快照锁定、线程池最大并行度与阻塞式像素预算。
- 每段原子检查点：融合辐亮度、去噪/增强细节、色度缓冲、线性/量化 RGB、越界数据、内容哈希和三类版本。
- 实时、断点恢复、全量重算三条路径严格逐像素一致；自动比对定位首个差异的字段、像素坐标和分段编号，失败返回 `E_DIVERGE`。
- 四类真实异常注入链路及坏段隔离重试；真实 `os._exit` 强制终止后仅依据已提交段恢复。
- 可选 tkinter 本地结果查看器。

## 环境

实际验证环境：

- Python `3.11.18`
- NumPy `2.4.6`
- SciPy `1.17.1`
- Pillow `13.0.0`
- tkinter `9.0`

从空环境准备：

```bash
uv python install 3.11
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -r requirements.txt
uv pip install --python .venv/bin/python -e .
.venv/bin/python -m mshdr doctor
```

也可以使用已创建的 `.venv`。所有运行依赖均可导入后离线工作。

## 快速开始

```bash
# 固定种子生成本地样本、元数据、真值与预览
.venv/bin/python -m mshdr generate --seed 20261123 --out samples
.venv/bin/python -m mshdr inspect --dataset samples/dataset.json

# 三场景、三路径处理并严格比对
.venv/bin/python -m mshdr run-paths \
  --dataset samples/dataset.json \
  --out outputs/paths \
  --checkpoint-root checkpoints/paths \
  --workers 4

# 单元测试
.venv/bin/python -m unittest discover -s tests -v

# 一键验收（测试、异常注入、强制终止恢复、三路径比对）
.venv/bin/python scripts/run_acceptance.py
```

查看结果：

- 三路径报告：`outputs/paths/paths_report.json`
- 单场景统计：`outputs/paths/full/<scene_id>/stats.json`
- 逐段误差：`outputs/paths/full/<scene_id>/segment_errors.json`
- 色域报告：`outputs/paths/full/<scene_id>/gamut_report.json`
- 实际验收记录：`docs/VALIDATION.md`
- 一键验收证据：`outputs/acceptance/acceptance_report.json`

可选 GUI：

```bash
.venv/bin/python -m mshdr gui \
  --dataset samples/dataset.json \
  --output-root outputs/paths
```

## 目录

```text
src/mshdr/                 算法、I/O、状态机、检查点、CLI
tests/                     unittest 自动化测试
scripts/run_acceptance.py  本地端到端验收
samples/                   固定种子样本、metadata.json、truth.json
checkpoints/               原子分段检查点
outputs/                   合成图像、色域、统计与比对证据
docs/TECHNICAL.md          技术设计与协议
docs/VALIDATION.md         当前机器实际验证结果
```

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_META` | 必需元数据缺失、波段数/尺寸等声明不匹配 |
| `E_DECODE` | 像素载荷损坏、哈希不匹配或无法解码；不填充、不修复 |
| `E_REG` | 配准偏移超出可校正范围；包含波段对和实测偏移 |
| `E_PARA` | 插值阶数、曝光权重、滤波/增强/并发参数越界或质量界失败 |
| `E_GAMUT` | 指定意图内无法完成色域映射；包含越界计数与最大越界量 |
| `E_STATE` | 非法分段状态迁移；原状态不变 |
| `E_VER` | 元数据、位深、参数或算法版本不匹配 |
| `E_ORDER` | 曝光时间非严格递增或非法 |
| `E_SEG` | 分段执行或注入坏段失败；已提交段保持隔离 |
| `E_DIVERGE` | 输出严格比对不一致；定位首个差异 |

详细算法、数据格式、检查点和复现协议见 `docs/TECHNICAL.md`。


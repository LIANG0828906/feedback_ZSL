# SciSeq Scientific Imaging Workbench

面向科学成像序列的本地、确定性 Python 组件与 PySide6 示例。项目覆盖样本生成、严格加载、亚像素配准、噪声估计、自适应去噪、特征提取、预算调度、状态机、原子检查点、三路径逐像素一致性校验、交互式预览与高精度导出。

## 环境

本次实际验证环境：

- Python 3.14.4（要求 3.11+）
- NumPy 2.5.5
- PySide6 6.12.0
- Linux/WSL 无头环境；GUI 以 Qt `offscreen` 平台完成冒烟测试

Linux/WSL 快速安装：

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python -e ".[test]"
```

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -e ".[test]"
```

## 快速开始

生成三组固定种子样本：

```bash
.venv/bin/python -m sciseq generate --out samples --overwrite
```

处理并导出小规模序列：

```bash
.venv/bin/python -m sciseq process \
  --sequence samples/small \
  --mode incremental \
  --checkpoint artifacts/checkpoints/demo \
  --export exports/small
```

启动桌面示例：

```bash
.venv/bin/python -m sciseq gui \
  --sequence samples/small \
  --checkpoint artifacts/checkpoints/gui
```

在 Windows 桌面直接运行 `sciseq-gui` 亦可。没有显示环境时，可用：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m sciseq.gui.app \
  --sequence samples/small --smoke-test
```

运行测试与完整验收：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m pytest -q
.venv/bin/python -m sciseq acceptance \
  --samples samples --output artifacts/acceptance.json
```

## 本地样本

| 组 | 尺寸 (H×W) | 帧数 | 种子 | 真值位移上限 | 噪声范围 |
|---|---:|---:|---:|---:|---:|
| `small` | 320×384 | 24 | 1101 | 3.2 px | 3–7 |
| `medium` | 512×640 | 56 | 2202 | 4.2 px | 5–11 |
| `large` | 768×1024 | 96 | 3303 | 5.0 px | 7–16 |

每组包含：

```text
samples/<group>/
  manifest.json
  truth.json
  clean_base.npy
  frames/frame_000000.npy ... frame_000095.npy
```

`truth.json` 记录每帧精确亚像素位移、噪声基底、参考几何中的亮斑/线段参数，以及每帧特征中心。帧文件和基准图均记录 SHA-256；加载器默认全量校验。

## 实际验收结果

完整验收运行耗时约 183.6 秒，三组共 176 帧。结果文件为 `artifacts/acceptance.json`。

### 质量指标

| 组 | 配准最大误差 | 配准均值误差 | 残留噪声最大值 | 边缘保真度最小值 | 特征中心最大误差 | 噪声估计平均相对误差 |
|---|---:|---:|---:|---:|---:|---:|
| small | 0.0654 px | 0.0172 px | 1.680 | 0.8915 | 0.489 px | 2.52% |
| medium | 0.1200 px | 0.0370 px | 2.301 | 0.8605 | 1.028 px | 2.74% |
| large | 0.2049 px | 0.0610 px | 2.859 | 0.8138 | 1.752 px | 2.78% |

声明阈值：配准最大误差 `<0.35 px`、残留噪声 `<3.25`、边缘保真度 `>0.78`、特征中心最大误差 `<2.5 px`、噪声估计平均相对误差 `<20%`。三组全部通过。

精度阶梯 `1× → 2× → 4× → 6×` 的平均配准残差在三组均单调不增。例如大规模组为 `0.4458 → 0.1630 → 0.0810 → 0.0634 px`。

### 三路径与并发一致性

| 组 | 全量 vs 增量差异像素 | 断点恢复 vs 全量差异像素 | 并行 vs 单线程差异像素 | 决策序列一致 |
|---|---:|---:|---:|---|
| small | 0 | 0 | 0 | 是 |
| medium | 0 | 0 | 0 | 是 |
| large | 0 | 0 | 0 | 是 |

偏移、去噪像素、特征中心与统计输出全部逐值一致。大规模组增量、恢复和单线程全量耗时分别约 25.44 s、29.63 s 和 38.27 s。

### 预览与导出闸门

紧内存预算 `60 MB` 下，小规模组自动选择：

- 模式：`preview`
- 降采样：`2×`
- 选择性去噪：按噪声从高到低选择 `15/24` 帧
- 内存估计：`57.375 MB`
- 预览耗时：约 `0.595 s`

导出前自动以高精度模式重算。对 15 个选择性去噪帧、460,800 像素比较，预览与高精度降采样结果的平均绝对偏差 `0.9957`，低于声明上限 `35.0`；最终导出内容为高精度结果，位于 `artifacts/preview_export/small`。

另以 `5 s / 512 MB` 约束大规模组，调度器自动选择 `8×` 降采样、48 帧选择性去噪，解析估计 `4.80 s`，实测 `3.19 s` 完成，满足声明时间预算。

### 错误码

验收实际触发并校验：

| 错误码 | 场景 | 定位信息 |
|---|---|---|
| `E_META` | 缺少 `manifest.frame_count` | 字段名 |
| `E_DECODE` | 第 2 帧文件损坏 | 帧号、字段名 |
| `E_REG` | 实际位移超过 `max_shift` | 帧号、实际位移 |
| `E_PARA` | `block_size=2` 越界 | 字段名、允许范围 |
| `E_STATE` | 从 `pending` 直接迁移到 `committed` | 当前/目标状态，原状态保持 |
| `E_DIVERGE` | 人为修改第 2 帧一个像素 | 帧号、y、x、字段名和偏差度量 |
| `E_BUDGET` | 时间/内存预算下无可行模式 | 预算依据 |

## 关键设计

- 配准采用幅度加权相位相关：在纯相位相关与互相关之间折中，抑制噪声高频，再用局部 DFT 上采样和抛物线细化得到亚像素峰。
- 噪声估计使用分块一阶差分 MAD，并取块估计的低分位数避开特征边缘；去噪强度随每帧噪声自适应。
- 支持 `gaussian`、`median`、`guided` 三种滤波器。
- 预算决策只依赖帧数、像素数、配置参数和声明预算，不读取墙钟，因此实时与离线路径的决策序列完全一致。
- 每帧状态为 `pending → registered → denoising → committed`，任一未提交状态可进入 `failed`，失败帧可显式重试。
- 检查点每帧一个原子 NPZ，记录偏移、去噪数组与摘要、源内容哈希、参数版本和算法版本；提交由单写锁和协调器保证帧序稳定。
- 重启时任一元数据、内容哈希、摘要、参数版本或算法版本不匹配，整个快照被隔离并全量重算，禁止拼接旧帧。
- 全量、增量、恢复和并行路径共享同一个纯帧处理函数；并行只改变调度，不改变像素计算。
- `LiveIncrementalSession` 支持在提交边界后调整未提交帧参数；已提交帧保持不变，而混合参数版本的快照在重启时按规则整体作废。

更完整的算法与不变量说明见 `docs/TECHNICAL.md`。

## 目录

```text
sciseq/
  generator.py     确定性样本生成
  loader.py        严格元数据与内容校验
  registration.py  亚像素相位相关
  denoising.py     块噪声估计与自适应滤波
  features.py      连通域特征提取与帧统计
  planner.py       可解释预算模式选择
  state.py         五态帧状态机
  checkpoint.py    原子串行快照
  engine.py        全量/增量/恢复/并行/预览/Live 会话
  compare.py       逐像素差异定位
  export.py        预览偏差闸门与高精度导出
  acceptance.py    本地端到端验收
  cli.py           命令行
  gui/             PySide6 工作台
tests/             26 项自动化测试
docs/TECHNICAL.md  技术说明
samples/           三组本地确定性样本
artifacts/         验收报告、示例快照与预览导出
```

## 测试状态

```text
26 passed
```

覆盖生成器确定性、加载异常、配准精度与精度单调性、非零参考帧、双滤波器、质量阈值、状态非法迁移与冻结、快照失效、活动参数更新、三路径/并行一致、首个差异像素定位、预算预览与 GUI 冒烟。


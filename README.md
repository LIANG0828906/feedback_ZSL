# 可微渲染与逆向成像组件

这是一个从零构建的本地确定性示例，面向游戏与互动媒体美术资产的逆向渲染。项目包含：

- 参数化球体、无限平面、矩形面光源和相机场景格式。
- 基于 PyTorch CPU 的可微光线投射、最近交点、法线、直接光照和软阴影近似。
- 精确、降采样、屏幕分块三种渲染/优化模式及可解释的自动选择规则。
- 梯度范数裁剪、非有限梯度检测、逐参数连续超阈值冻结。
- 固定块大小、受限并行缓冲、严格块序提交、参数快照锁定和失败重试。
- 带内容哈希、数据集指纹、算法版本、参数版本和裁剪配置的原子检查点。
- 实时、断点恢复、一次性全量三条路径的逐元素一致性验证。
- 光度损失、梯度曲线、块耗时、目标/重建对照和差异热力图导出。
- `E_SCHEMA`、`E_PARA`、`E_GRAD`、`E_STATE`、`E_DECODE`、`E_DIFF`、`E_CHECKPOINT`、`E_BLOCK` 等结构化错误码。

所有样本、目标图像和检查点都在本地生成，不访问网络服务。

## 环境

已验证环境：

- Python 3.11.18
- torch 2.15.0+cpu
- numpy 2.4.6
- matplotlib 3.11.2
- pytest 9.2.0

Linux/macOS：

```bash
uv venv --python 3.11 .venv
uv pip install --python .venv/bin/python \
  --index-url https://download.pytorch.org/whl/cpu torch
uv pip install --python .venv/bin/python numpy matplotlib pytest
```

也可以直接使用 `requirements.txt`：

```bash
python -m venv .venv
source .venv/bin/activate
python -m pip install --index-url https://download.pytorch.org/whl/cpu torch
python -m pip install -r requirements.txt
```

Windows 标准控制台：

```powershell
py -3.11 -m venv .venv
.venv\Scripts\activate
python -m pip install --index-url https://download.pytorch.org/whl/cpu torch
python -m pip install -r requirements.txt
python -m diffrender --help
```

`torch` 使用 CPU 即可。所有确定性验证仅依赖 CPU。

## 快速运行

首次运行先生成数据集：

```bash
python -m diffrender generate --dataset data --seed 20261123
```

生成物：

```text
data/
  manifest.json
  single/
    scene.json
    targets.json
    images/*.npy
  multi/
    scene.json
    targets.json
    images/*.npy
```

单次优化：

```bash
python -m diffrender run \
  --dataset data \
  --group single \
  --out artifacts/run_single \
  --iterations 20 \
  --learning-rate 0.012 \
  --mode exact
```

输出包括：

- `artifacts/run_single/run_report.json`
- `artifacts/run_single/checkpoints/checkpoint.json`
- `artifacts/run_single/diagnostics/comparison.png`
- `artifacts/run_single/diagnostics/difference_*.png`
- `artifacts/run_single/diagnostics/loss_curve.png`
- `artifacts/run_single/diagnostics/gradient_curve.png`
- `artifacts/run_single/diagnostics/block_times.png`
- `artifacts/run_single/diagnostics/parameter_table.png`
- `artifacts/run_single/diagnostics/dashboard.html`
- `artifacts/run_single/diagnostics/parameter_errors.{json,csv}`

断点恢复：

```bash
python -m diffrender run \
  --dataset data --group single --out artifacts/run_resume \
  --resume --iterations 20
```

三路径一致性验收：

```bash
python -m diffrender verify \
  --dataset data --group single --out artifacts/verify_single \
  --iterations 20 --checkpoint-every 2
```

完整验收演示：

```bash
python -m diffrender demo \
  --dataset data --out artifacts/demo --iterations 20
```

该命令会演示单物体和多物体重建、三条路径一致性、降采样误差单调性、梯度失效冻结与恢复、块异常重试、永久块异常 `E_BLOCK`，以及 `E_SCHEMA`/`E_DECODE`/`E_PARA`/`E_STATE` 错误链路。结果写入 `artifacts/demo/demo_summary.json`。

## 目录

```text
diffrender/
  schema.py          严格场景/目标/真值校验
  generator.py       固定种子样本生成
  renderer.py        可微光线投射和软阴影
  params.py          参数布局与物理变换
  strategy.py        精确/降采样/分块取舍
  blocks.py          块账本、受限调度、提交边界
  gradient.py        裁剪和梯度失效防护
  checkpoint.py      原子持久化和版本识别
  optimize.py        逆渲染优化状态机
  paths.py           三条执行路径
  compare.py         首个差异定位和 E_DIFF
  diagnostics.py     曲线、热力图和参数对照
  mode_compare.py    精确/降采样前向差异报告
  cli.py             命令行与整数退出码
tests/               自动测试
docs/DESIGN.md       设计和技术说明
docs/VALIDATION.md   实际验证记录
```

## 约束与说明

- 场景加载不填充默认值；缺失或越界字段返回 `E_SCHEMA` 和精确 JSON 路径。
- 目标图像使用 float64 `.npy`，避免 8 位 PNG 量化破坏解析真值。损坏文件返回 `E_DECODE`。
- 精确前向与生成目标在同一解析模型下逐元素一致。
- 可选模式为：
  - `exact`：整图逐像素精确求交。
  - `downsample`：低分辨率渲染后双线性上采样；报告 MAE/RMSE 随倍率非递减。
  - `tiled`：固定屏幕块，块缓冲和并行数受内存预算限制，按块序提交。
- 分块模式中每块使用当前轮参数快照和固定阴影种子；非法块序或漂移快照返回 `E_STATE`。
- 梯度失效参数被冻结，优化器步进后会恢复其步进前数值，避免 Adam 动量继续隐式更新；其他参数正常更新并返回 `E_GRAD`。
- 检查点写入使用文件锁、同目录临时文件、`fsync` 和原子替换。内容哈希或任一版本不匹配时整体作废，从初始场景全量重算。
- 强制终止不承诺完整报告；已持久化的上一窗口检查点仍可校验后恢复。
- 漫反射 albedo 与光源强度存在乘性退化，环境光项提供有限锚定。因此优化目标是降低光度损失并报告逐参数误差，而不是宣称无条件唯一恢复物理真值。

## 退出码

| 业务码 | 整数退出码 | 含义 |
|---|---:|---|
| `E_SCHEMA` | 2 | 场景、目标清单或真值结构/范围错误 |
| `E_PARA` | 3 | 渲染/优化参数越界 |
| `E_GRAD` | 4 | 梯度失效并冻结目标参数 |
| `E_STATE` | 5 | 非法块序、漂移快照或非法状态转换 |
| `E_DECODE` | 6 | 目标图像缺失/损坏/尺寸不符 |
| `E_DIFF` | 7 | 三路径或报告比对不一致 |
| `E_CHECKPOINT` | 8 | 检查点内容结构错误 |
| `E_BLOCK` | 9 | 块前向/反向内部异常且重试耗尽 |
| `E_IO` | 10 | 本地文件 I/O 错误 |
| `E_INTERNAL` | 11 | 未预期内部错误或演示验收失败 |

错误信息以 JSON 写入 `stderr`，并同时携带 `code`、`message`、`field_path` 和 `exit_code`。

## 已知取舍

- 软阴影使用确定性低差异矩形光源采样和光滑遮挡，而非蒙特卡洛全局光照。
- 分块模式在并行数大于 1 时仍按固定块序提交和累加梯度，因此结果不依赖线程完成顺序。
- 降采样模式主动损失高频细节；默认以聚合 MAE/RMSE 作为“差异”验收指标。单像素 `max_abs` 可因高动态范围边缘波动，报告中单独保留。
- 优化使用余弦学习率日程。裁剪开关、阈值、耐心轮数和日程都纳入配置指纹，因此开启/关闭裁剪的轨迹可分别复现。


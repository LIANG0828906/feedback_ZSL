# NumPy Deterministic Volume Renderer

桌面端可直接集成的三维标量体渲染组件与 PyQt5 示例应用。渲染、归一化、
传递函数、光照、前向 alpha 合成和误差传播全部在 NumPy 中完成；界面只负责
交互、显示和把核心结果转换为 `QImage`。

项目包含：

- 72 字节自定义体数据头 + 原始体素读写，带 payload/header CRC32。
- 64×64×64 确定性密度体与四类分割掩码样本，记录 SHA-256 和生成参数。
- 分段线性不透明度/颜色传递函数，浮点与 12-bit 定点 LUT。
- 固定步长光线投射、中心差分梯度、归一化法向、共享浮点合成路径。
- 每像素累积误差估计、可信度阈值、收敛状态和误差热力图。
- 内容哈希、版本哈希、完整快照保存/恢复和 `E_SNAPSHOT_STALE` 检测。
- 单飞渲染会话（`E_BUSY`）、异常时离屏缓冲释放和最后成功帧保留。

## Quick Start

Python 3.10+。本机验证时 Python 3.14 暂无可用 PyQt5 wheel，因此项目虚拟环境
使用 Python 3.13。

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -r requirements.txt

python scripts/generate_samples.py
python scripts/build_cases.py
python -m volrender.render_demo --mask samples/segmentation_64.vol
python -m volrender.app
```

无窗口测试：

```bash
QT_QPA_PLATFORM=offscreen python -m unittest discover -s tests -v
```

验收复现：

```bash
python scripts/verify_acceptance.py
python scripts/platform_smoke.py
```

Windows PowerShell 对应命令：

```powershell
py -3.11 -m pip install numpy PyQt5
py -3.11 scripts\platform_smoke.py
py -3.11 -m unittest discover -s tests -v
```

## Project Layout

| Path | Purpose |
| --- | --- |
| `volrender/format.py` | 自定义头、CRC、体素文件读写 |
| `volrender/volume.py` | 归一化、窗宽窗位、间距重映射与映射差异 |
| `volrender/transfer.py` | 控制点校验、浮点/定点 LUT、区间误差上界 |
| `volrender/render.py` | 光线投射、光照、alpha 合成、误差传播、快照 API |
| `volrender/snapshot.py` | 快照编码、哈希/版本校验、事务式恢复 |
| `volrender/session.py` | 单任务并发约束和错误恢复 |
| `volrender/app.py` | PyQt5 示例应用 |
| `volrender/render_demo.py` | 脚本化、无 GUI 渲染入口 |
| `samples/` | 64³ 密度体、分割掩码、manifest |
| `cases/` | 视角/TF/精度/损坏与过期快照用例 |
| `docs/` | 格式、数值语义、验收记录 |

## Component API

```python
from volrender import (
    RenderSession,
    TransferFunction,
    VolumeData,
    VolumeRenderer,
    VolRenderError,
)

volume = VolumeData.from_file("samples/density_64.vol")
tf = TransferFunction([
    (0.00, 0.00, 0.0, 0.0, 0.0),
    (0.45, 0.18, 1.0, 0.7, 0.2),
    (1.00, 0.94, 1.0, 1.0, 1.0),
])
renderer = VolumeRenderer(
    volume=volume,
    transfer=tf,
    mode="float",
    step=1.0,
    image_size=(96, 96),
    error_threshold=1.0e-3,
)
session = RenderSession(renderer)
result = session.submit_render()

print(result.content_hash)
print(result.error_summary.as_dict())
print(result.untrusted.sum())       # 不可信像素
print(result.error_map)             # 与显示管线同源的误差图
```

编辑失败不会替换旧 LUT：

```python
try:
    renderer.set_transfer_points(invalid_points)
except VolRenderError as exc:
    assert exc.code == "E_TF_INVALID"
    # renderer.transfer / renderer.lut 仍是上一版
```

精度切换会同时渲染两条路径并返回像素差和误差传播对比：

```python
report = renderer.set_precision("fixed")
print(report.comparison.as_dict())
```

## GUI Controls

- 拖动/释放 Width、Level 滑块时重建窗映射，并在状态栏显示映射版本和
  `max|delta|`。
- TF 表格可增删点并编辑 `x/opacity/R/G/B`；非法编辑显示
  `E_TF_INVALID`，表格恢复为当前仍生效的旧控制点。
- 精度下拉框切换到 float/fixed 时自动双路径渲染，并显示像素差和最大误差。
- `Show error heat map` 直接显示核心 `error_map / threshold` 的显示映射。
- `Compare Precision` 与 `Audit Lighting Drift` 可单独重放检查。
- `Save/Load Snapshot` 使用原子写入、整体校验和恢复，失败时保留最后成功帧。

## Business Error Codes

| Code | Meaning |
| --- | --- |
| `E_TF_INVALID` | 控制点非单调、重复、越界或非数值；旧 LUT 继续生效 |
| `E_MODE_INVALID` | 精度模式非法或渲染器未初始化 |
| `E_LIGHT_DRIFT` | 步长细化后的逐像素光照偏差超过声明容差 |
| `E_SNAPSHOT_INVALID` | 快照损坏、字段缺失、内容哈希/完整性哈希不符 |
| `E_SNAPSHOT_STALE` | 视角、步长、LUT、模式或映射版本与状态哈希不一致 |
| `E_BUSY` | 已有渲染任务在执行 |

## Verified Platforms

| Platform | Python | NumPy | PyQt5 / Qt | Window | QImage |
| --- | --- | --- | --- | --- | --- |
| Windows 10 (10.0.26300) | 3.11.9 | 1.26.4 | 5.15.11 / 5.15.2 | created | constructed |
| WSL2 Ubuntu 26.04 | 3.13.18 | 2.5.5 | 5.15.11 / 5.15.14 | created | constructed |

同一 48×48 视角下，两端浮点内容哈希均为
`d0bb1f6a1b74ce5ff671fc0880609cd044402bcaecf5b7730c315edd1bbd024a`，
定点内容哈希均为
`d11724dd8f737260bec1268c9a4beb0a05dffeebe13fff3b2e2e58babb72cf97`。
详见 [docs/VERIFICATION.md](docs/VERIFICATION.md)。

## Numerical Contract

- 归一化使用加载时记录的原始标量 `min/max`，窗口参数以原始标量单位解释。
- 所有采样、LUT 插值、梯度和合成计算使用 `float64`；仅最终显示缓冲量化到
  8-bit RGBA。
- 浮点/定点模式只改变 LUT 值量化，不改变控制点、采样位置或合成语义。
- 误差图使用 LUT 量化误差、透射率、有效 alpha、颜色量化和采样权重传播得到。
- 超过阈值的像素最终 RGBA 置零，不被当作可信颜色显示。
- 内容哈希覆盖显示 RGBA、浮点误差图和不可信掩码。

完整公式见 [docs/TECHNICAL.md](docs/TECHNICAL.md)。

## Format and Fixtures

自定义头格式见 [docs/FORMAT.md](docs/FORMAT.md)。

样本哈希：

- `samples/density_64.vol`:
  `5501875c09bc4482b2d748519e656ee369324f70ea39f2a0364ee404967fb37b`
- `samples/segmentation_64.vol`:
  `dd130bf0ae9735636de043573723ddec9db075927b10b8341638f137aecce340`

`cases/cases.json` 包含合法/非法传递函数、两种精度模式、两个脚本化视角；
`cases/snapshots/` 包含两组合法快照及截断、内容篡改、字段缺失、视角/步长
版本过期快照。

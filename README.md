# HDR Atlas Studio

面向游戏资产流水线的本地确定性 HDR 图集转换工具。项目实现自定义 RGBA16F
图集格式、Rec.709 / Display-P3 / Rec.2020 色彩转换、线性 / sRGB / PQ
传递函数、分区曝光与色调映射、增量重算、不可变版本链、断电恢复、错误报告、
预览界面，以及可复现的样本生成器。

## 快速开始

```bash
python -m venv .venv
source .venv/bin/activate             # Windows: .venv\Scripts\activate
pip install -r requirements.txt

python -m atlas_pipeline opengl-check
python -m atlas_pipeline generate --suite artifacts/demo-suite

python -m atlas_pipeline generate \
  --output sample.atlas --width 64 --height 48 \
  --bit-depth 10 --color-space rec709 --transfer srgb \
  --grid-cols 4 --grid-rows 3 --seed 7

python -m atlas_pipeline convert \
  --atlas sample.atlas \
  --target-color-space display-p3 --target-transfer linear \
  --target-peak 1000 \
  --exposure 1=0.5 --curve 1=reinhard \
  --snapshot-out state.json \
  --export-atlas exported.atlas --report report.json --trace trace.json

python -m atlas_pipeline preview \
  --snapshot state.json --output preview.png --scale 4 --renderer auto

python -m atlas_pipeline gui sample.atlas --renderer auto
```

`gui` 启动仅监听本机的图形预览界面。界面提供区块选择、曝光 / 色调曲线编辑、
共享转换链编辑、误差热力、版本时间轴、提交、取消、回退和导出。浏览器前端
不依赖第三方 JavaScript；预览 PNG 由 NumPy 确定性渲染，存在 OpenGL
上下文时可用 PyOpenGL/EGL 纹理管线渲染。

## 验收与测试

```bash
python -m pytest -q
python scripts/run_acceptance.py
```

验收产物在 `artifacts/acceptance/`，包括三组不同分辨率/位深/传递函数/网格的
样本、分区增量版本、中断快照、回退版本、导出图集、误差报告、CPU/OpenGL
预览以及 `acceptance-report.json`。实际结果见
`docs/VALIDATION.md`。

## 核心语义

- 图集头固定 48 字节小端格式，载荷为 RGBA16F。解析时校验魔术字、头 CRC、
  位深、色彩空间码、传递函数码、存储格式、网格整除关系、载荷长度和载荷 CRC。
- 源通道先按头部传递函数线性化。色彩链顺序为：线性化、源 RGB→XYZ、
  Bradford 白点适应、XYZ→目标 RGB、曝光、峰值归一化、色调映射、目标传递函数
  编码、位深量化。
- 基色矩阵与白点适应由全局配置决定。修改目标色彩空间、传递函数、峰值亮度、
  白点或适应方法会使全部区块失效；只修改区块曝光/色调映射时仅重算对应区块。
- 每次提交先构建私有事务缓存。只有全部受影响区块成功后才在发布锁内一次性
  替换版本、区块缓存和当前版本指针。预览读取同一把锁，只能看到完整旧状态或
  完整新状态。
- 版本号单调递增，永不覆盖历史。回退会发布一个新版本，内容取自历史版本，
  后续提交以该回退版本为父版本。
- 快照包含嵌入的原始图集、完整版本链、区块哈希、区块载荷、预览版本和可选
  未完成事务；写入使用同目录临时文件、`fsync` 与 `os.replace`。
- GUI 与设置 `--snapshot-out` 的 CLI 启用自动检查点，在事务开始、每个区块完成
  和发布后原子更新快照；取消/失败会清除 pending 事务。
- 导出前可携带期望版本、内容哈希和区块哈希清单。导出文件头写入版本号，
  报告记录区块参数、误差、哈希与链路摘要；`verify-export` 重新载入半浮点载荷
  并复算每个区块哈希和整体内容哈希。

## 错误码

| 错误码 | 用途 |
| --- | --- |
| `E_ATLAS_INVALID` | 头部、字段、网格、长度、CRC 或未知码非法 |
| `E_PARAM_CONFLICT` | 曝光/TM/峰值/参数表冲突，附带 `tile` 与 `field` |
| `E_BUSY` | 同一图集已有转换任务 |
| `E_CANCELLED` | 事务在发布前被取消 |
| `E_VERSION_STALE` | 提交/导出基于过期版本或哈希清单 |
| `E_RECOVERY_INVALID` | 快照字段缺失、哈希链断裂或缓存损坏 |
| `E_EXPORT_INVALID` | 导出文件或报告结构不可读 |
| `E_IO` / `E_USAGE` | 文件系统与命令行/API 使用错误 |

## 目录

```text
atlas_pipeline/
  codec.py          二进制格式读写、头部校验、分区索引
  color.py          色彩矩阵、白点适应、色调映射、逐区块转换
  pipeline.py       事务提交、增量缓存、版本链、预览与回退
  storage.py        原子快照、哈希链与中断恢复
  export.py         图集/报告导出和复载校验
  sample.py         确定性样本与样本套件
  gui.py            本地图形预览 API 与浏览器界面
  opengl_preview.py EGL/WGL 上下文诊断与 PyOpenGL 预览
  render.py         PNG 与误差热力渲染
  cli.py            命令行入口
docs/
  FORMAT.md         二进制格式与校验规则
  ARCHITECTURE.md   事务、版本、缓存、快照与导出设计
  VALIDATION.md     本次实际验证结果
scripts/run_acceptance.py
tests/
```

## 环境说明

- Python 3.11+，NumPy 与 PyOpenGL。
- 无网络时可用本地 wheel：

```bat
python -m pip install --no-index --find-links wheels numpy PyOpenGL
```

- 主 GUI 通过 `127.0.0.1` 提供页面，不需要 Tkinter 或 GLUT。
- OpenGL 诊断在 Linux 使用 EGL；Windows 依次尝试 EGL 和隐藏窗口 WGL。
  OpenGL 不可用时预览自动回退到 NumPy CPU 渲染，转换、版本和导出不受影响。

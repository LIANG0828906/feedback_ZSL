# VP Composite Pipeline

面向虚拟制片实时合成场景的本地桌面数据处理与验证工具。管线把
`vpfc/1`、`vpfc/2`、`vpfc/3` 三种前景合成格式统一为内部线性光模型，按固定
帧窗口执行 alpha 合成、增量失效、持久化检查点、跨版本导出，并提供
PySide6 操作界面。所有样本由固定种子本地生成，不依赖网络素材。

## 已实现能力

- 三版本层描述统一加载：字段命名、可见性表达、层级、alpha 约定、色彩与
  时间单位均不同。
- 线性光逐帧合成：预乘/非预乘输入确定性换算，RGBA8 输出，帧尺寸、通道数、
  帧率严格来自内部模型。
- 四态窗口状态机：`pending → computing → committed/failed`，显式失效才允许
  回退到 `pending`，非法迁移返回 `E_STATE` 且状态不变。
- 精确增量重算：只失效层参数真正影响的窗口；未失效窗口复用已持久化结果。
- 像素级验证：增量/全量与串行/并行结果自动比对，首个差异定位到帧、窗口、
  像素坐标和覆盖层；失败返回 `E_DIFF`。
- 原子检查点：逐窗口串行写入，内容/源版本/算法版本任一不匹配时整体作废，
  支持部分已持久化窗口恢复。
- 跨版本导出：3×3 回读互操作、规范化字节稳定、`E_LOSSY` 降级警告与痕迹。
- 桌面界面：源版本、层栈、窗口状态矩阵、帧预览、统计、热力图、参数编辑、
  时间基准调整、增量/全量重算和逐窗口导出。
- 故障隔离与资源边界：单窗口失败可重试，最大并行度 8，每线程工作帧缓冲
  上限 4，检查点写入串行化。

详细设计见 `docs/TECHNICAL.md`，实测结果见 `docs/TEST_REPORT.md`。

## 目录

```text
vp_pipeline/          核心库、CLI、PySide6 UI
scripts/              确定性样本生成器、一键验收入口
samples/              三版本场景、逐帧指令、解析/像素真值
tests/                unittest 验收套件（含 offscreen UI）
docs/                 技术说明与测试报告
requirements.txt      运行依赖
pyproject.toml        包元数据与 CLI 入口
```

## Windows 安装

要求 Windows Python 3.11 或更高版本，PowerShell 中执行：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install --upgrade pip
.\.venv\Scripts\python.exe -m pip install -r requirements.txt
.\.venv\Scripts\python.exe -m vp_pipeline.cli verify --samples samples
```

如果 PowerShell 阻止激活脚本，可直接使用
`.\.venv\Scripts\python.exe`，无需执行 `Activate.ps1`。若 3.11 不存在，
将 `py -3.11` 换成已安装的 3.11+ 版本选择器。

本机 WSL2 实测环境为 Python 3.14.4、NumPy 2.5.5、PySide6 6.12.0。容器无法
调用 Windows 宿主，因此 Windows 实机执行未在本会话中完成；代码路径使用
跨平台原子替换和 `msvcrt`/`fcntl` 双分支锁。

## 快速开始

重新生成固定种子样本：

```bash
python scripts/generate_samples.py --seed 20261123 --output samples
```

检查单版本加载：

```bash
python -m vp_pipeline.cli inspect --source-version 3 --samples samples
```

运行窗口合成并与全量结果比对：

```bash
python -m vp_pipeline.cli run \
  --source-version 3 \
  --samples samples \
  --checkpoints artifacts/checkpoints \
  --workers 2
```

导出并回读验证：

```bash
python -m vp_pipeline.cli export \
  --source-version 3 \
  --target vpfc/1 \
  --samples samples \
  --output artifacts/exports/v3_to_v1
```

一键执行全部单测和端到端验证：

```bash
python scripts/run_acceptance.py
```

或分别执行：

```bash
python -m unittest discover -s tests -v
python -m vp_pipeline.cli verify --samples samples
```

启动桌面界面：

```bash
python -m vp_pipeline.cli gui \
  --samples samples \
  --checkpoints artifacts/checkpoints \
  --source-version 3
```

无显示器环境下可冒烟运行：

```bash
QT_QPA_PLATFORM=offscreen python -m unittest tests.test_ui -v
```

## 操作界面

- 顶部选择源版本、目标导出格式、并行窗口数和线程数。
- 左侧展示有序层栈、父层、可见性、源版本；选择层后可改 opacity、alpha、
  可见性和线性化颜色。
- 中央显示当前帧和窗口状态矩阵。绿色为已提交、灰色为待合成、橙色为合成中、
  红色为失败；按钮文字标注“已失效”或“复用缓存”。
- 右侧显示已提交窗口进度、逐帧统计、失效/复用清单，以及增量重算、全量重算、
  校验并热力图、场景导出和当前窗口导出按钮。
- 应用编辑只会标记受影响窗口失效，不会静默改写已提交窗口的旧帧与快照。

## 数据与真值

样本包含 5 个可绘制层、18 帧、每窗口 3 帧：

- `samples/scene_vpfc1.json`
- `samples/scene_vpfc2.json`
- `samples/scene_vpfc3.json`
- `samples/frame_instructions.json`
- `samples/truth/expected_frames.json`
- `samples/truth/visibility_endpoints.json`
- `samples/truth/field_mappings.json`

`expected_frames.json` 由生成器中的独立纯 Python 参考合成器产生，不使用生产
渲染器，覆盖每帧 6 个采样像素。三个版本渲染结果必须逐字节一致。

## 错误码

`E_VER`、`E_SCHEMA`、`E_PARA`、`E_STATE`、`E_LOSSY`、`E_DIFF`、`E_DECODE`、
`E_IO`、`E_INTERNAL`。结构化异常通过 `PipelineError.to_dict()` 输出
`code`、`message`、`field_path` 和上下文细节。

## 边界与约定

- 当前合成器要求 4 通道 RGBA 场景；其他通道数返回 `E_SCHEMA`，不做隐式转换。
- 预乘输入必须与 alpha 一致；不一致样本返回 `E_SCHEMA`，不静默修复。
- 重启恢复会逐窗口校验当前模型快照；全部匹配才恢复，任一不匹配则整体作废。
  因而编辑后只有完成全部失效窗口重算，才允许该编辑模型的完整缓存恢复。
- 导出目录固定写入 `scene.json`、`frames.bin`、`frames_meta.json`、
  `export_report.json`，不写入时间戳。

# 多通道声学测量与校准工作台

面向游戏与互动媒体音频素材的本地桌面工作台。项目从确定性样本生成开始，完成多通道 IR 分析、逐帧声学参数重建、干声卷积渲染、可复现检查点、三路一致性验证与异常注入链路。

项目不依赖 Docker、网络服务、真实声卡或外部音频数据。默认样本由固定随机种子生成，写入 `samples/`。

## 交付内容

- `acoustic_workbench/`：分析、渲染、状态机、SQLite 检查点、并行管线、验证、PyQt5 桌面界面。
- `samples/`：31 段确定性四通道 IR、对应干声、逐通道解析真值与四类异常注入用例。
- `tests/`：20 项自动化测试，覆盖接口、状态、恢复、真值精度、长短样本一致性和 UI 启动。
- `artifacts/verification_report.json`：一键验收报告。
- `artifacts/ui_smoke.png`：实际渲染的桌面窗口截图。
- `docs/DESIGN.md`：算法、状态机、检查点和并发设计说明。
- `docs/VERIFICATION.md`：实际验证环境、命令、输出与结论。

## 环境准备

要求 Python 3.10 或 3.11。

Windows PowerShell：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
python -m acoustic_workbench.cli generate --force
python -m acoustic_workbench.app
```

Linux / WSL：

```bash
python3.11 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
python -m acoustic_workbench.cli generate --force
python -m acoustic_workbench.app
```

固定版本为 NumPy 1.26.4、SciPy 1.11.4、PyQt5 5.15.10、pyqtgraph 0.13.3、pytest 8.0.2。

无显示环境下可用 offscreen 冒烟测试：

```bash
QT_QPA_PLATFORM=offscreen python scripts/check_ui.py
```

## 一键验收

```bash
python -m acoustic_workbench.cli verify --parallel 2 --buffer-mb 16
```

该命令执行：

1. 实时逐帧处理。
2. 中断后从检查点恢复处理。
3. 基于已持久化参数的全量复算。
4. 三条路径逐样本比对。
5. 四类异常注入。
6. 短、中、长样本的分辨率取舍与预览/校准容差检查。

完整结果写入 `artifacts/verification_report.json`。

## 桌面功能

- 逐帧重建参数表：通道直达延迟、早期反射能量、DRR、方向与置信度。
- 逐通道延迟曲线、早期能量曲线、DRR 曲线。
- 声场方向示意与逐帧切换。
- 原 IR、干声和渲染输出归一化波形对比。
- 实时处理、断点恢复、全量复算、三路验证和异常注入按钮。
- 取消任务时只保留已持久化提交帧，不承诺完整报告。
- WAV 试听与导出，逐帧 CSV 与 JSON 报告导出。
- 最大并行度、每线程缓冲上限、卷积窗长和渲染块大小可配置。
- 在已有检查点上修改估计窗等分析参数后使用“断点恢复处理”，新参数只作用于未提交帧；已提交帧保持冻结。帧长、帧移和渲染配置在首次提交后冻结并返回 `E_STATE`。

Windows 会自动使用系统微软雅黑字体。WSL 下若存在 `/mnt/c/Windows/Fonts/msyh.ttc` 也会自动加载；也可通过 `ACOUSTIC_WORKBENCH_FONT` 指定字体文件。

## 错误码

| 错误码 | 含义 | 定位信息 |
|---|---|---|
| `E_META` | 采样率、声道布局或样本元数据缺失/非法 | `field`、样本 ID |
| `E_PARA` | 帧长、帧移、估计窗、卷积窗、块大小、并行度或缓冲上限越界 | 具体 `field` |
| `E_TOL` | 预览与校准差异超过声明容差 | 首个超差帧、校准对应帧、三项差值 |
| `E_STATE` | 非法状态迁移、修改已冻结参数或从不完整快照复算 | 当前状态、请求状态、帧号 |
| `E_DIFF` | 三路输出或统计首个样本/帧不一致 | 通道、样本位置、帧号、块号 |
| `E_CHAN` | 通道缺失或声道映射非法 | 缺失通道或映射字段 |
| `E_ORDER` | 通道时序倒置 | 布局与时序字段 |
| `E_CLIP` | 输入超过数字满幅或存在非有限值 | 峰值与门限 |
| `E_FRAME` | 单帧坏帧 | 帧号、通道、失败上下文 |

所有错误均可通过 `WorkbenchError.to_dict()` 获得结构化链路 `chain`，UI 与 CLI 会逐层显示。

## 复现性

- 样本生成器不用时间戳或未固定随机源；相同种子重复生成得到相同清单与数组字节。
- 检查点采用 SQLite 事务，逐帧串行提交并保存载荷摘要、输入内容哈希与算法版本。
- 输入哈希只绑定输入内容和布局；估计窗等运行参数可作用于未提交帧，已提交帧保持冻结。
- 帧网格、渲染配置或算法版本不兼容时不会拼接旧快照，只允许整体作废后重算。

详细设计见 `docs/DESIGN.md`，实测结果见 `docs/VERIFICATION.md`。

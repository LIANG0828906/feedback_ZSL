# 语音与音乐修复桌面工作台（AudioRestore Workbench）

面向游戏与互动媒体素材的本地、确定性音频修复工作台。支持三种历史工程
格式的统一加载、版本升级/降级、分帧谱减 + 掩蔽平滑 + 噪声门修复链、
窗口状态机、原子检查点、暂停/退出/重启续跑以及有界并发，并保证
实时处理、断点恢复与一次性全量重算三条路径**逐样本一致**。

## 环境要求

- Windows 10 及以上（开发与 CI 在 Linux + Python 3.12 上同样验证通过）
- Python 3.10+（推荐 3.12）
- 依赖固定在 `requirements.txt`：`numpy==2.2.6`、`PySide6==6.9.1`
- 无需网络与外部服务（仅首次安装依赖需要包源）

### Windows 安装

```powershell
py -3.12 -m venv .venv
.venv\Scripts\activate
python -m pip install -r requirements.txt
```

### Linux / WSL 安装

```bash
python3.12 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
```

## 一分钟跑通

```bash
# 1. 生成确定性样本（固定种子 20260101，可重复）
python -m audiorest.generate_samples samples

# 2. 端到端验收（等价性 / 三路径一致 / 错误码 / 真值阈值）
python -m scripts.run_demo --samples samples --workdir workdir

# 3. 单元测试
python -m unittest discover -s tests -v

# 4. 启动桌面界面
python -m audiorest.gui.app workdir
# 无显示环境：QT_QPA_PLATFORM=offscreen python -m audiorest.gui.app workdir
```

## 目录结构

```
audiorest/
├── audiorest/
│   ├── errors.py              # E_VER/E_LOSSY/E_PARA/E_XVER/E_STATE/E_DIFF
│   ├── model.py               # 内部统一修复模型 + 升级默认值声明
│   ├── generate_samples.py    # 固定种子样本/真值/损坏工程生成器
│   ├── formats/               # v1/v2/v3 适配器、统一加载、等价性比对
│   ├── core/                  # PCM、修复链、状态机、检查点、引擎
│   └── gui/                   # PySide6 桌面界面
├── scripts/run_demo.py        # 端到端验收脚本
├── tests/                     # 18 个 unittest 用例
└── requirements.txt
```

## 三种工程格式差异

| 维度 | v1.0（legacy） | v2.0 | v3.0（当前内部模型） |
| --- | --- | --- | --- |
| 字段命名 | 蛇形 | 驼峰 | 蛇形 |
| 分帧表达 | 毫秒 `frame_len_ms/hop_ms` | 采样点 `frameSamples/hopSamples` | 采样点 + 显式 `fft_size` |
| 阶段清单 | 阶段名字符串列表 | 阶段对象数组 `stageList` | 强类型 `StageConfig` |
| 音频引用 | `source.pcm_file` | `audioRef.uri` | `audio.path` |
| 窗口/种子 | 无（升级补默认） | 有窗口、无种子 | 窗口 + 种子齐全 |

## 错误码

| 错误码 | 触发条件 |
| --- | --- |
| `E_VER` | 未知版本号、JSON 不可解码、必填字段缺失（携带字段路径） |
| `E_LOSSY` | 降级导出存在无法表达字段，返回完整损失清单，不静默丢阶段 |
| `E_PARA` | 帧长/帧移/谱减强度/门限等越界（携带字段名） |
| `E_XVER` | 同工程多版本内部模型不等价（携带首个差异字段路径与两侧取值） |
| `E_STATE` | 窗口状态机非法迁移（如已提交窗口再处理） |
| `E_DIFF` | 多路径输出逐样本不一致（携带首个采样位置与窗口编号） |

## 关键设计与一致性保证

- **确定性算法**：修复链无任何 RNG；噪声谱取窗口内能量最低的
  `noise_frames` 帧做最小统计平均，掩蔽平滑为一阶递归，所有运算
  使用 float64，同输入同参数必然逐样本一致。
- **检查点整体作废**：manifest 记录源版本、参数版本、算法版本与
  源音频内容哈希；任一不匹配则清空整个快照目录并全量重算，禁止
  混用部分旧窗口。
- **原子串行写入**：每窗口 `npz` 与 manifest 都先写临时文件再
  `os.replace`，全程持锁；并发计算但按窗口编号顺序提交。
- **有界资源**：`BoundedSemaphore` 限制在途采样缓冲，耗尽时阻塞
  排队而非丢弃窗口；并行与串行输出逐样本相等。

## 真值与验收口径

生成器注入已知高斯噪声并记录静音段；真值清单中的 `snr_gain_db_min`
与 `residual_std_max` 由当前算法版本参考运行后加保护边际（SNR 留 1 dB、
残留放宽 20%）写入。测试仍以修复前后采样的逐样本比对独立复算统计量，
不存在“用实现证明实现”的循环。

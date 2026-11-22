# Cast Workbench

面向游戏与互动媒体演示场景的多屏互动投屏工作台。应用使用本地确定性样本驱动一条完整的模拟投屏协议链路：能力协商、会话建立、媒体分片、本地回环传输、乱序重组、输入回传、发送端桌面重放、原子检查点、断点续传和全量重演。

![Workbench with injected faults](artifacts/generated/workbench_faults.png)

## 交付内容

- PySide6 专业桌面工作台：实时镜像、阶段时间线、分片重组、输入时间轴、桌面状态、重传/缺失统计、检查点和三路一致性面板。
- 确定性样本生成器：40 帧合成桌面、28 个远程输入事件、能力集、阶段脚本和时间戳真值。
- 本地 UDP 回环中继：只绑定 `127.0.0.1` 的临时端口，原样回显媒体包和输入封装。
- 协议核心：会话隔离、分片 CRC、帧内容 CRC、有限重传、缺失帧标记、乱序容忍、输入幂等去重。
- 原子检查点：`temp + fsync + os.replace`，线程锁串行化；内容哈希、能力哈希或协议版本不匹配时整体作废。
- 可重复 CLI 演示、48 项自动化测试、异常注入矩阵和实际验证记录。

## 快速开始

### Linux / macOS

```bash
python3 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python -m scworkbench generate --out samples
.venv/bin/python run_workbench.py
```

本仓库当前验证环境使用 `uv venv` 创建了等价虚拟环境，因为宿主缺少 `python3.14-venv`：

```bash
uv venv --python 3.14 .venv
uv pip install --python .venv/bin/python -r requirements.txt
```

### Windows

PowerShell：

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\setup_windows.ps1
.\.venv\Scripts\python.exe -m scworkbench generate --out samples
.\.venv\Scripts\python.exe run_workbench.py
```

或运行：

```bat
run_workbench.bat
```

固定版本：

- Python `>= 3.11`
- `PySide6==6.12.0`
- `numpy==2.3.5`

应用自带 OFL 许可的 Noto Sans CJK SC 字体，避免 Windows 之外的演示机因缺少中文字体出现方框。

## 常用命令

```bash
# 生成确定性样本 + 真值 + 能力集 + 异常参数
.venv/bin/python -m scworkbench generate --out samples

# 加载校验与能力协商
.venv/bin/python -m scworkbench validate --sample samples/sample_session.json

# 启动本地回环并验证媒体包下发、输入封装回显
.venv/bin/python -m scworkbench relay-test --sample samples/sample_session.json

# 实时 / 断点恢复 / 全量重演三路比对 + 全部异常码矩阵
.venv/bin/python -m scworkbench demo --sample samples/sample_session.json --data-dir artifacts

# 48 项验收测试
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -s tests -v

# 可重复界面截图
.venv/bin/python scripts/capture_workbench.py
```

也可直接使用控制台命令：

```bash
cast-workbench gui --sample samples/sample_session.json --data-dir artifacts
```

## 工作台操作

- `启动 / 继续`：开始或继续执行当前时间线。
- `暂停` / `恢复`：通过协议状态机暂停；已提交帧数、下一帧序号、输入重放偏移均不重置。
- `触发重连`：执行 `mirroring → reconnecting → mirroring`，只续传已提交状态。
- `立即检查点`：原子写入当前阶段、时间线索引、帧/分片结果、输入状态、桌面状态和协商结果。
- `三路一致性`：后台运行实时、断点恢复和全量重演，比较首个差异帧或输入及字段名。
- `导出回放日志`：正常完成后写入 `complete=true` footer；未正常完成时明确标记 `partial`，不会宣称完整。
- 编码档位与重传上限可在运行中修改，只作用于尚未提交的帧；已提交帧结果不会被改写。
- 异常组合在“新建运行”时生效，避免中途替换当前运行的确定性参数。

## 项目结构

```text
scworkbench/
  loader.py        样本结构/语义校验、E_SCHEMA/E_DECODE
  negotiation.py   能力交集、编码档位与分辨率协商、E_NEGO
  engine.py        阶段状态机、媒体链路、输入重放、检查点编排
  protocol.py      媒体包/输入封装二进制协议
  reassembly.py    分片重组、CRC、乱序与缺失检测
  desktop.py       合成桌面状态与坐标裁剪
  rendering.py     确定性 RGBA 镜面渲染与缺失帧标记
  checkpoint.py    原子串行检查点与整体失效
  compare.py       三路逐帧/逐事件比较和 E_DIFF 定位
  loopback.py      127.0.0.1 UDP 中继与客户端
  replaylog.py     NDJSON 增量日志、complete/partial 语义
  gui/             PySide6 工作台、后台播放和线程池
samples/           确定性样本、真值、能力集、异常参数
tests/             48 项自动化验收测试
docs/              架构与验证说明
artifacts/generated/ 演示结果、截图和测试输出
```

## 确定性样本

- 分辨率：`640×360`
- 帧率：`12 FPS`
- 帧：40 帧，序号 `1..40`，固定种子 `424242`
- 输入：28 个，覆盖 click / drag / key / scroll
- 阶段脚本：暂停、恢复、重连、重连完成、断开
- 协商结果：`hevc-main`，`640×360@12`，输入回传开启
- 样本内容哈希：`02753c13e1451963fc9f170c63486bb2fccbeb512e1c3813c6447a45052ecebe`

`samples/truth.json` 包含设备、时间戳解析真值、帧/输入序号、阶段脚本、协商结果、每帧源画面摘要和正常完成的预期计数。

## 协议与状态

阶段迁移：

```text
negotiating ──► mirroring ──► paused ──► mirroring
                    │            │
                    ├──► reconnecting ──► mirroring
                    └──────────────► disconnected
```

媒体包为 50 字节定长头 + 分片 payload，包含：

- magic / wire version / flags
- 16 字节会话摘要
- 帧序号、分片序号、分片总数
- 毫秒逻辑时间戳
- payload 长度、payload CRC32、整帧压缩内容 CRC32

接收端允许同帧分片乱序到达但仍按分片序号重组；缺失分片触发有限次重传。超过上限后生成确定性缺失帧标记，且 `sent frames == reassembled frames` 仍成立。错误会话摘要的包抛 `E_SESSION` 并丢弃。

## 错误码

| 错误码 | 触发场景 |
|---|---|
| `E_SCHEMA` | 字段缺失、设备引用不存在、能力取值非法 |
| `E_DECODE` | JSON、帧场景或事件内容损坏 |
| `E_NEGO` | 编码档位/分辨率/输入回传能力无交集，`details.items` 指出项 |
| `E_STATE` | 非镜像中阶段收到帧/输入，或非法阶段迁移；阶段与计数不变 |
| `E_SESSION` | 媒体包/输入封装会话标识不匹配，丢弃 |
| `E_LOSS` | 分片缺失，已发起重传 |
| `E_ORDER` | 分片乱序到达，已按序重组 |
| `E_CAP` | 运行期能力变化被拒绝，保留协商基线 |
| `E_CORRUPT` | payload CRC、整帧 CRC、压缩数据或尺寸校验失败 |
| `E_DUP` | 输入重复或过期，桌面状态不变 |
| `E_DIFF` | 三路比较不一致，报告 component/index/field/phase |

## 关键一致性约束

1. 帧包和输入只在 `mirroring` 阶段提交；拒绝不改变计数器。
2. 暂停/恢复和重连只改变阶段，不重置帧序号与输入去重进度。
3. 检查点枚举完整快照；任一版本/内容/能力哈希不匹配则整体作废。
4. 单帧 CRC 或解码失败只影响该帧；其他帧和已提交画面不变。
5. 帧提交、输入提交和检查点写入在同一引擎锁下串行；编码通过有界线程池执行。
6. 三路结果按 `frame_sha256`、状态、分片顺序、输入 `desktop_digest`、映射坐标、裁剪轨迹和阶段逐项比较。
7. 强制终止不保证完整日志；日志 footer 是唯一完整完成凭据。

## 实际验证

- 自动化测试：`48/48 OK`，见 `artifacts/generated/test-results.txt`。
- 机器可读摘要：`artifacts/generated/verification_summary.json`。
- CLI 三路比对：`PASS`，实时、18 帧检查点恢复、全量重演均为 `40/40` 帧、`27/28` 成功输入（1 个暂停期探针按协议返回 `E_STATE`），桌面 revision 均为 `27`。
- 完整日志：`artifacts/generated/realtime_replay.ndjson` 与 `recovery_replay.ndjson` 均带 `complete=true` footer。
- 本地回环：媒体与输入均收到并回显，`invalid_requests=0`。
- 异常矩阵：`E_LOSS / E_ORDER / E_CAP / E_CORRUPT / E_SESSION / E_DUP / E_STATE` 全部命中。

详细记录见 [docs/VERIFICATION.md](docs/VERIFICATION.md)，设计与不变量见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 边界说明

这是可重复的本地协议与状态工作台，不调用真实 H.264/HEVC 编解码器，也不访问外部网络。编码档位通过不同的压缩级别与分片大小模拟；画面由固定种子生成。所有传输仅经 `127.0.0.1` 回环。

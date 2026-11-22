# 多人协作对局实时状态同步内核

这是一个完全本地、确定性的实时同步内核和可重复演示应用。它从固定种子生成对局、输入意向流和三套协议帧，在权威侧逐 tick 推进，在客户端侧预测与回滚，并通过本地回环 TCP 服务验证丢帧、重复帧和乱序帧下的收敛性。

项目不依赖 Docker、线上账号或付费服务。核心模拟/协议/检查点只使用 Python 标准库；NumPy 与 PySide6 仅用于可选分析支持和图形界面。

## 快速开始

目标环境为 Windows 10/11 + Python 3.11 或更高版本。

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
python demo.py
```

Linux / WSL 等价命令：

```bash
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements.txt
python demo.py
```

常用命令：

```bash
python demo.py                      # 生成样本、执行全部验证、输出无界面报告
python demo.py --gui                # 验证完成后打开 PySide6 交互界面
python demo.py --no-generate        # 复用 samples/ 中已有确定性样本
python -m unittest discover -s tests -v
python scripts/generate_samples.py  # 只重新生成本地样本
```

Windows 也可双击或执行 `demo.cmd`。

## 交付内容

- `sync_kernel/protocol.py`：三版本统一解码、跨版本编码、`E_LOSSY` 降级清单与确定性 JSON 字节编码。
- `sync_kernel/models.py`：内部权威状态、固定点字段、统一摘要与逐字段差异定位。
- `sync_kernel/engine.py`：仅依赖输入意向流和 `seed/tick/unit` 派生随机源的 tick 推进器。
- `sync_kernel/client.py`：本地预测、权威回滚、未确认输入重放、序列空洞/倒序保护。
- `sync_kernel/service.py`：127.0.0.1 回环同步服务、三类故障注入、断点补齐和上报回显。
- `sync_kernel/checkpoint.py`：提交窗口检查点、原子替换、严格身份校验和恢复边界。
- `sync_kernel/concurrency.py`：有界工作线程池、队列/内存上限、稳定顺序归并。
- `sync_kernel/ui.py`：单位状态表、协议版本切换、tick 滑动、预测修正曲线。
- `samples/`：固定种子、4 个单位、2 名玩家、61 个 tick 的初始状态/输入流/三版帧/协议描述。
- `tests/`：30 个单元与集成测试。
- `artifacts/demo/report.json`、`report.md`：本次实际运行的机器可读与可读报告。

## 协议版本

| 版本 | 名称 | 时间基准 | 增量方式 | 缺省字段语义 |
|---|---|---|---|---|
| v1 | `legacy-tick-delta` | 服务器 tick 计数 | 只传变化字段 | 缺省表示保留上一值 |
| v2 | `microsecond-delta-with-mask-semantics` | 服务器微秒时间 | 变化字段组 + 8 位内容哈希 | 普通字段保留；缺省 `status` 显式归零 |
| v3 | `logical-clock-snapshot-delta` | 120 Hz 单调逻辑时钟 | 完整快照 + 差量投影 | 快照完整；差量缺省保留快照值 |

所有版本都表达同一组权威核心字段：所属玩家、队伍、定点坐标、朝向、生命、冷却、资源和状态位。v1/v2 无法直接表达逻辑时钟、完整快照哈希和内部时间基准，因此编码结果携带结构化 `E_LOSSY` 警告与 `from_path/to_path/strategy/lossy` 映射痕迹；编码本身仍成功，回读后的权威摘要与 v3 一致。

完整字段和示例见 `docs/PROTOCOL.md`。

## 确定性与检查点

- 坐标单位为 `1/1000` 世界单位，朝向为 `1/1000` 度，生命/资源为整数定点值。
- 所有随机判定由 `SHA-256(seed | tick | unit_id | purpose)` 派生，不依赖进程全局 RNG、线程调度或推进路径。
- 同一输入流重复推进时，每个 tick 的规范 JSON 摘要完全一致。
- 编码使用键排序、紧凑分隔符和 UTF-8，不使用字典插入顺序或运行时日期。
- 检查点每 `commit_window=10` tick 写入一次，包含状态、随机进度、输入消费偏移、协议版本、快照内容哈希和信封哈希。
- 文件先写同目录临时文件并 `fsync`，再以 `os.replace` 原子替换；写入由路径级锁串行化，提交 tick 必须严格递增。
- 加载时种子、输入流指纹、协议版本、窗口大小或任一哈希不匹配，整份检查点作废并要求从确定性起点重算。
- 强制终止后只恢复到最后一个完整持久化窗口；之后的 tick 标记为重算，绝不为已终止进程声称完整报告。`RecoveryResult` 同时支持用户取消并标记 `cancelled=true`、`complete=false`。

## 并发与资源边界

- 默认最多 4 个 worker、16 个排队任务、8 MiB 预留缓冲。
- 演示中三个协议版本分别在 worker 内完成推进、编码和解码，超限返回 `E_RESOURCE`。
- 服务端事件队列有界，客户端未来帧缓冲同时受条数和字节数限制。
- 检查点写锁保证并发场景下 tick 提交顺序稳定。
- 演示实际观测最大排队任务 3、最大预留缓冲 1.5 MiB，均在配置上限内。

## 错误码

| 错误码 | 含义 |
|---|---|
| `E_VER` | 版本未知或必填字段缺失；包含字段路径和源版本 |
| `E_DECODE` | JSON/编码/哈希损坏 |
| `E_SEMANTIC` | 单位引用不存在、时间倒退、快照差量冲突等语义错误 |
| `E_SEQ` | 序列空洞、重复或倒序；已确认状态不被修改 |
| `E_LOSSY` | 低版本编码降级警告，不是失败 |
| `E_RESOURCE` | 队列、内存、帧大小或服务容量超限 |

## 实际验证结果

本次执行环境：WSL2 Linux、CPython 3.14.4、NumPy 2.5.5、PySide6 6.12.0。Python 3.11+ 的代码路径保持兼容，Windows 下目录 `fsync` 不可用时采用安全的原子文件替换边界。

```text
python -m unittest discover -s tests -v
Ran 30 tests ... OK

python demo.py
demo: PASS (10 passed, 0 failed)
final_hash: d16897e62030771afaa912121ed5cd314c53f56cc29252d43ea0f753e4af3dfa
```

- v1/v2/v3 三个客户端在同一 60 tick 对局中最终摘要完全一致。
- 三版本逐帧回读与再编码字节差异为 0，跨线程重复编码字节稳定。
- 回环服务注入每个版本各 1 次丢帧、1 次重复帧、1 次乱序帧，3 个客户端均收敛，首个分歧为 `null`。
- 预测回滚在 tick 12 与权威逐字段一致，共记录 71 条可观测修正。
- 未知版本/缺字段为 `E_VER`，损坏 JSON 为 `E_DECODE`，未知单位/时间倒退/快照冲突为 `E_SEMANTIC`，序列空洞/倒序为 `E_SEQ`。
- 强制终止场景只从持久化的 tick 20 恢复，21–60 明确标记为重算；取消场景不声称完成。

详细记录见 `docs/VALIDATION.md` 和 `artifacts/demo/report.md`。

## 图形界面与无界面降级

`python demo.py` 始终以无界面模式运行并产出报告。`python demo.py --gui` 在 Windows 或有显示服务的桌面上打开 PySide6 界面；没有可用显示时自动使用 Qt offscreen 冒烟模式。界面可切换 v1/v2/v3、拖动 tick、查看单位表和预测修正曲线。


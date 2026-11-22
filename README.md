# 触觉事件调度内核与诊断工作台

这是一个完全本地运行的确定性触觉事件调度系统。输入是游戏逻辑产生的逐帧事件流，输出是逐通道设备指令包；本机回环模拟设备返回接收序号与处理时刻。系统同时提供 PySide6 诊断工作台、SQLite 检查点、断点恢复和仅依据持久化决策的全量重放。

原始需求说明已保存在 `docs/TASK.md`。

## 已实现功能

- 事件流/能力清单 JSON 加载与校验：`E_SEQ`、`E_CHAN`、`E_PARA` 均带事件标识和字段名。
- 确定性逐通道调度：优先级、到达顺序、最小间隔合并、禁止打断保护、打断与丢弃。
- 逐事件决策记录：决策类型、依据事件、目标全局包序号、原因和参数。
- 依赖图与增量失效：只失效目标通道锚点包及其后续包，其余包复用持久化字节。
- 稳定二进制协议：固定头部、包内事件、包络、CRC32；重复编码逐字节一致。
- 本机 TCP 回环模拟设备：可复现丢包、重发、延迟和乱序；支持单包重试。
- 回显对齐：`E_MISS`、`E_DUP`、`E_TIMEOUT`、`E_REORDER` 为服务侧异常，`E_LOCAL` 为本地包序违规。
- 包状态机：`PENDING → ENCODED → DISPATCHED → CONFIRMED/FAILED`，非法迁移返回 `E_STATE` 且不改变状态。
- SQLite 原子检查点：稳定窗口顺序、包字节、决策、消费偏移、内容哈希、能力版本、内核版本和哈希链。
- 三条路径自动比对：实时、断点恢复、仅持久化决策重放；首个差异返回 `E_DIVERGE`。
- 有界并行调度/编码，串行包号分配、下发和检查点写入。

## 环境

开发与验证环境：

- Python 3.11.18
- PySide6 6.6.3.1
- NumPy 1.26.4
- WSL2 Ubuntu 26.04，本机图形会话；界面用 PySide6 `offscreen` 完成无头截图验证
- 无 Docker、无外部服务、无真实触觉设备

离线安装（Linux/WSL，wheelhouse 为本地 wheel 目录）：

```bash
python3.11 -m venv .venv
uv pip install --python .venv/bin/python --offline --no-index \
  --find-links /path/to/wheelhouse -r requirements.txt
```

或使用脚本：

```bash
bash scripts/bootstrap_offline.sh /path/to/wheelhouse
```

Windows PowerShell：

```powershell
.\scripts\bootstrap_offline.ps1 -Wheelhouse C:\wheelhouse
```

## 运行

生成确定性样本：

```bash
.venv/bin/python -m haptics sample --workdir artifacts/acceptance
```

运行三路径验收：

```bash
.venv/bin/python -m haptics verify \
  --workdir artifacts/acceptance \
  --parallel --max-workers 3 \
  --max-events-per-task 7 --max-packets-per-task 3 \
  --json-out artifacts/verification.json
```

也可用 `--events path/to/events.json --capabilities path/to/capabilities.json` 对自定义本地输入执行同样的三路径验证。

启动诊断工作台：

```bash
.venv/bin/python -m haptics gui --port 0 --workdir artifacts/workbench
# 或
.venv/bin/python run.py --port 0 --workdir artifacts/workbench
```

运行测试：

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest discover -s tests -v
```

## 回环端口

默认 `--port 0`，由操作系统分配空闲端口，并在界面状态栏显示实际端口。也可指定固定端口：

```bash
.venv/bin/python -m haptics gui --port 17890 --workdir artifacts/workbench
```

端口被占用时返回 `E_IO`，`field=port`，并带 `host`/`port` 详情。无需外部网络或账号。

## 诊断界面

界面四个区域并排展示：

- 事件流时间轴与逐事件调度决策；
- 各通道包序列、状态、复用/失效标记；
- 指令包十六进制、SHA-256、回显记录与对齐状态；
- 异常分类、单包重试、失效传播和三路径比对。

工具栏支持实时运行、三路径验证、暂停、断点恢复、重启续跑和导出报告。编辑事件优先级或通道能力参数后立即展示增量失效清单，已确认包保留原状态并叠加 `INVALIDATED`/`REUSED`。

截图：`artifacts/workbench.png`

## 目录

```text
haptics/
  errors.py         稳定错误码与 Issue
  models.py         事件、能力、包、决策、回显模型
  validation.py     JSON 加载与 E_SEQ/E_CHAN/E_PARA 校验
  scheduler.py      确定性逐通道调度与有界并行
  codec.py          HTP1 二进制指令包编码/解码
  state.py          包状态机
  dependencies.py   包依赖图与失效传播
  incremental.py    优先级/增删事件/能力参数增量更新
  transport.py      TCP 回环模拟设备与确定性故障注入
  alignment.py      回显对齐与单包重试
  persistence.py    SQLite 原子检查点与哈希链
  pipeline.py       实时/恢复/重放三路径与自动比对
  sample.py         确定性事件、能力与故障样本
  workbench/        PySide6 诊断工作台
tests/              42 项 unittest
docs/               设计、验证记录、原始任务
artifacts/          验收 JSON、日志、截图和检查点
```

## 关键约定

- 包内 `event_count` 是合并后的效果槽数量；每个槽的 `source_count` 表示被合并的原始事件数。
- 合并采用采样点逐点最大值，优先级取最大值，包络持续时间取最晚结束时间。
- `no_interrupt` 在效果生效期内保护事件；低/同/高优先级均不能覆盖受保护事件。
- 模拟设备处理时刻是确定性逻辑时钟；另附 `wall_time_ns` 仅用于人工排障，不参与比对。
- 包序倒置属于内核侧 `E_LOCAL`；服务侧相邻交付次序互换属于 `E_REORDER`。
- 检查点任一内容哈希、能力版本或内核版本不匹配时，整库作废并执行全量重放。

详细设计见 `docs/DESIGN.md`，实际运行结果见 `docs/VERIFICATION.md`。


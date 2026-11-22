# syncdemo — 实时状态同步演示与结果验证工具 技术说明

## 环境

- Python 3.14.4（兼容 3.11+），NumPy 2.5.5，pygame-ce 2.5.8（提供 `pygame`
  API；上游 pygame 暂无 cp314 预编译轮，pygame-ce 为直接替代品），pytest 9.2.0。
- 无 Docker/容器，无外部服务与账号；客户端时延通过内存模拟（回环语义）。
- 安装：`python3 -m pip install -r requirements.txt`

## 模块与需求映射

| 需求 | 实现 |
|---|---|
| 1 样本加载与校验 | `syncdemo/schema.py`（`load_sample` / `validate_sample`） |
| 2 权威确定性推进 | `syncdemo/engine.py` + `syncdemo/rng.py` |
| 3 本地预测与回滚 | `syncdemo/client.py`（`ClientSim`） |
| 4 延迟/一致性取舍 | `syncdemo/frames.py`（`FramePolicy` / `FrameScheduler`） |
| 5 持久化检查点 | `syncdemo/checkpoint.py`（`CheckpointStore`） |
| 6 三路径一致 | `syncdemo/runner.py`（`run_realtime` / `run_resume` / `run_replay` / `compare`） |
| 7 可交互界面 | `syncdemo/ui.py`（`DemoApp`） |
| 8 并发与资源边界 | `syncdemo/workers.py`（`WindowPool`） |

样本生成器：`syncdemo/generator.py`，默认种子 `20261123`、输出目录 `sample/`，
均可通过 CLI 参数覆盖。真值（关键 tick 权威状态、各客户端可见状态、结算）由
引擎与客户端模拟自身产出，保证与验证器同源。

## 错误码

- `E_SCHEMA`：字段缺失、玩家/单位标识重复、属性非有限值、tick 跳号、时延档位为负；
  异常携带 `field` 指出字段名。
- `E_UNIT`：意向流/真值/客户端引用了不存在的单位；异常携带 `unit` 指出标识。
- `E_PARA`：帧策略或线程池参数越界（fps 非正、fps 非 tick 速率整数倍、
  延迟/校正窗口 < 1、并行度或队列 < 1）。
- `E_DIVERGE`：三路径比对发现不一致；异常携带首个不一致的 `tick` / `unit` / `field`。

## 确定性设计

- 全部随机判定由 `derive_rng(seed, tick, purpose)` 产生：SHA-256 派生 128 位子种子
  驱动 PCG64。派生只依赖 `(对局种子, tick, 用途)`，与推进路径、历史消耗无关，
  因此实时、断点恢复、重放三条路径对同一判定得到同一值。
- 引擎按 tick 依次应用意向（移动/技能/采集/道具），技能做前置校验
  （冷却、资源、目标存活），支持死亡与延迟复活；任一玩家资源达到阈值即锁定
  对局并写入结算，锁定后状态不再变化。

## 预测、回滚与帧取舍

- 客户端持有 confirmed（最近权威帧）+ pending（未确认本地输入），可见态为
  confirmed 重放 pending 的预测结果；权威帧到达即回滚到权威态并重放未确认输入，
  修正量按单位×字段绝对差记录（`Correction.magnitude/total`）。
- 时延档位 L 表示 tick t 的权威帧在 t+L 才送达；对局结束后按最大时延做
  drain，所有客户端可见态与权威结算逐字段一致（测试覆盖）。
- 帧调度：输入后 `max_input_delay_frames` 内显示预测态（响应性）；权威校正在
  `correction_window_frames` 内上屏；冲突时一致性优先，帧日志记录
  `conflict:correction_priority(consistency>responsiveness)` 作为取舍依据。

## 检查点与恢复

- 每个提交窗口（默认 30 tick）写入：权威状态、子随机源游标（=tick）、输入消费
  偏移、各客户端已确认帧号、快照内容哈希（SHA-256）、算法版本。
- 写入经单一锁串行化，`tmp` 文件 + `os.replace` 原子落盘；清单与窗口文件分离。
- 加载时对局种子 / 输入流指纹 / 算法版本任一不匹配，或任一记录哈希校验失败，
  则整体作废并全量重算，禁止部分混用。
- 已提交窗口以不同内容重复提交会抛错而非静默改写（相同内容幂等）。
- 取消/强制终止后只能依据已持久化且版本匹配的窗口恢复；取消的运行
  `status="cancelled"`，不产出完整结算报告。

## 并发与资源边界

- `WindowPool` 限制最大并行度、任务队列与结果缓冲上限；队列/缓冲满时
  生产者阻塞排队（`Queue.put` 背压），不静默丢弃。
- 窗口内异常被捕获并标记 `failed`，可重新提交重试，不影响其他窗口；
  `cancel()` 后未执行窗口标记 `cancelled`。
- 运行器将窗口的编码+提交任务投入池中并行执行，提交本身由存储层串行化。

## 界面操作

`python3 -m syncdemo demo --sample sample`

- 左图权威态、右图所选客户端可见态；顶部标注本帧 PREDICTED / AUTHORITATIVE
  及取舍依据；右上修正量曲线与回滚次数。
- `空格` 暂停，`Tab` 切换客户端，`↑/↓` 调整时延档位，`+/-` 速度，
  `←/→` 单步，`i` 注入输入，`r` 重演，`q` 退出。
- 已播放完成的提交窗口被锁定：向已提交 tick 注入输入会被拒绝并提示；
  重演会校验已提交窗口哈希，不一致则拒绝应用。

## 验证结果（2026-11-23 实测）

- `python3 -m pytest tests/ -q`：**41 passed**（覆盖 E_SCHEMA/E_UNIT/E_PARA/
  E_DIVERGE 全部异常码、回滚一致性、三路径一致、检查点作废与不可改写、
  线程池阻塞/失败隔离/取消、UI 无头冒烟与注入拒绝）。
- `python3 -m syncdemo generate --out sample && python3 -m syncdemo verify
  --sample sample`：三路径一致 OK；结算
  `p0 res=57 hp=64 / p1 res=31 hp=84 / p2 res=56 hp=12 / p3 res=93 hp=100`，
  winner=p3；各客户端回滚 120 次，修正量随档位递增（c0 22876 / c1 33339 /
  c2 44841）。
- 同种子两次生成逐文件 `diff` 一致（生成器确定性）。
- 无头演示 `python3 -m syncdemo demo --sample sample --headless 120` 正常运行；
  界面截图确认帧模式标注、双视图与修正量曲线渲染正确。

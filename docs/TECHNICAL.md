# 确定性回合制对局回放系统 — 技术说明

## 1. 模块架构

| 模块 | 职责 |
| --- | --- |
| `replay/config.py` | 配置加载；`FrozenConfig` 为冻结 dataclass，运行期无法修改并发上限与预算 |
| `replay/model.py` | 对局描述、资源用量、初始运行时状态的数据模型与校验 |
| `replay/resources.py` | 确定性资源计量器；每个采样点即资源检查点，超预算抛 `BudgetExceeded` |
| `replay/engine.py` | 规则引擎：种子化 PCG64 随机数、战斗规则、事件流、逐回合状态与哈希 |
| `replay/hashing.py` | 规范化 JSON、SHA-256 哈希链、临时文件 + `os.replace` 原子写 |
| `replay/snapshot.py` | 快照提交、哈希链校验、最近已提交快照恢复 |
| `replay/task.py` | 单任务状态机、协作式暂停/取消、任务级轨迹日志 |
| `replay/scheduler.py` | 线程池调度、全局并发信号量、稳定提交序号、全局轨迹日志 |
| `replay/verifier.py` | 基线生成、版本闸门、逐回合哈希比对与差异定位 |
| `replay/samplegen.py` | 多种规模/种子的确定性样本与基线生成器 |
| `replay/gui.py` | PySide6 界面，QTimer 轮询线程安全状态，不阻塞调度线程 |
| `replay/cli.py` | 命令行入口 |

## 2. 确定性保证

1. 随机数仅来自 `numpy.random.Generator(PCG64(rng_seed))`；RNG 的完整
   `bit_generator.state`（含 `has_uint32/uinteger` 缓冲）随每回合快照保存，
   恢复时整体还原，保证中断后续跑随机流与一次跑完完全一致。
2. 所有 JSON 以 `sort_keys=True`、固定分隔符、UTF-8、LF 换行写出；
   不写入时间戳、文件大小、进程 ID 等环境相关字段。
3. 资源消耗由输入（回合号、玩家 ID、操作类型）经固定公式推导，
   不读取系统时间或进程 RSS，因此跨机器/跨平台可复现。
4. 全局轨迹日志在调度器关闭时按 `(match_id, turn, observation)` 排序后落盘，
   消除线程调度时序对字节输出的影响；任务级轨迹只记录该任务自身的有序观测。
5. 比对只使用版本号与内容哈希，明确禁止以文件大小或时间戳作为一致性依据。

## 3. 资源与预算

- 全局资源池：`max_concurrency` 个槽位的有界信号量，任务获取槽位前在队列等待。
- 单任务预算：`task_compute_budget` / `task_memory_budget`，累计超出即
   `E_BUDGET_EXCEEDED`，任务终止并保留最后一次已提交快照，等待恢复；
   不存在静默降级或运行期提高预算的路径。
- 可运行任务按提交序号 `seq` 稳定排序（提交顺序即入线程顺序，信号量 FIFO 放行）。

## 4. 快照与恢复

- 每回合结束先写 `snapshot_XXXXXX.json.tmp`，`fsync` 后 `os.replace` 原子重命名；
  在途回合只存在于内存，不会污染正式快照目录。
- 快照含回合号、完整状态、`state_hash`、`event_prefix_hash`、
  `prev_snapshot_hash`、`snapshot_hash` 与累计资源用量。
- 恢复时从回合 1 起逐节点验证哈希链与状态内容哈希；断裂、缺片或内容不符返回
  `E_RECOVERY_INVALID`，无快照却要求恢复同样返回该错误码。
- 强制终止后只依据磁盘快照恢复，不承诺恢复任何在途回合。

## 5. 状态机与异常链路

- 状态：`WAITING / RUNNING / PAUSED / COMPLETED / CANCELLED / FAILED`，
  其中业务要求的四种核心状态运行中、等待中、已完成、已取消全部具备，
  `PAUSED` 与 `FAILED` 为可诊断的补充状态；`transitions()` 提供状态转换查询。
- 同一 `match_id` 未终态重复提交返回 `E_BUSY`；终态后允许重新提交用于恢复/重跑。
- 取消为协作式：只在回合边界与资源检查点响应，取消后释放信号量槽位，
  已提交快照保留。

## 6. 验证与差异诊断

- 基线版本与回放器版本不一致：直接 `E_BASELINE_MISMATCH`，不执行任何比对。
- 比对顺序：判定结果 → 事件流总哈希 → 逐回合
  `state_hash / event_prefix_hash / snapshot_hash`，报告第一个不一致的
  回合号与字段路径（如 `turns[3].state_hash`）。

## 7. GUI 线程模型

- 调度器与任务全部在后台 daemon 线程；GUI 通过 200ms QTimer 调用
  `scheduler.statuses()`（带锁快照）刷新表格与资源趋势，界面线程不执行回放。
- 关闭窗口触发 `shutdown()`：协作式取消、`join` 工作线程、落盘全局轨迹，
  文件句柄随 `with open(...)` 与原子写自动关闭。
- 无显示器环境可用 `QT_QPA_PLATFORM=offscreen` 做自动化冒烟。

## 8. 业务错误码

| 错误码 | 触发场景 |
| --- | --- |
| `E_BUSY` | 同一对局任务未终态时重复提交 |
| `E_BUDGET_EXCEEDED` | 累计算力/内存超过配置预算 |
| `E_RECOVERY_INVALID` | 快照缺失、哈希链断裂或无快照恢复 |
| `E_BASELINE_MISMATCH` | 基线版本与回放器版本不一致 |
| `E_CANCELLED` | 协作式检查点响应取消 |
| `E_VERIFICATION_FAILED` | 基线内容比对发现首个差异 |

## 9. Windows 运行

```bat
scripts\run_windows.bat
```

脚本创建 `.venv`、安装 `numpy/PySide6`、生成样本、执行回放与验收并启动 GUI。
全部文件 IO 使用 `pathlib` 与 `os.replace`，换行统一 LF，路径不依赖 POSIX 特性。

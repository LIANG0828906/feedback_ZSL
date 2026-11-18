# 中断可续训的两层 MLP 参数服务器

这是一个面向 Windows 10/11、Python 3.10、PyTorch 2.1 CPU 的单机多进程验证工程。一个参数服务器持有完整模型、优化器状态和全局步，四个训练工作进程各自声明并缓存一个按张量维度切分的参数分片；训练过程中可关闭指定工作进程连接，并在确定性恢复窗口内由新进程接管，最终参数与从未中断的单进程对照训练逐元素一致。

## 快速开始

```powershell
cd <repo>
.\scripts\setup_windows.ps1
.\scripts\run_demo.ps1
```

完整验收入口：

```powershell
.\.venv\Scripts\python.exe -m mlp_ps.demo --config config\demo.json --repeat 2
```

`--repeat 2` 会运行两遍相同的 200 步作业，并逐字节比较两份 `status.jsonl`。结果写入：

- `artifacts/demo/verification_report.json`：总验收报告。
- `artifacts/demo/repeat-01/`、`repeat-02/`：两次作业的完整证据。
- `artifacts/demo/repeat-01/final/`：服务器的末端参数。
- `artifacts/demo/repeat-01/control/`：单进程连续训练对照结果。
- `artifacts/demo/repeat-01/checkpoints/`：一致性检查点文件集合。
- `artifacts/demo/repeat-01/status.jsonl`：每步确定性训练状态。
- `artifacts/demo/repeat-01/metrics.jsonl`：带时间戳的耗时和检查点重叠指标。

单次服务器训练约 12 秒，`--repeat 2` 连同对照和校验约 25-30 秒。

## 训练与分片语义

- 模型为 `Linear(16,32) -> ReLU -> Linear(32,4)`，共四个参数张量。
- 每个张量沿第 0 维切成 4 个连续、互不重叠、并集覆盖完整的切片；每个工作进程拥有每个张量的一片。
- 每个全局步使用 128 个确定性样本，按 4 个连续数据分区分给 4 个工作进程，每片 32 个样本。
- 工作进程根据服务器广播的完整参数计算本地批次的全梯度，并回报梯度、自己负责的最新参数切片和样本进度。
- 服务器按分片号 `0,1,2,3` 的固定顺序累加四个梯度并取平均，再以单线程 SGD + momentum 更新完整模型。
- 所有浮点载荷为 little-endian float32；所有进程固定 `OMP/MKL/OpenBLAS` 为单线程，并启用 PyTorch 确定性算法。
- 单进程对照脚本重复完全相同的四分区梯度聚合和优化器步骤，不经过网络。

分片布局标识是布局规范 JSON 的 SHA-256 前 16 位。启动时校验每个参数的切片恰好覆盖完整第 0 维；未知分片、重复声明或覆盖缺口均以 `E_LAYOUT_CONFLICT` 拒绝。

## 故障与续训

`config/demo.json` 中：

```json
{
  "fault": {
    "enabled": true,
    "shard_id": 2,
    "after_step": 100,
    "outage_steps": 70,
    "stall_tolerance_steps": 90,
    "replacement_id": "worker-2-recovery",
    "replacement_delay_ms": 100
  }
}
```

故障链路如下：

1. 分片 2 的初始工作进程在第 100 步回报后主动关闭 TCP 连接。
2. 服务器在下一步发送时检测断连，将分片 2 标记为 pending，状态中的活动工作进程数变为 3。
3. 其余三个工作进程继续接收参数和提交梯度，不发生等待。
4. 为确保全局轨迹与不间断训练完全一致，服务器用该分片的确定性数据批次在权威参数上执行一次等价梯度计算。该修复提交在状态中标记为 `"source":"repair"`，并计入样本进度，不会重复或跳过样本。
5. 验收编排器在观察到 pending 后按 `replacement_delay_ms` 启动新工作进程。新进程从最近完成检查点验证分片参数、动量和样本游标，再重放内存恢复日志到当前步。
6. 服务器在固定 `outage_steps` 后激活接管者；后续状态恢复活动进程数 4，分片 2 的提交来源回到 `"worker"`。
7. 如果 pending 连续步数超过 `stall_tolerance_steps`，服务器广播 `E_TRAIN_STALL` 并退出，不会无限等待。

该设计同时满足两个约束：存活进程的全局步持续前进；全局参数轨迹与四个进程从未断开时完全相同，而不是依赖近似补偿。

## 检查点

`checkpoint.steps` 默认是 `[0,50,100,150,200]`。每个检查点包含：

- 完整布局与布局标识、模型/优化器元数据、全局步。
- 四个分片的参数切片与 SGD momentum 切片。
- 每个工作进程的已消费样本数。
- 每个文件、每个切片的 SHA-256。
- `.complete` 完成标记，其中记录 manifest 哈希。

检查点快照在短临界区内复制，实际文件写入由后台线程完成。`checkpoint.bandwidth_mbps` 控制本地写带宽，默认 20 MB/s；`padding_bytes` 添加确定性填充文件，使慢盘效果可观察。训练不等待后台写盘。检查点捕获耗时、总字节数、实际耗时、目标与实际带宽均写入 `checkpoint_events.jsonl`。

独立校验：

```powershell
.\.venv\Scripts\python.exe -m mlp_ps.checkpoint verify --path artifacts\demo\repeat-01\checkpoints\ckpt-00000100
.\.venv\Scripts\python.exe -m mlp_ps.checkpoint latest --root artifacts\demo\repeat-01\checkpoints
```

目录不存在返回 `E_CKPT_MISSING`；缺文件、manifest/分段哈希不符、布局不完整或完成标记缺失返回 `E_CKPT_CORRUPT`。

## 状态与确定性

`status.jsonl` 每完成一个优化步写一行，字段按 JSON 键稳定排序；`submissions` 始终按分片号排序。每行包含：

- `step`、`layout_id`、`global_batch_size`。
- 每个分片的 `submission_seq`、提交来源、工作进程 ID、样本进度。
- `aggregate_gradient_norm`、`consumed_samples_total`。
- `active_worker_count`、`active_worker_ids`、`pending_shards`。

`submission_seq = step * 4 + shard_id` 是可复现的确定性聚合顺序，不包含实际到达时间。实际到达时间和耗时放在 `metrics.jsonl`，因此重复运行时 `status.jsonl` 可逐字节一致。

## 业务码

| 业务码 | 退出码 | 含义 |
|---|---:|---|
| `E_CONFIG` | 2 | 配置字段或取值范围错误 |
| `E_LAYOUT_CONFLICT` | 10 | 分片重复声明、未知分片、布局覆盖冲突 |
| `E_PARAM_SHAPE` | 11 | 参数/梯度维度、段边界或数值不合法 |
| `E_CKPT_MISSING` | 12 | 续训时没有完整检查点 |
| `E_CKPT_CORRUPT` | 13 | 检查点文件缺失、哈希或布局校验失败 |
| `E_PROGRESS_RANGE` | 14 | 样本预算不足、游标越界、重复或跳号 |
| `E_TRAIN_STALL` | 15 | 断连超过容忍步数 |
| `E_REGISTRATION_TIMEOUT` | 16 | 初始四个工作进程未按时注册 |
| `E_PROTOCOL` | 17 | TCP 协议消息、步号或布局不一致 |
| `E_NETWORK` | 18 | 绑定、连接或传输失败 |
| `E_INTERNAL` | 19 | 未归类的内部失败 |

任何服务器侧异常都会先向仍存活的工作进程发送带 `code`、`message`、`details` 的 `ABORT`，再关闭连接并退出。工作进程也把同一业务码写入自己的 `worker_result_*.json`。

## 异常链回归

```powershell
.\.venv\Scripts\python.exe -m unittest discover -s tests -v
```

测试包含：

- 分片完整、互斥和布局冲突。
- 检查点往返与篡改检测。
- 样本预算越界。
- 真实 TCP 下重复分片声明：服务器广播 `E_LAYOUT_CONFLICT`。
- 真实 TCP 下错误梯度维度：存活进程收到 `E_PARAM_SHAPE`。
- 真实 TCP 下断连且无接管：存活进程收到 `E_TRAIN_STALL`，无悬挂。

## 本次本机验收结果

环境：Linux 容器内使用 Python 3.10.22 + `torch 2.1.2+cpu` 做功能验证；代码仅使用 Windows 同样支持的 Python 标准库、asyncio 和 PyTorch CPU API。

`--repeat 2` 结果：

- 两次运行 `status.jsonl` 逐字节一致。
- 服务器端与单进程对照端参数最大绝对误差：`0.0`，阈值 `1e-5`。
- 评估 MSE：`0.0685898513 -> 0.0403899699`，未回落。
- 200 步平均 round 耗时：约 `3.4 ms`，上限 `150 ms`（不含 `step_pace_ms`）。
- 检查点捕获最大耗时：约 `0.3 ms`，后台写盘未阻塞训练。
- 每个约 2 MB 检查点在 20 MB/s 限速下耗时约 `0.12 s`，实际约 `17.1-18.2 MB/s`。
- 接管发生在第 `171` 步，共 `70` 次修复提交；接管后活动工作进程恢复为 4。
- 训练期间峰值常驻内存约 `1081 MiB`，低于 `1.25 GiB`；五个训练期 PyTorch 进程均固定单线程。

## 配置

`config/demo.json` 的关键字段：

- `run.seed/host/port/run_dir`：固定种子、回环地址、端口和输出目录。
- `training.sample_count/batch_size/total_steps`：样本预算、全局批大小、总步数；预算必须不少于 `total_steps * batch_size`。
- `training.learning_rate/momentum`：SGD 配置。
- `fault.*`：断连分片、断连步、固定恢复窗口、容忍步数和接管者 ID。
- `checkpoint.steps`：检查点步；必须含 0。
- `checkpoint.bandwidth_mbps/padding_bytes`：慢盘限速与可观测填充。
- `network.*`：注册、单步、广播超时和最大帧。
- `runtime.step_pace_ms`：仅用于让外部验收编排稳定观察到接管窗口，不计入 round 耗时指标。

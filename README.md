# 异步参数服务器训练协调器

这是一个从零实现的本地异步训练系统，面向 2 核 CPU、1.25GiB 内存的 Windows
主机。系统包含 1 个协调器、2 个独立参数服务器分片进程、3 个独立训练工作进程，
三者只通过 gRPC 通信；所有可核对状态落在工作区内的 SQLite 中。

## 快速开始（Windows）

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.txt
.\.venv\Scripts\python.exe generate_data.py --config config.json
.\.venv\Scripts\python.exe run_training.py --config config.json --fresh --with-faults
```

Linux / WSL 对等命令：

```bash
python3.11 -m venv .venv
.venv/bin/python -m pip install -r requirements.txt
.venv/bin/python generate_data.py --config config.json
.venv/bin/python run_training.py --config config.json --fresh --with-faults
```

训练完成后查看：

- `reports/training_report.md`：可读验收报告。
- `reports/verification.json`：逐参数、逐步、故障事件核对结果。
- `reports/run_report.json`：端到端时间、故障注入序列和最终状态。
- `run/training_state.sqlite3`：训练状态与审计库。

## 架构

```text
worker-0 --\
worker-1 ----> Coordinator (gRPC :50310, HTTP :50311)
worker-2 --/       |  registration / heartbeat / routing / step transaction
                   |
             +-----+------+
             |            |
        shard-0      shard-1
        :50321       :50322
             \            /
              shared SQLite WAL
```

- 工作进程直接调用其分配到的分片 `GetParameters`，不经过完整参数代理。
- 协调器按参数名 `SHA-256(name) % shard_count` 路由全部 650 个参数。
- 每一步严格等到 3 个登记工作进程各 64 条样本的梯度到齐后，以固定
  `worker-0 -> worker-1 -> worker-2` 顺序做样本加权平均，再执行一次 SGD。
- 没有中央 `Barrier` RPC；工作进程独立推进、重试和心跳。协调器用
  `max_pending_steps`、`max_pending_submissions` 两个硬上限做有界背压，避免
  700 条无界积压。
- 参数名、参数向量顺序、样本块顺序和参考实现完全共用 `train/common.py`
  与 `train/model.py`。

模型是 64 维、10 类的线性 softmax，650 个参数，float64 训练。`torch==2.3.1+cpu`
按目标环境写入依赖，但训练核心使用 NumPy 的确定性 float64 小矩阵路径，以控制
6 个 Python 进程同时运行时的常驻内存。数据由固定种子生成为 40 万样本，并按
133334 / 133333 / 133333 分成三个本地 `.npy` 分片，大小约 104MiB。

## 一致性与恢复语义

### 登记和路由

- 工作进程登记 `worker_id + data_shard_index + sample_count`。
- 分片登记 `shard_id + endpoint + parameter_names`，参数集合必须与其哈希结果一致。
- 同一身份重复登记返回原记录，不创建第二实例。
- 声明冲突返回稳定错误码，原记录保持不变：
  - `WORKER_IDENTITY_CONFLICT`
  - `SHARD_PARAMETER_SET_CONFLICT`
  - `SHARD_OWNERSHIP_CONFLICT`
- 其他稳定业务码包括 `STALE_VERSION`、`DUPLICATE_GRADIENT`、
  `INVALID_BASE_VERSION`、`PIPELINE_BACKPRESSURE`、`TAKEOVER_IN_PROGRESS`。

### 提交、重复与过期

- 每个工作进程每个逻辑步只允许一条 `applied` 提交，由 SQLite 部分唯一索引
  和内存待提交桶双重保证。
- 重复提交不会再次聚合参数，返回该步已经应用后的版本。
- `current_version - base_version > max_stale_steps` 时返回 `STALE_VERSION`，
  工作进程重新读取最新参数并重算。
- 连续过期次数超过 `max_consecutive_stale` 后，工作进程被标记为 `lagging`，
  记录触发版本差和 `lag_events`。
- 被拒绝的过期梯度、被丢弃的重复梯度都保存原始梯度 BLOB 和版本信息。

### 原子步提交

一个 `BEGIN IMMEDIATE` 事务同时写入：

- `steps`：步号、全局参数版本、损失、耗时。
- `submissions`：该步三个已应用梯度、基础版本、延迟。
- `routes`：650 个参数名到当前分片的完整路由结果。
- `parameters` 与 `parameter_history`：新参数和该版本快照。
- 待提交阶段收到的重复记录。

未完成步只存在于内存中，不写 `steps`。已完成步在重启后不会被重算；如果提交
响应丢失，工作进程重发时命中重复判定。

### 分片接管

- 分片每 300ms 心跳，超过 1.8s 未心跳即标记 `dead`。
- 协调器选择存活分片，从 SQLite 已提交参数快照构造接管负载，先调用目标分片
  `InstallTakeover` 安装 `shard_cache`，成功后用一个事务整体切换所有权。
- 所有权表以 `parameter_name` 为主键，因此任一参数最多由一个分片负责。
- 接管期间步骤提交保留在待提交桶，工作进程等待接管完成；无法完成时可返回明确
  错误，不会读到缺失或置零参数。
- 协调器网络调用不持有全局状态锁，存活分片的提交处理不会被接管 RPC 阻塞。

## 运行和故障注入

```powershell
# 正常 300 步
.\.venv\Scripts\python.exe run_training.py --fresh

# 重复、过期、终止 shard-1
.\.venv\Scripts\python.exe run_training.py --fresh --with-faults

# 再在第 150 步强杀并重启协调器
.\.venv\Scripts\python.exe run_training.py --fresh --with-faults --restart-coordinator-at-step 150
```

故障脚本可以单独运行，不需要修改服务代码：

```powershell
.\.venv\Scripts\python.exe tools\inject_duplicate.py --config config.json
.\.venv\Scripts\python.exe tools\inject_stale.py --worker-id worker-1 --count 4
.\.venv\Scripts\python.exe tools\kill_shard.py --shard-id shard-1
```

`tools/pause_process.py` 用于把工作进程短暂暂停，确保四次过期 RPC 真正连续，
从而验证滞后标记；它不参与正常训练。

## 状态与指标

协调器状态端口提供：

- `GET http://127.0.0.1:50311/status`
- `GET http://127.0.0.1:50311/metrics`
- `GET http://127.0.0.1:50311/healthz`

JSON 使用稳定键排序，分片和工作进程按 ID 排序，参数名排序，不输出绝对路径。
内容包括分片实际参数集合、最近已应用步、工作进程 `lagging` /
`trigger_version_gap`、最近接管记录和事件计数。

稳定 Prometheus 指标：

- `training_step_duration_seconds`
- `training_gradient_submit_duration_seconds`
- `training_workers_alive`
- `training_shards_up`
- `training_shard_up{shard_id}`
- `training_shard_status{shard_id,status}`
- `training_gradient_submissions_total{result}`
- `training_gradient_discarded_total{reason}`
- `training_shard_gradient_submissions_total{shard_id}`
- `training_shard_gradient_discarded_total{shard_id,reason}`
- `training_stale_rejections_total{shard_id}`
- `training_shard_takeovers_total{from_shard,to_shard}`
- `training_parameter_version`
- `training_pending_submissions`
- `training_takeover_in_progress`

## 校验方式

`train/reference.py` 按与工作进程完全相同的固定样本块顺序做单进程同步 SGD。
`train/verify.py` 进一步执行：

1. 最终参数与参考实现逐参数比较，阈值 `1e-4`。
2. 300 步损失序列比较，最差相对误差阈值 `1e-3`。
3. 对每个已应用提交，从其记录的 `base_version` 历史参数重算梯度。
4. 从已应用梯度重算每一步参数更新。
5. 检查每步 650 条路由、每步 3 条提交、参数历史完整、所有权无重叠。
6. 检查工作进程 P95 不超过 400ms、接管 30s 内恢复。
7. 在 `affected_steps_and_parameters` 列出过期、重复和接管影响的步、参数名及
   偏差来源，不只报告最终参数。

```powershell
.\.venv\Scripts\python.exe -m unittest discover -s tests -v
.\.venv\Scripts\python.exe -m train.verify --config config.json
```

## 实测结果

在本次工作区、3 工作进程 + 2 分片 + 协调器、300 步、包含重复/连续过期/
强制终止 shard-1/协调器强杀重启的验收中：

- 端到端约 10-13 秒，远低于 8 分钟。
- 最终参数最大偏差 `0.0`。
- 损失序列最大相对误差 `0.0`。
- 梯度独立重算最大偏差 `0.0`。
- 验证准确率 `1.000000`，同步参考 `1.000000`。
- 工作进程提交 P95 约 10ms，远低于 400ms。
- 分片接管约 0.2 秒完成，远低于 30 秒。
- SQLite、数据合计约 132MiB；所有进程由 2 核绑核运行，峰值常驻内存低于 1GiB。

具体数字以最后一次 `reports/verification_summary.json` 为准。

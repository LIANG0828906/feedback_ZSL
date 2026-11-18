# order-events 消费者组协调服务

面向 Windows 单机的 Kafka 3.6 / Python 3.11 消费者组协调器。协调器通过
HTTP/1.1 管理 3 个独立消费者工作进程，使用 SQLite 持久化成员、分配、心跳与
提交记录，并通过受控拉取与批次租约保证同一分区不会同时交给两个成员。

项目只依赖 Python 标准库。Kafka 访问使用内置的 Kafka 3.6 wire-protocol
客户端，无需安装 `kafka-python` 或 `confluent-kafka`。

## 核心保证

| 能力 | 实现方式 |
| --- | --- |
| 分区独占 | 分区主键分配 + generation + 已派发批次租约；撤销完成前不分配给新成员 |
| 独立心跳 | 每个工作进程使用独立心跳线程；消费停顿不阻塞心跳 |
| 会话超时 | 超过成员声明的 `session_timeout_ms` 才回收；记录回收原因、时刻和分区 |
| 重平衡抑制 | 分区数阈值、冷却窗口；延迟/合并期间原成员继续消费，失联成员分区隔离 |
| 提交绑定 | 校验活跃成员、分区归属和 generation；拒绝回退，重复提交幂等 |
| 暂停/恢复 | 暂停分区固定归属、冻结已提交偏移；拉取返回空，恢复从提交点继续 |
| 滞后核对 | 开始时刻快照提交偏移，再查询 Kafka 末端偏移；后续提交不会污染结果 |
| 可观测性 | Prometheus 分区级偏移/滞后/归属/提交计数和成员/重平衡计数 |

## 目录

```text
ordergroup/             协调器、HTTP 服务、Kafka 客户端、工作进程、生成器
config/                 协调器与故障注入配置
scripts/                Windows PowerShell 启动脚本
tools/acceptance.py     独立进程端到端验收
tests/                  unittest 测试
docs/API.md             HTTP API 与错误码
VERIFICATION.md         实测记录
artifacts/              验收报告与 trace
```

## 环境准备

1. Windows 10/11 或 Windows Server 2019+。
2. Python 3.11。
3. Java 17。
4. Kafka 3.6.0 二进制包，例如 `C:\kafka\kafka_2.13-3.6.0`。

项目无需 `pip install`。若本机还没有 Kafka，请先下载并解压 Kafka 3.6.0，
然后执行：

```powershell
$env:JAVA_HOME = "C:\Program Files\Java\jdk-17"
powershell -ExecutionPolicy Bypass -File .\scripts\bootstrap_kafka.ps1 `
  -KafkaHome "C:\kafka\kafka_2.13-3.6.0"
```

脚本会：

- 在 `data\kafka` 下写入 KRaft 单节点配置；
- 首次运行时生成 cluster id 并 format 存储目录；
- 监听 `127.0.0.1:9092`，控制器监听 `127.0.0.1:9093`；
- 创建 12 分区、单副本的 `order-events` 主题。

Kafka PID 写入 `data\kafka\kafka.pid`，日志位于 `data\logs`。

## 真实 Kafka 运行

### 1. 生成 2 万条确定性事件

```powershell
python -m ordergroup generate `
  --backend kafka `
  --bootstrap 127.0.0.1:9092 `
  --topic order-events `
  --partitions 12 `
  --count 20000 `
  --seed 290
```

事件格式固定为：

```json
{"amount":5427.42,"event_time":"2025-01-01T00:00:00.000Z","order_id":"ORD-0000000001"}
```

生成器按索引轮转写入 12 个分区，相同 seed/count 产生相同
`stream_sha256`。30 万条验收：

```powershell
python -m ordergroup generate --backend kafka --bootstrap 127.0.0.1:9092 `
  --partitions 12 --count 300000 --seed 290
```

### 2. 启动协调器

```powershell
python -m ordergroup coordinator --config .\config\coordinator.json
```

默认监听 `http://127.0.0.1:8090`，数据库为 `data/coordinator.sqlite3`。
首次启动或需要清空状态时可加 `--reset-state`。

### 3. 启动 3 个独立工作进程

分别打开 3 个 PowerShell 窗口：

```powershell
python -m ordergroup worker --coordinator http://127.0.0.1:8090 `
  --member-id worker-1 --session-timeout-ms 10000 `
  --heartbeat-interval-ms 1000 --batch-size 250
```

```powershell
python -m ordergroup worker --coordinator http://127.0.0.1:8090 `
  --member-id worker-2 --session-timeout-ms 10000 `
  --heartbeat-interval-ms 1000 --batch-size 250
```

```powershell
python -m ordergroup worker --coordinator http://127.0.0.1:8090 `
  --member-id worker-3 --session-timeout-ms 10000 `
  --heartbeat-interval-ms 1000 --batch-size 250
```

也可以运行 `.\scripts\run_workers.ps1` 一次启动 3 个进程。

### 4. 查询状态

```powershell
Invoke-RestMethod http://127.0.0.1:8090/healthz
Invoke-RestMethod http://127.0.0.1:8090/v1/status
Invoke-RestMethod http://127.0.0.1:8090/v1/lag
Invoke-WebRequest http://127.0.0.1:8090/metrics
```

`/v1/status` 返回成员、每个成员持有的分区、暂停分区、隔离分区、最近一次
重平衡原因及最近事件。`/v1/lag` 返回每个分区的已提交偏移、Kafka 末端偏移和
滞后条数。

## 无 Kafka 的确定性演示

模拟底座保持与 Kafka 相同的偏移和分区语义，适合自动测试：

```powershell
python -m ordergroup generate --backend simulator --partitions 12 `
  --count 20000 --seed 290 --output-dir .\data\simulated

python -m ordergroup coordinator --backend simulator `
  --simulator-dir .\data\simulated --db .\data\sim-demo.sqlite3 --port 8090
```

另开窗口运行 3 个 worker，参数同上。也可执行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\run_demo.ps1
```

模拟底座只用于本地验收与故障回归，不替代真实 Kafka 部署。

## 故障注入

工作进程支持以下独立故障参数：

- `--stop-heartbeat-after-ms 2500`：停止心跳但继续运行，由协调器按会话超时踢出；
- `--stall-after-records 150 --stall-duration-ms 1500`：模拟消费停顿，心跳继续；
- `--pause-after-records 150 --pause-duration-ms 1500`：调用真实暂停/恢复 API；
- `--consume-delay-ms 5`：放慢单条事件处理；
- `--processed-log FILE`：逐事件写处理账本，供去重与漏处理核对。

也可以使用 `config/faults.json`：

```powershell
python -m ordergroup worker --config .\config\coordinator.json `
  --member-id worker-1 --faults .\config\faults.json
```

## 重平衡配置

`config/coordinator.json` 中的关键参数：

```json
{
  "rebalance": {
    "max_partitions_per_rebalance": 6,
    "cooldown_ms": 5000,
    "partition_threshold_delay_ms": 1500,
    "suppression_max_delay_ms": 8000,
    "revoke_ack_timeout_ms": 2500,
    "initial_assignment_immediate": true
  }
}
```

- 预期变更分区数大于阈值时，合并请求并延迟执行；
- 距上次完成不足冷却时长时，等待冷却结束；
- 抑制记录写入 `rebalances` 表，原因同时出现在 `/v1/status` 和事件日志；
- 抑制期间原有成员继续持有并消费原分区；
- 超时/主动离开成员的分区立即进入隔离区，任何其他成员都不能消费；
- 队列中的请求会合并为一次分配，避免重平衡风暴。

实际切换采用两阶段撤销：

1. 目标分区标为 `revoking`，旧成员不能再拉取；
2. 已派发但未提交的批次必须完成提交或显式释放；
3. 之后原子写入新 generation 的分配；
4. 新成员从该分区最后提交偏移继续。

## 提交与暂停语义

- 提交值是下一条待消费偏移（offset + 1）。
- 提交必须来自当前持有该分区的活跃成员。
- `offset < committed_offset` 返回 `OFFSET_REGRESSION`。
- `offset == committed_offset` 幂等成功，进度不会回退。
- 暂停期间提交返回 `PARTITION_PAUSED`，已提交值保持不变。
- 暂停分区固定在原成员，普通加入/离开重平衡不会把它转给其他成员。
- 成员离开或心跳超时后暂停自动结束，分区进入隔离，由后续分配处理。

完整路由、请求/响应及错误码见 [docs/API.md](docs/API.md)。

## 指标

`GET /metrics` 返回 Prometheus 文本格式，主要指标：

```text
order_group_committed_offset{topic,partition}
order_group_lag{topic,partition}
order_group_partition_assigned{topic,partition,member}
order_group_partition_paused{topic,partition}
order_group_offset_commits_total{partition,outcome}
order_group_offset_rejections_total{partition,reason}
order_group_member_heartbeat_timeouts_total{member}
order_group_rebalance_total
order_group_rebalances_suppressed_total{reason}
order_group_active_members
order_group_member_active{member}
order_group_generation
```

## 测试与验收

单元和 HTTP 测试：

```powershell
python -m unittest discover -s tests -v
```

端到端独立进程验收（模拟底座、3 worker、暂停/停顿/心跳超时）：

```powershell
python -m tools.acceptance --backend simulator --count 20000 --faults full
```

30 万条：

```powershell
python -m tools.acceptance --backend simulator --count 300000 --faults full `
  --timeout-seconds 600
```

真实 Kafka 验收要求主题为空：

```powershell
python -m tools.acceptance --backend kafka --bootstrap 127.0.0.1:9092 `
  --count 20000 --faults full
```

验收脚本检查：

- 最终总滞后为 0；
- 3 个工作进程账本中每个订单号恰好出现一次；
- 活跃成员的分区集合互不重叠；
- 12 个分区的 Prometheus 偏移/滞后/归属序列齐全；
- 故障模式下确实发生暂停、恢复、心跳超时与重平衡抑制。

`full` 模式中，故障成员使用 3 秒会话窗口并在 1 秒后停止心跳，正常成员使用
60 秒窗口，以区分“真实超时”与宿主机调度抖动。

报告写入 `artifacts/acceptance_report.json`，过程 trace 写入
`artifacts/trace.jsonl`。

## 资源

- 协调器单进程通常低于 100 MiB；
- 每个 worker 通常低于 80 MiB；
- 2 万条模拟数据约数 MiB，30 万条约数十 MiB；
- Kafka 建议设置 `KAFKA_HEAP_OPTS=-Xms192m -Xmx384m`；
- SQLite 使用 WAL，提交为单行 upsert，不保存事件正文。

在 2 逻辑 CPU、1.25 GiB 总内存限制下，推荐 Kafka 约 384 MiB、协调器约
100 MiB、3 个 worker 合计约 250 MiB，保留系统余量。

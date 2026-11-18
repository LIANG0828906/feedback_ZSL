# 验证记录

## 自动化测试

```text
python -m unittest discover -s tests -v
Ran 13 tests ... OK
```

覆盖：

- 三成员 12 分区独占与均衡分配；
- 心跳超时回收、隔离和指标；
- 阈值/冷却抑制期间旧成员持续持有分区；
- 提交回退拒绝、重复提交幂等、非 owner 拒绝；
- 暂停冻结提交、暂停固定归属、恢复继续消费；
- 批次租约阻止撤销完成前重新分配；
- 持有关系连续时跨 generation 提交；
- 重平衡进行中到达的后续成员请求被合并，不产生额外 generation；
- 核对快照与并发提交隔离；
- Prometheus 必需序列；
- 确定性生成与 Kafka RecordBatch CRC32C/编解码；
- HTTP join/status/metrics/lag。

## 真实 Kafka 3.6.0 KRaft 集成

测试环境：

- Apache Kafka `3.6.0`（Scala 2.13）
- KRaft 单节点，临时测试端口 `127.0.0.1:19092`
- `order-events` 12 分区、单副本
- OpenJDK 17

结果：

1. 内置客户端批量写入 100 条事件成功；
2. Kafka 官方 console consumer 可读取；
3. `ListOffsets` 返回 12 分区末端偏移；
4. `Fetch v4` 成功解析 RecordBatch，事件 JSON、key、offset 正确；
5. 3 个 worker 消费完成后核对，12 个分区 `lag=0`。

随后重建空主题并写入 2000 条事件，运行故障场景：

- `worker-1`：达到 200 条后暂停分区 3 秒；
- `worker-2`：达到 200 条后消费停顿 2 秒，心跳线程继续；
- `worker-3`：1.5 秒后停止心跳，会话超时后被协调器回收。

观测结果：

```text
processed rows:      2000
unique order ids:    2000
duplicate events:    0
missing events:      0
final total lag:     0
```

暂停事件：

```text
partition=6 committed_offset=40 -> pause -> resume committed_offset=40
```

超时与重平衡指标：

```text
order_group_member_heartbeat_timeouts_total{member="pw3"} 1
order_group_rebalance_total 4
order_group_rebalances_suppressed_total{reason="cooldown"} 2
```

最终成员重新分配到 6/6 个分区，无重复处理。

## 30 万条验收

执行：

```powershell
python -m tools.acceptance --backend simulator --count 300000 --faults full `
  --timeout-seconds 900
```

验收脚本实际启动协调器与 3 个独立 worker 子进程。正常 worker 使用 60 秒会话
窗口，故障 worker 使用题面中的 3 秒窗口并在 1 秒后停止心跳；同时触发暂停、
消费停顿和重平衡抑制。

实测报告（`artifacts/acceptance_report.json`）：

```text
requested events:       300000
processed rows:         300000
unique order ids:       300000
duplicate events:       0
events without order:   0
final total lag:        0
generation:             3
rebalance count:        3
heartbeat timeouts:     1
suppressed rebalances:  1 (partition_threshold:8>6)
```

最终存活成员仍各持有 6 个不重叠分区：

```text
worker-1: [0, 2, 4, 6, 8, 10]
worker-2: [1, 3, 5, 7, 9, 11]
worker-3: timed_out, []
```

验收检查全部通过：

```text
active_assignments_exclusive: true
all_events_present:           true
heartbeat_timeout_exercised:  true
lag_zero:                     true
metrics_partition_series:     true
no_duplicate_events:          true
no_missing_order_ids:         true
pause_resume_exercised:       true
suppression_exercised:        true
```

本次数据目录实测约 60 MiB（事件日志 28 MiB、处理账本 33 MiB、SQLite
240 KiB）；协调器 RSS 约 40 MiB，单个 worker HTTP 处理期间 RSS 约 45 MiB，
明显低于 2 GB 临时数据和 1.25 GiB 总内存限制。

资源约束说明：Kafka 建议 384 MiB 堆，协调器与 3 个 worker 为 Python 进程；
模拟事件按分区流式写入，不在协调器保存事件正文。30 万条处理账本约数十 MiB，
总体低于 1.25 GiB 内存与 2 GB 临时数据上限。


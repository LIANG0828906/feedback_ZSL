# Edge Sync — 本地多出口验证服务

在单台 Windows 主机（同样可在 Linux/WSL 上运行）上用三个本地 TCP 代理模拟
有线 / VPN / 4G 三条出口，对遥测归档的分片上传做连接级拥塞信号采集与保守
出口切换。除 `pytest` 外仅使用 Python 3.11+ 标准库，适配 2 vCPU / 1.25 GiB
内存 / 2 GB 临时磁盘的并行运行约束。

## 架构

```
归档客户端 (orchestrator 内置工作负载)
   │  HTTP/1.1 chunked，每分片一条新 TCP 连接
   ├──────────► egress:wired:19101 ─┐
   ├──────────► egress:vpn:19102  ──┼──► 目标 HTTP 服务 /ingest (SQLite 权威字节范围)
   └──────────► egress:cellular:19103┘
                  链路仿真: RTT / 丢包 / 令牌桶限速 / Reno 风格 cwnd
服务入口 127.0.0.1:18080  /status  /metrics(Prometheus)  /report
```

模块职责：

- `edge_sync/link.py`：确定性链路仿真。固定 RTT、丢包率、带宽，里程碑式故障
  注入（按出口累计投递 MB），Reno 风格慢启动/拥塞避免/快重传/RTO。丢包采用
  “按序扣留 + 重传延迟”，真实 TCP 字节不丢，仅体现重传与 RTO 代价。
- `edge_sync/proxy.py`：三个出口 TCP 代理，HTTP/1.1 chunked 帧感知，转发到同一
  目标服务，并在响应头 `X-Link-Stats` 注入本连接仿真统计；执行每出口连接上限。
- `edge_sync/target_service.py`：目标 HTTP 服务。`(task_id, shard_seq)` 主键
  去重，校验 SHA-256，返回已接收字节范围；重复提交返回 `duplicate=true` 且不重复
  入库，校验失败返回稳定错误码。
- `edge_sync/tcp_probe.py`：连接级真实内核信号。Linux 用 `TCP_INFO`，Windows 用
  `SIO_TCP_INFO`（ctypes）。读不到的字段显式标记缺失与错误码，绝不用估计值冒充。
- `edge_sync/health.py`：滑动窗口健康评估。需要连续坏样本数与累计坏暴露时长同时
  超过抖动容忍才切换，瞬时尖峰不切换。
- `edge_sync/orchestrator.py`：调度与服务 API。每任务并发 4、合计 ≤16；分片在
  认领时才绑定出口，切换只改 `pending` 分片，在途分片不回滚、字节范围不重传。
- `edge_sync/metrics.py`：Prometheus 文本指标与字段稳定排序的同步报告。

## 快速开始

```bash
python -m venv .venv
# Windows: .venv\Scripts\activate ; Linux/macOS: source .venv/bin/activate
pip install -r requirements.txt

python -m edge_sync validate --config configs/edge_sync.json   # 仅校验资源/分片契约
python -m edge_sync run       --config configs/edge_sync.json   # 全量跑一次并出报告
python -m edge_sync stack     --config configs/edge_sync.json   # 只起服务，Ctrl+C 退出
pytest -q
```

小规模冒烟配置见 `configs/smoke.json`（48 分片，秒级）。生产形态配置
`configs/edge_sync.json` 为 4 任务 × 3000 分片，总流量约 160 MiB（低于 180 MiB
硬上限），出口并发 6/5/5，总并发 16。

## 信号采集口径

每个分片落库两类信号，互不冒充：

- `signal`：真实 OS 读数（RTT、RTO、cwnd、MSS、重传/RTO 计数的可得字段）。
  Windows 无直接 cwnd 字段时，用真实 `BytesInFlight/MSS` 并明确标注为近似；
  RTO 超时计数在部分内核/平台不可得时进入 `missing` 与 `missing_codes`。
- `link`：代理链路仿真读数（确定性 RTT 样本、重传段数、RTO 次数、cwnd 近似、
  限速等待与恢复耗时）。环回口上 OS 协议栈不会真拥塞，因此切换阈值默认对
  `link` 信号评估（`health.source=link`），`os` 信号仍如实展示。

稳定业务错误码：`SIG_PROBE_UNSUPPORTED`、`SIG_PROBE_ERROR`、
`SHARD_CHECKSUM_MISMATCH`、`SHARD_UPLOAD_TIMEOUT`、`SHARD_MAX_ATTEMPTS`、
`EGRESS_CONCURRENCY_CAP`、`HTTP_UPLOAD_FAILED`。

## 切换语义

- 触发：最近窗口内 RTT 中位数或重传率超阈值，且连续坏样本数与累计坏暴露时长
  同时满足（后者超过 `jitter_tolerance_ms`），短于容忍时长的尖峰不动作。
- 动作：把坏出口上所有 `pending` 分片改派到健康目标出口，记录触发指标、阈值、
  被切换分片数与目标出口；在途分片继续在原出口完成，目标服务幂等保证字节范围
  只成功提交一次。
- 恢复：冷却期后连续好样本达标才恢复健康，避免抖动来回切。

## 指标与报告

`GET /metrics` 输出（标签 `egress` / `task` 稳定）：

- `edge_sync_egress_uploaded_bytes_total`
- `edge_sync_egress_shards_total`
- `edge_sync_egress_retransmit_rate`
- `edge_sync_egress_rtt_median_ms`
- `edge_sync_egress_switch_total`
- `edge_sync_egress_active_connections`
- `edge_sync_task_shards_completed_total`
- `edge_sync_task_progress_ratio`

`GET /status` 返回出口健康度、进行中任务计数与最近一次切换；`GET /report` 返回
按任务/出口聚合的成功失败分片、字节、切换序列、重传与 RTT 分位。报告字段稳定
排序，`deterministic_fields` 列出的字段在相同配置与注入序列下跨运行一致
（仅耗时与时间戳不同）。

## 确定性

- 分片大小、负载、校验和、丢包决策、RTT 抖动均由固定种子经 SplitMix64 纯函数
  推导（`edge_sync/rng.py`），不依赖线程交错。
- 故障窗口按出口累计投递 MB 触发而非墙钟时间，避免宿主机快慢影响是否切换。
- 运行数据写入 SQLite WAL（`data/`），报告写入 `artifacts/`，均为运行期产物。

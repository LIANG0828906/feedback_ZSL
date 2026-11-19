# 边缘双栈入口

面向 24 台视频巡检上行设备的本机入口：同一进程监听 QUIC/UDP 与 TLS TCP，
将分片转发到本地 Python HTTP 后端，并用 SQLite 保存会话、确认范围、迁移、
拒绝和背压反馈。

## 快速验收

Linux/WSL：

```bash
make test
python3 scripts/run_scenario.py --duration 5 --seed 309
python3 scripts/check_report.py
```

Windows 11 / Windows 10 22H2（Go 1.22、Python 3.12）：

```powershell
.\scripts\run_windows.ps1 -Duration 5 -Seed 309 -Devices 24
```

脚本会先构建 `edge-gateway` 和 `loadgen`，选择四个预先空闲的本地端口，
启动后端与入口，运行固定种子场景，输出：

- `artifacts/scenario-report.json`：场景报告
- `artifacts/run/edgegateway.db`：SQLite 账本
- `artifacts/run/metrics.txt`：入口 Prometheus 文本快照
- `artifacts/run/*.log`：后端、入口、负载器日志

## 主要组件

| 路径 | 作用 |
| --- | --- |
| `cmd/edge-gateway` | QUIC/HTTP3 + TCP/HTTP2/1.1 入口、Prometheus、管理 API |
| `cmd/loadgen` | 固定种子 24 设备负载、迁移/延迟/拒绝注入、JSON 报告 |
| `backend/backend.py` | Python 3.12 标准库后端，分片幂等、范围确认、故障注入 |
| `internal/store` | SQLite schema 与关键状态事务 |
| `internal/gateway` | 会话、迁移、限速、拒绝暂停、并发窗口 |
| `scripts/check_report.py` | 报告不变量与 SQLite 数值逐项核对 |

## 监听端口

默认值见 `config.example.json`：

- QUIC/UDP：`0.0.0.0:4433`，ALPN `h3`
- TCP TLS：`0.0.0.0:8443`，协商 `h2` 或 `http/1.1`
- Python 后端：`127.0.0.1:18080`
- Prometheus：`127.0.0.1:19090`，同时入口的 `/metrics` 也可用

所有端口可配置。启动时先解析/绑定监听，端口被占用会直接失败，不会静默换端口。
首次启动自动生成仅用于本机测试的自签名证书，也可通过配置替换。

启动方式：

```bash
go build -o edge-gateway ./cmd/edge-gateway
python3 backend/backend.py --port 18080
./edge-gateway -config edgegateway.json
```

`edge-gateway -init` 可生成默认配置文件。

## 业务协议

入口统一使用 HTTP 语义，QUIC 与 TCP 共享同一套业务处理。

### 建立或复用会话

`POST /v1/sessions`

```json
{
  "device_id": "dev-001",
  "connection_id": "stable-logical-connection-id",
  "resume_token": "optional"
}
```

响应包含 `session`、`session_id`、`resume_token`、`reused`、`migrated`。
同一设备在 QUIC/TCP 各自只有一个活动会话；重复建立返回原会话引用。

### 地址切换后接续

`POST /v1/sessions/attach`

```json
{
  "device_id": "dev-001",
  "resume_token": "v1..."
}
```

恢复令牌使用持久化的 HMAC 密钥签名，入口重启后仍有效。接续前后
`ack_through`、`accepted_ranges`、迁移计数和已生效限速均不变。

### 上行分片

`POST /v1/fragments`

```json
{
  "session_id": "ses_...",
  "device_id": "dev-001",
  "seq": 42,
  "size": 1024,
  "data_base64": "...",
  "checksum_sha256": "..."
}
```

响应始终带 `accepted_ranges`、`ack_through`、`backend_backlog`、
`send_window_used`、`send_window_total` 和 `feedback`。

关键业务码：

| HTTP | 业务码 | 含义 |
| --- | --- | --- |
| 200 | `OK` | 已确认；重复分片返回 `duplicate=true`，不再访问后端 |
| 422 | `BACKEND_FRAGMENT_REJECTED` | 后端拒绝，带 `rejected_seq` 和原因，会话暂停 |
| 409 | `SESSION_BACKEND_REJECTION_PAUSED` | 拒绝未恢复，后续分片停止下发 |
| 429 | `RATE_LIMITED_BACKEND_BACKPRESSURE` | 后端背压限速生效 |
| 429 | `SEND_WINDOW_EXHAUSTED` | 单会话在途分片达到发送窗口 |
| 503 | `SESSION_LIMIT_REACHED` | 全局活动会话达到上限 |
| 504 | `BACKEND_TIMEOUT` | 后端超时，可重试同一分片 |

### 查询与运维

- `GET /v1/sessions?active=true`
- `GET /v1/sessions/{id}`：会话、迁移、反馈、拒绝明细
- `GET /v1/sessions/{id}/migrations`
- `GET /v1/sessions/{id}/feedback`
- `GET /v1/sessions/{id}/rejections`
- `POST /v1/sessions/{id}/resume`：清除拒绝暂停，仅在确认后端已恢复后调用
- `POST /v1/sessions/{id}/complete`
- `GET /healthz`
- `GET /metrics`

## 迁移与 QUIC 版本边界

必须明确：**quic-go v0.42 不支持 QUIC 传输层连接迁移**，其源码固定发送
`disable_active_migration=true`。因此不能把四元组变化后的新 UDP 路径伪装成
原传输连接继续运行。

本实现采用业务会话连续方案：

1. 设备持有稳定的 `connection_id` 与签名 `resume_token`。
2. 地址变化后重建 QUIC/TCP 传输连接，入口按设备标识 + 逻辑连接标识/令牌
   找到原会话。
3. 已确认范围、迁移计数、后端转发关系和限速状态从 SQLite 恢复。
4. 已确认分片在入口直接返回原确认，不再次访问后端。

这保证业务结果中的“不重传已确认分片、进度连续、限速不被清零”；但报告会明确
标记 `transport_rebuilt=true`，不声称底层 QUIC 连接本身未重建。

地址判定使用地址的 host 部分；迁移窗口默认 8 秒。生产配置应将
`allow_peer_override` 设为 `false`，此时使用 QUIC `ConnContext` 的实时
`RemoteAddr` 或 TCP 对端地址。场景工具在本地回环上通过
`X-Edge-Peer-Address` 注入 Wi-Fi/4G 地址切换。

## 背压与反馈

每个会话独立维护：

- 最近 16 个后端 RTT 样本，阈值判断使用最近 8 个样本的中位数
- 当前/峰值后端在途条数
- 最大在途发送窗口占用
- 后端超时次数

当某会话 `backend_backlog > backlog_threshold` 或最近 RTT 中位数超过
`rtt_threshold_ms` 时，入口为该会话设置 `rate_limit_per_sec`，后续请求返回
429 + `Retry-After`，反馈来源固定记录为 `backend_backpressure`。
QUIC/TCP 样本不混算，也不使用其他会话状态。

迁移窗口内跳过新的阈值判断并沿用迁移前的限速；窗口结束后的新反馈到达前，
旧限速继续有效。连续 `feedback_recovery_samples` 个低于阈值的样本后才恢复。

后端拒绝时，入口在同一事务中：

1. 增加失败/拒绝计数；
2. 记录拒绝原因、分片号和后端业务码；
3. 设置暂停分片；
4. 不回退任何已确认范围。

重试成功会记录 `recovered_at_unix_nano`，成功分片只计一次。

## SQLite

使用纯 Go `modernc.org/sqlite`，Windows 构建无需 CGO。WAL、单写连接、
5000 ms busy timeout 和固定会话/窗口上限控制资源。

表：

- `sessions`
- `fragments`
- `migrations`
- `feedback_events`
- `rejections`
- `meta`（持久化恢复令牌 HMAC 密钥）

确认分片、会话确认范围与计数在同一事务提交；迁移与会话地址/计数同事务；
反馈与会话限速状态同事务；拒绝与暂停/失败计数同事务。

入口重启后，活动会话和确认范围继续使用；已确认分片不会重新转发；完成会话
不会被同名设备覆盖。

## 固定种子场景

`cmd/loadgen` 使用 `-seed` 确定：

- 24 台设备，QUIC/TCP 各 12 台
- 每台 20–120 分片/秒
- 每个分片 512–4096 字节，内容由 seed/device/seq 的 SHA-256 派生
- 约 1/3 设备执行两次地址切换（Wi-Fi → 4G → Wi-Fi）
- 指定设备注入精确分片号拒绝
- 全局注入后端处理延迟
- 对指定会话执行 6 路并发突发，使积压超过阈值但不超过 8 路发送窗口

场景结束后报告包含：

- 各入口会话数、迁移次数
- 迁移后确认范围保留会话比例
- 迁移时限速保留比例
- 各会话每次限速的生效时刻、依据、积压和 RTT 中位数
- 成功、失败、拒绝、重复、限速、超时分片数
- 后端积压峰值、发送窗口峰值
- 入口转发处理 P99（含注入的后端延迟，按请求开始到入口响应测量）
- 全局并发上限、超限 HTTP 状态和业务码
- 协商协议（HTTP/3、HTTP/2）

`scripts/check_report.py` 会再次读取 SQLite，逐会话核对报告中的成功数、
迁移数、确认范围、确认进度、积压峰值、窗口峰值、反馈记录与拒绝恢复记录。

## 5 秒场景结果

本工作区最后一次 5 秒、seed 309、进程 CPU 亲和性限制为 2 个逻辑 CPU
（`taskset -c 0,1`，`GOMAXPROCS=2`）的场景结果：

| 指标 | QUIC | TCP |
| --- | ---: | ---: |
| 会话数 | 12 | 12 |
| 迁移次数 | 8 | 8 |
| 确认范围保留比例 | 1.0 | 1.0 |
| 迁移时限速保留比例 | 1.0 | 1.0 |
| 成功/失败分片 | 726 / 1 | 703 / 1 |
| 限速事件 | 13 | 13 |
| 限速请求 | 176 | 177 |
| 后端积压峰值 | 6 | 6 |
| 转发 P99 | 186.31 ms | 186.04 ms |
| 协商协议 | HTTP/3 | HTTP/2 |

全局活动会话 24，超限测试返回 HTTP 503 /
`SESSION_LIMIT_REACHED`；后端重复接收分片为 0；两处注入拒绝均恢复。

该表是当前工作区实际运行值，报告 `runner.affinity_cpus=2`。Windows 可执行
文件已交叉编译；若验收机是
Windows 2 核/1.25 GiB，请直接用 `scripts/run_windows.ps1` 复跑，报告会记录
实际平台、CPU 亲和性和耗时。当前 WSL 环境不是原生 Windows，不能把上述数值
冒充 Windows 实机性能数据。

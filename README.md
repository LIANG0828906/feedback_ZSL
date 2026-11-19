# feedback_ZSL：HTTP/3 + HTTP/2 双栈反向代理验证服务

代理同时监听同一端口的 HTTP/3（UDP，quic-go v0.45）和 HTTP/2（TCP + TLS，
x/net/http2 帧层），按配置路由到三个仅支持 HTTP/1.1、HTTP/2、h2c 的本地上游。

工程包含三部分：

- `cmd/gateway`：双栈网关、优先级调度、重试/幂等/健康检查、Prometheus 与 JSON 状态。
- `cmd/upstream`：本地三协议内容服务，支持健康摘除、慢响应、单次/持续断连和统计查询。
- `cmd/loadgen`：固定速率混合负载、一致性/优先级/迁移/健康/幂等探针，输出 JSON 报告。

## 快速运行

Linux/WSL：

```bash
export PATH=/home/liang/go-sdk/bin:$PATH
export GOTOOLCHAIN=local
export GOMAXPROCS=2

go test ./...
go build -o bin/gateway ./cmd/gateway
go build -o bin/upstream ./cmd/upstream
go build -o bin/loadgen ./cmd/loadgen

./scripts/run-demo.sh
```

Windows PowerShell：

```powershell
$env:GOMAXPROCS = "2"
.\scripts\run-demo.ps1
```

脚本只停止自己启动的进程。默认端口冲突时，同时修改
`configs/gateway.json`、`configs/scenario.json` 和启动参数。

| 用途 | 默认地址 |
| --- | --- |
| 网关 HTTP/2 TLS | `127.0.0.1:18444` |
| 网关 HTTP/3 QUIC | `127.0.0.1:18444` |
| 管理/指标 | `127.0.0.1:19091` |
| 上游 HTTP/1.1 | `127.0.0.1:18082` |
| 上游 TLS HTTP/2 | `127.0.0.1:18083` |
| 上游 h2c | `127.0.0.1:18084` |

## 协议语义

- H2 入口由 `internal/h2server` 直接处理 HTTP/2 帧，可读取真实 `PRIORITY`、
  `HEADERS`、`DATA`、`RST_STREAM` 和流 ID，不是读取合成头部。
- H3 入口使用 quic-go 的真实 HTTP/3 请求流。RFC 9218 `Priority: u=0..7`
  与 `X-Gateway-Priority` 类别均映射到内部优先级。
- 方法、路径、query、Host、Content-Type、User-Agent、Accept、Priority、
  业务自定义头和请求体原样转发；仅移除 hop-by-hop 头，并追加
  `X-Forwarded-For`、`X-Forwarded-Proto`、`X-Gateway-Entry-Protocol`。
- 同一路径通过 H2/H3 入口进入同一路由时，上游选择相同，状态码和响应体一致。

## HTTP/3 迁移边界

`quic-go v0.45.0` 服务端固定发送 `disable_active_migration=true`，源码也说明
尚不支持连接迁移。本工程不伪造“原生迁移成功”：

- H3 入口仍使用真实 QUIC/UDP 和 HTTP/3 流。
- 样本用相同 `X-Gateway-Session-ID`、不同 UDP 源地址/连接发起续接请求；
  网关记录 `observed/succeeded/failed` 迁移事件。
- 已建立请求继续完成、续接请求成功、未完成数为 0 时，报告标记迁移成功。
- `/__gateway/status` 明确返回 `transport_migration_supported: false` 及原因。
- v0.45 公开 API 未暴露 H3 `PRIORITY_UPDATE` 扩展帧；H3 优先级通过 RFC 9218
  `Priority` 头映射，并记录调度结果。

若验收要求“原生 QUIC 主动迁移必须成功”，需要升级到支持迁移的 quic-go 版本并
重新做线级验证。当前实现选择遵守任务给定的 v0.45 基线，不掩盖能力缺口。

## 优先级调度

请求先进入全局优先级调度器，再进入目标上游的并发槽优先级调度器：

```text
effective_rank = base_rank - waited / aging_interval
```

- 高优先级先获得下一个上游转发槽。
- 低优先级随等待时间老化，最终可推进，不会因严格优先长期饥饿。
- 已占用连接上的在途请求不被抢占；高优先级只争下一个槽。
- `gateway_priority_wait_seconds` 记录入口协议和优先级类别的 P50/P95/P99。
- `/__gateway/status` 返回所有已配置优先级队列深度（含零值）。

## 重试、副作用与幂等

- 仅无 body 的 `GET/HEAD/OPTIONS` 可自动重试。
- 重试仅发生在尚未收到上游响应头时，记录原因和断开阶段。
- 写请求一旦转发即不自动重试；失败返回 `502`、
  `UPSTREAM_WRITE_AMBIGUOUS` 和 `phase`。
- 下游取消或 RST_STREAM 后，排队请求直接丢弃；已转发请求 context 被取消，
  不进入重试路径。
- 写请求带 `Idempotency-Key` 时缓存完整业务响应。相同 key 后续请求返回
  `X-Gateway-Idempotent-Replay: true`，不会再次到达上游；未知写结果缓存同一错误。
- 健康失败候选不承接新请求；恢复后重新加入。响应头阶段的单次断连不会把整个
  上游永久标记为不健康。

| code | 含义 |
| --- | --- |
| `PROTOCOL_HEADER_TOO_LARGE` | 头部超过配置上限 |
| `PROTOCOL_INVALID_HEADER` | 非法伪头、连接头、重复伪头等 |
| `PROTOCOL_MISSING_REQUIRED_HEADER` | 缺少 Host 或配置要求头 |
| `PROTOCOL_INVALID_TRAILER` | trailer 含伪头或禁止字段 |
| `REQUEST_BODY_TOO_LARGE` | 请求体超限 |
| `DOWNSTREAM_CANCELED` | 下游取消，未继续转发 |
| `PRIORITY_QUEUE_TIMEOUT` | 排队超过请求 deadline |
| `UPSTREAM_UNAVAILABLE` | 路由候选全部健康失败 |
| `UPSTREAM_READ_FAILED` | 安全读取重试耗尽 |
| `UPSTREAM_WRITE_AMBIGUOUS` | 写请求断连，禁止自动重试 |
| `UPSTREAM_RESPONSE_ABORTED` | 已开始响应后 body 中断 |

## 指标与状态

Prometheus 文本：`GET http://127.0.0.1:19091/metrics`。

- `gateway_entry_requests_total{entry,priority}`
- `gateway_entry_errors_total{entry,code}`
- `gateway_entry_active_streams{entry}`
- `gateway_upstream_requests_total{upstream,protocol}`
- `gateway_upstream_errors_total{upstream,protocol,phase}`
- `gateway_upstream_retries_total{upstream,protocol,reason}`
- `gateway_upstream_active_requests{upstream,protocol}`
- `gateway_priority_wait_seconds{entry,priority,quantile}`
- `gateway_priority_queue_depth{priority}`
- `gateway_h3_connection_migrations_total{result}`

JSON 状态：`GET /__gateway/status`（兼容 `/status`），包含配置 revision、
实际绑定地址、上游健康、队列深度、调度等待分位、最近协议错误的入口/流标识/
稳定 code、迁移计数和能力说明。

## 配置与原子切换

示例配置为 `configs/gateway.json`。启动前完整校验：

- 负超时、越界重试、重复优先级 rank/urgency/weight 区间、重复 ID、
  未知上游、重复路由前缀和非法 URL 均拒绝启动。
- 错误配置以 JSON 稳定 code 写 stderr，退出码 `78`，此时尚未绑定端口。
- `-check` 只校验配置，不监听端口。

热切换：`POST /__gateway/reload`；Unix 也支持 `SIGHUP`。新配置先完整构建，
成功后一次性替换运行时快照：

- 在途请求继续使用旧路由；
- 新请求立刻使用新路由；
- 重试使用请求自己的同一快照；
- 不会新旧路由混杂或同一请求被切换转发两次；
- `listen_*` 变更需重启，reload 返回 `CONFIG_LISTEN_CHANGE_REQUIRES_RESTART`。

## 负载与故障注入

`configs/scenario.json` 控制固定速率、H3 比例、模板和上游统计。默认样本包含：

- 双入口混合 GET/POST；
- `/fault/slow?slow_ms=60` 慢响应；
- `X-Fault-Close: once` 单次断连，验证 GET 重试；
- `X-Fault-Close: always` 写断连，验证禁止自动重试；
- 8 个低优先级慢请求 + 延后高优先级，验证优先级和不饥饿；
- 健康摘除/恢复、幂等重放、H3 会话续接探针。

上游运维接口：

```text
GET /__health
GET /__stats
GET /__control/health?value=down
GET /__control/health?value=up
GET /__control/reset
```

报告默认写入 `artifacts/scenario-report.json`，包含双入口延迟分位、优先级等待、
重试原因、拒绝码、迁移成功数，以及三个上游实际收到的请求摘要。

## 验收命令

```bash
export PATH=/home/liang/go-sdk/bin:$PATH
export GOTOOLCHAIN=local
export GOMAXPROCS=2

go test ./...
go vet ./...
./bin/gateway -check -config configs/gateway.json
./scripts/run-demo.sh
```

报告断言：

```text
consistency[*].equivalent == true
priority_probe.high_priority_advanced == true
priority_probe.no_starvation == true
migration.success == 1
migration.unfinished_dropped == 0
failover.success == true
idempotency.success == true
rejections.UPSTREAM_WRITE_AMBIGUOUS > 0
retry_reasons.response_headers > 0
```

## 资源占用

默认场景按约束运行：`GOMAXPROCS=2`、20 req/s、4 秒负载、并发上限 16；
单网关、三上游、单管理端口；不启动数据库或额外集群。完整样本约 7 秒。

## 测试覆盖

`internal/h2server` 直接发送线级 HTTP/2 帧，覆盖真实 PRIORITY 帧、坏流与后续
正常流隔离、超大头部 431、非法 trailer 在转发前拒绝。其他测试覆盖配置拒绝、
重试边界、取消不下发、幂等重放、高优先级推进与非饥饿、双栈响应等价及必需头一致性。

# 轻量配置中心：变更风暴下的通知合并与一致性收敛

本项目从零实现一个单机轻量配置中心，重点解决“同一命名空间短时间内大量变更时，客户端收到中间版本或部分变更集合”的问题。服务端使用 Go 1.22 编写，内嵌纯 Go SQLite 驱动 `modernc.org/sqlite`（实测 SQLite 3.45.3），不依赖外部数据库或消息系统。

## 实现概览

运行拓扑被压缩为：

- 1 个配置中心服务进程：HTTP/1.1 API、SQLite 版本链、通知合并器、指标与状态接口。
- 3 个客户端模拟业务进程：各自维护本地配置、应用版本和过期响应计数。
- 0 个外部数据库、0 个消息中间件、0 个额外协调服务。

目录：

```text
cmd/configcenter/       服务、客户端、压力器入口
internal/store/         SQLite 原子发布、版本历史、快照
internal/notify/        合并窗口、发布屏障、长轮询唤醒
internal/server/        HTTP API、指标和 JSON 状态
internal/business/      客户端单调应用状态与业务热加载模拟
internal/stress/        固定种子变更风暴及端到端自检
internal/seed/          23 个订单配置项与固定种子生成器
scripts/                Windows / Linux 启动与演示脚本
```

## 一致性协议

### 1. 原子发布与版本链

- 每次发布在一个 SQLite 事务内完成：写 `config_items` 当前值、追加 `config_history` 版本链、递增命名空间 `current_version`。
- 同一批提交中的所有配置项共享同一个新版本号；任一项校验失败时事务不提交，版本号与历史都不变化。
- 版本号按命名空间单调递增，初始内置配置为版本 `1`。

### 2. 合并窗口与广播屏障

- 通知器按命名空间维护待广播项和原始变更计数。
- 每次成功发布后重置长度为 `merge-window` 的去抖定时器，默认 `500ms`，服务端强制其不小于 `200ms`。
- 连续风暴最多等待 `merge-max-wait`（默认 `2s`）后必须形成一个合并边界，避免无限延期。
- 发布提交和通知记入共用一把屏障锁；合并刷新在同一屏障内读取数据库最新版本。因此不会出现“版本已落库但该次变更未进入待广播集合”的竞态。
- 一个合并窗口只生成一次广播；窗口内同一配置项被多次修改时，只保留最终值。
- 指标在广播边界对等待客户端可见之前更新，客户端看到新版本时计数器已经一致。

### 3. 长轮询返回规则

- 请求携带 `version=<客户端已知版本>`。
- 若已知版本大于数据库最新版本，返回 HTTP `409` 和稳定业务错误码 `E_VERSION_AHEAD`。
- 若请求到达时已有新提交但尚未越过合并边界，长轮询继续等待该边界，而不是返回更早的广播。
- 返回内容只来自已完成的合并边界，包含从客户端已知版本到响应版本之间所有发生变化的配置项，每项带最终值和该项版本号。
- 查询按 `item version ASC, key ASC` 排序，不返回部分集合。
- 超时且确实没有变化时返回 `changed=false`、空 `changes` 和当前版本。
- 若超时正好落在待广播窗口内，会延长至该次广播完成，避免产生“新版本号 + 空变更”的错误收敛。

### 4. 客户端单调应用

- 客户端维护已应用版本、完整本地配置和历史应用版本列表。
- 响应版本小于本地版本时忽略并记录 `stale`，不会回退已知版本。
- 响应必须以本地已应用版本为 `base_version`，且每项变更版本都必须落在 `(base_version, response.version]`。
- 客户端先校验整个响应，再在影子配置上按项版本递增应用，全部成功后才原子替换本地配置并提升应用版本。
- 非法或部分响应不会造成半应用状态。

## 启动

环境要求：Go 1.22+、GOPATH 已配置。SQLite 由纯 Go 驱动嵌入，无需安装 SQLite 命令行工具。

### Windows 10/11

在 PowerShell 中执行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\start-demo.ps1
```

默认监听 `127.0.0.1:18473`。可指定端口或保持进程运行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\start-demo.ps1 -Port 18573 -KeepRunning
```

脚本会：

1. 构建 `bin\configcenter.exe`。
2. 启动 1 个服务进程。
3. 启动 3 个独立客户端进程 `client-1`、`client-2`、`client-3`。
4. 运行固定种子压力器并校验最终收敛。
5. 输出 JSON 状态、Prometheus 指标和客户端应用日志。

### Linux / WSL

```bash
./scripts/start-demo.sh
PORT=18573 MERGE_WINDOW=600ms ./scripts/start-demo.sh
```

### 手动启动

```bash
go build -trimpath -o bin/configcenter ./cmd/configcenter

bin/configcenter server \
  --addr 127.0.0.1:18473 \
  --db data/configcenter.db \
  --merge-window 500ms \
  --merge-max-wait 2s

bin/configcenter client --base-url http://127.0.0.1:18473 --client-id client-1
bin/configcenter client --base-url http://127.0.0.1:18473 --client-id client-2
bin/configcenter client --base-url http://127.0.0.1:18473 --client-id client-3

bin/configcenter stress --base-url http://127.0.0.1:18473 --clients 3
```

`server` 也支持环境变量 `CONFIGCENTER_ADDR`、`CONFIGCENTER_DB`；客户端和压力器支持 `CONFIGCENTER_URL`。

## 固定种子变更风暴

内置生成器固定种子为 `20261119`，恰好产生 23 个订单配置项：

- 初始版本含 23 项，`order.timeout_ms=800`。
- 压力器在同一秒内按固定伪随机顺序执行 23 次单配置项原子发布，形成版本 `2..24`。
- `order.timeout_ms` 在版本 `22` 变为 `1200`，但该中间版本不会到达业务进程。
- 合并窗口结束后，3 个客户端都只从版本 `1` 直接应用版本 `24`。

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/healthz` | 健康检查，同时返回 SQLite 版本 |
| `GET` | `/v1/namespaces/{ns}/current` | 当前最新版本 |
| `GET` | `/v1/namespaces/{ns}/watch?version=N&timeout=10s` | 长轮询 |
| `POST` | `/v1/namespaces/{ns}/publish` | 原子批量发布 |
| `GET` | `/v1/namespaces/{ns}/snapshots/{version}` | 指定版本完整配置快照 |
| `POST` | `/v1/clients/report` | 客户端应用版本与过期响应上报 |
| `GET` | `/v1/status` | JSON 状态 |
| `GET` | `/metrics` | Prometheus 文本指标 |

批量发布示例：

```json
{
  "changes": [
    {"key": "order.timeout_ms", "value": "1200"},
    {"key": "order.retry.max_attempts", "value": "4"}
  ]
}
```

成功响应：

```json
{
  "namespace": "orders",
  "version": 25,
  "changed_keys": ["order.retry.max_attempts", "order.timeout_ms"],
  "atomic": true
}
```

长轮询响应：

```json
{
  "namespace": "orders",
  "base_version": 1,
  "version": 24,
  "changed": true,
  "changes": [
    {"key": "order.batch.flush_ms", "value": "40", "version": 18},
    {"key": "order.timeout_ms", "value": "1200", "version": 22}
  ]
}
```

快照接口返回指定版本下每个配置项当时的最新值及该项版本号，是完整快照而不是差量。

校验失败返回稳定结构：

```json
{
  "code": "E_VALIDATION",
  "message": "change 23 has an empty key"
}
```

版本超前返回：

```json
{
  "code": "E_VERSION_AHEAD",
  "message": "known version is greater than the current latest version"
}
```

## Prometheus 指标

指标名与命名空间标签保持稳定：

```text
config_long_poll_requests_total{namespace="orders"}
config_merged_broadcasts_total{namespace="orders"}
config_merged_raw_changes_total{namespace="orders"}
config_client_applied_version{namespace="orders",client_id="client-1"}
config_client_stale_responses_total{namespace="orders",client_id="client-1"}
config_client_applied_version_distribution{namespace="orders",version="24"}
```

- `config_merged_raw_changes_total` 统计被折叠的原始配置项变更次数，重复修改同一项也按原始次数计数。
- `config_client_applied_version_distribution` 是当前各应用版本的客户端数量。
- 所有指标按命名空间、客户端 ID、版本稳定排序。

## JSON 状态

`GET /v1/status` 返回：

- 每个命名空间的最新版本。
- 已上报客户端的已知版本、应用版本、过期响应数、最近应用历史。
- 最近一次合并广播的版本、原始变更数、窗口时间和最终变更项摘要。

命名空间、客户端、变更项均稳定排序；响应不包含数据库路径或其他绝对路径。

## 验证

自动化测试：

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试覆盖：

- 批量发布共享一个版本、版本链和完整历史快照。
- 重复/空键等非法批次整体回滚，不产生版本和历史。
- 3 个并发长轮询只收到最终版本 `24` 和全部 23 个变更项。
- 不返回更早广播：有新提交未合并时，长轮询等待最新边界。
- 客户端拒绝回退版本，非法响应不会半应用。
- `E_VERSION_AHEAD`、超时空结果、Prometheus 指标和状态排序。

真实三客户端运行结果摘要：

```json
{
  "result": "PASS",
  "namespace": "orders",
  "initial_version": 1,
  "final_version": 24,
  "merged_broadcast_count": 1,
  "raw_changes_merged": 23,
  "clients": [
    {
      "client_id": "client-1",
      "known_version": 24,
      "applied_version": 24,
      "stale_responses": 0,
      "applied_history": [1, 24]
    },
    {
      "client_id": "client-2",
      "known_version": 24,
      "applied_version": 24,
      "stale_responses": 0,
      "applied_history": [1, 24]
    },
    {
      "client_id": "client-3",
      "known_version": 24,
      "applied_version": 24,
      "stale_responses": 0,
      "applied_history": [1, 24]
    }
  ],
  "rollback": {
    "error_code": "E_VALIDATION",
    "version_before": 24,
    "version_after": 24
  },
  "snapshot": {
    "intermediate_version": 2,
    "item_count": 23,
    "timeout_value": "1200"
  },
  "timeout_poll": {
    "version": 24,
    "changed": false,
    "changes": 0
  },
  "version_ahead_error": "E_VERSION_AHEAD"
}
```

对应的 Prometheus 实测结果：

```text
config_long_poll_requests_total{namespace="orders"} 11
config_merged_broadcasts_total{namespace="orders"} 1
config_merged_raw_changes_total{namespace="orders"} 23
config_client_applied_version{namespace="orders",client_id="client-1"} 24
config_client_applied_version{namespace="orders",client_id="client-2"} 24
config_client_applied_version{namespace="orders",client_id="client-3"} 24
config_client_stale_responses_total{namespace="orders",client_id="client-1"} 0
config_client_stale_responses_total{namespace="orders",client_id="client-2"} 0
config_client_stale_responses_total{namespace="orders",client_id="client-3"} 0
config_client_applied_version_distribution{namespace="orders",version="24"} 3
```

客户端日志中的关键行只有：

```text
APPLIED client=client-1 version=1 base=0 changes=23
APPLIED client=client-1 version=24 base=1 changes=23
APPLIED client=client-2 version=1 base=0 changes=23
APPLIED client=client-2 version=24 base=1 changes=23
APPLIED client=client-3 version=1 base=0 changes=23
APPLIED client=client-3 version=24 base=1 changes=23
```

没有版本 `2..23` 的应用记录，最终 3 个客户端均为版本 `24`，失败发布前后版本均为 `24`。

## 资源边界

- 服务端使用 `GOMAXPROCS=2`、`GOMEMLIMIT=900MiB`；Linux 脚本给三个模拟客户端设置 `GOMAXPROCS=1`，客户端主要处于长轮询 I/O 等待。
- SQLite 使用 WAL 模式，数据库、WAL 和日志均位于相对目录 `data/`、`tmp/`，运行期磁盘占用为 MB 级。
- 不启动 etcd、Kafka、Redis、外部 SQLite 服务或其他重量级组件。
- 本项目定位单机一致性演示与轻量边缘配置中心，不宣称具备多节点 Raft 或跨机故障转移。

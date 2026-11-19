# Webhook 租户隔离分发实验

在单节点 kind 集群中实现 20 租户 Webhook 分发协调：优先级调度、每租户并发额度、重试/死信、慢租户降级隔离、Prometheus/JSON 可观测性，以及 SQLite 持久化恢复。

## 架构

```text
                     +-----------------------------+
 Producer ---------> | webhook-dispatcher (x2)     |
                     | HTTP API + scheduler        |
                     +--------------+--------------+
                                    |
                         SQLite WAL (hostPath)
                 events / config / dead_letters / state
                                    |
                  +-----------------+-----------------+
                  | priority-aware atomic claim       |
                  | per-tenant global concurrency cap |
                  | lease + idempotency key           |
                  +-----------------+-----------------+
                                    |
             20 Receiver Services (python:3.11-slim)
             /webhook  /health  /live  /control  /stats
```

- 两个分发实例使用同一 SQLite 文件（单节点 kind 的 `hostPath`），通过 `BEGIN IMMEDIATE` 原子领取事件。租约写入数据库，因此两个实例共享每租户的全局在途计数。
- 调度顺序为 `high > medium > low`；同优先级内按租户在途数/并发额度比例和最近调度时间做公平轮转。
- 达到并发额度的租户不会出现在候选查询中，不会阻塞其他租户。
- 接收方以 `Idempotency-Key` 和 `event_id` 去重。传输语义为 at-least-once，业务处理语义为 effectively-once：协调器若在“HTTP 成功、SQLite 提交前”崩溃，会重新投递同一 `event_id`，Receiver 只接受一次，协调器也只记录一次成功。

## 目录

```text
app/                      协调器、配置、SQLite、调度、API、指标
receiver/server.py        可注入延迟/5xx/4xx 的 Receiver
loadgen/generate.py       固定速率负载与故障注入
deploy/tenants.json       20 租户 ConfigMap 内容
deploy/dispatcher.yaml    两副本协调器与 Service
deploy/quota.yaml         命名空间资源请求预算
deploy/scenario-*.json    慢租户与重启验收场景
scripts/                  kind 部署、场景运行、结果检查、清理
tests/                    配置、调度、恢复和 API 自动化测试
```

## 租户配置

配置位于 `deploy/tenants.json`，由部署脚本创建为 ConfigMap `tenant-config`，挂载到 `/etc/webhook-lab/tenants.json`。

```json
{
  "tenant_id": "tenant-00",
  "priority": "high",
  "max_concurrency": 10,
  "max_retries": 2,
  "backoff_base_ms": 100,
  "timeout_ms": 1000,
  "degrade_after_timeouts": 3,
  "receiver_url": "http://receiver-tenant-00.webhook-lab.svc.cluster.local:8080"
}
```

- `priority`：仅允许 `high`、`medium`、`low`。
- `max_concurrency`：大于 0 的整数，是跨两个协调器实例的全局租户上限。
- `max_retries`：大于等于 0，表示首次请求之外的重试次数。
- `backoff_base_ms`：大于等于 0，第 n 次失败后的等待为 `base * 2^(n-1)`，封顶 60 秒。
- `timeout_ms`：单次 HTTP POST 超时。
- `degrade_after_timeouts`：大于等于 0；连续超时数严格大于该值时进入降级。

配置在创建数据库或启动调度前完整解析。任一租户非法都会拒绝启动，并输出稳定 JSON 业务错误，例如：

```json
{"business_code":"CFG_PRIORITY_INVALID","message":"tenant tenant-00 priority must be one of high, medium, low","tenant_id":"tenant-00"}
```

错误码包括：`CFG_FILE_MISSING`、`CFG_FILE_UNREADABLE`、`CFG_JSON_INVALID`、`CFG_ROOT_INVALID`、`CFG_TENANTS_INVALID`、`CFG_TENANT_INVALID`、`CFG_TENANT_ID_INVALID`、`CFG_DUPLICATE_TENANT`、`CFG_PRIORITY_INVALID`、`CFG_CONCURRENCY_INVALID`、`CFG_RETRIES_INVALID`、`CFG_BACKOFF_INVALID`、`CFG_TIMEOUT_INVALID`、`CFG_DEGRADE_THRESHOLD_INVALID`、`CFG_RECEIVER_URL_INVALID`。

## 分发语义

1. `/v1/events` 或 `/v1/events/batch` 在返回前同步提交 SQLite（WAL + `synchronous=FULL`），已接收事件不会停留在进程内存。
2. 协调器在事务内检查所有租户在途数，按优先级领取一个事件，并写入 5 秒租约。
3. 2xx 成功只提交一次；5xx、超时和连接错误按租户配置退避重试。
4. 重试耗尽或 4xx 时，事件在同一事务内进入 `dead_letters`，记录租户、事件 ID、原因、尝试次数和永久失败标记。
5. 连续超时超过阈值后，租户被标记为 `degraded`。该租户的待处理事件保留在 SQLite，但调度 SQL 不再选择该租户。
6. 健康探针每轮请求降级租户的 `/health`；连续成功后解除降级，等待事件按原并发上限重新调度。
7. 协调器重启后，过期租约自动回到待调度状态；已完成事件保持 `delivered`，不会再次领取。

## API 与指标

| 路径 | 用途 |
|---|---|
| `POST /v1/events` | 写入单个事件 |
| `POST /v1/events/batch` | 原子批量写入 |
| `GET /v1/events/{event_id}` | 查询事件状态 |
| `GET /v1/dead-letters?limit=100` | 查询死信 |
| `GET /v1/status` | 每租户队列深度、在途数、降级状态、最后分发时间 |
| `GET /v1/report` | 稳定排序的验收报告，不含时间戳 |
| `GET /metrics` | Prometheus 文本指标 |
| `GET /healthz` | 进程健康并暴露降级租户列表 |
| `GET /readyz` | SQLite 就绪检查 |
| `POST /v1/tenants/{id}/control` | 代理 Receiver 故障注入 |
| `GET /v1/receiver-stats/{id}` | 代理 Receiver 去重计数 |
| `POST /v1/tenants/{id}/recover` | 运维手动解除降级 |

Prometheus 指标名和标签固定为：

```text
webhook_dispatch_success_total{tenant="..."}
webhook_dispatch_failure_total{tenant="..."}
webhook_dispatch_retry_total{tenant="..."}
webhook_dispatch_dead_letter_total{tenant="..."}
webhook_dispatch_inflight{tenant="..."}
webhook_tenant_queue_depth{tenant="..."}
webhook_tenant_degraded{tenant="..."}
```

## 本地验证

以下命令在 WSL2 或 Git Bash 中执行。假设已有 kind 集群和 `kind-webhook-lab` 上下文；脚本不会创建集群。

```bash
cd /path/to/repo
kubectl config use-context kind-webhook-lab

# 构建镜像、加载到 kind、创建 webhook-lab、20 个 Receiver、2 个协调器
./scripts/deploy_kind.sh

# 固定速率注入健康租户与慢租户，并运行故障时间线
./scripts/run_scenario.sh

# 检查其他租户 P95、慢租户死信与降级持续时间
python3 scripts/verify_report.py artifacts/scenario-report.json --p95-limit-ms 500

# 在持续负载下滚动重启 2 个协调器，核对不丢不重
./scripts/restart_check.sh

# 删除本次 Namespace、负载资源和 hostPath 数据
./scripts/cleanup_kind.sh
```

Windows 上若命令名是 `python`：

```bash
PYTHON=python ./scripts/deploy_kind.sh
PYTHON=python ./scripts/run_scenario.sh
```

`scenario-slow.json` 的时间线：

- `0-2s`：`tenant-17` 100% 慢响应（2.5-3 秒），超过其 500ms 超时，触发降级。
- `2-10s`：Receiver 仍报告不健康，等待队列保持隔离，不发往该租户。
- `10-12s`：故障切换为 35% 5xx，但仍不健康。
- `12-15s`：健康检查恢复，等待队列重新调度；5xx 触发重试并最终产生死信。
- `15s` 后：故障清零，场景排空。

如需复现题目中的峰值输入，可运行全部 20 租户各 `60 事件/秒`、合计 `1200 事件/秒` 的 5 秒场景：

```bash
SCENARIO=$PWD/deploy/scenario-peak.json OUTPUT=$PWD/artifacts/peak-report.json \
  ./scripts/run_scenario.sh
```

报告字段按 JSON key 排序，数组按租户 ID 排序，不含任何时间戳。延迟从事件接收（`created_at`）计算到成功投递（`delivered_at`），包含排队与退避时间。每个租户包含 `success_rate`、`avg_latency_ms`、`p95_latency_ms`、`retries`、`dead_letters`、`degradation_ms`、队列/在途状态等字段。

## 自动化测试

```bash
python3 -m unittest discover -s tests -v
```

覆盖：全部非法配置错误码且不创建数据库、优先级与并发额度、重试、4xx 永久死信、连续超时降级与恢复、重启租约恢复、API、指标稳定名称和报告稳定结构。

## 资源预算

| 组件 | 请求 |
|---|---|
| 20 个 Receiver | `20 * 10m = 200m` CPU，`20 * 16Mi = 320Mi` |
| 2 个协调器 | `2 * 100m = 200m` CPU，`2 * 128Mi = 256Mi` |
| 2 个 init 容器 | `2 * 5m = 10m` CPU，`2 * 8Mi = 16Mi` |
| 合计（含 init） | `410m` CPU，`592Mi` 内存 |

`deploy/quota.yaml` 将命名空间请求总额限制为 1 核、1GiB。单节点 kind 使用 `hostPath=/var/lib/webhook-lab`，两副本访问同一 SQLite 文件；本方案面向题目给定的单节点实验拓扑，不宣称适用于多节点共享存储。

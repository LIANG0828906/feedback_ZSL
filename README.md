# 结算灰度变更编排器

本项目在单台 Windows 机器上复现 6 实例、两组站点的结算灰度发布。实现只依赖
Python 3.11 标准库，不需要数据库服务、消息队列或 Prometheus 服务。

核心设计是“稳定入口 + 可替换版本后端”：

- 每个逻辑实例有一个长期存活的外部 HTTP/1.1 入口。
- 新版本先作为独立后端启动并执行健康预检。
- 预检通过后，入口原子切换代理目标；旧后端继续处理在途请求，排空后退出。
- 回滚使用同一机制。重启被拒绝时，当前版本继续承接请求，只记录
  `INSTANCE_RESTART_REFUSED` 并继续处理其他实例。

因此版本切换和回滚不会先停掉整组实例。

## 快速开始

环境：Windows 10/11 或 Windows Server 2022，Python 3.11；本机回环地址。

```powershell
python -m settlement_rollout validate --plan configs/change-plan.json --slo configs/slo.json
```

运行 3 秒观察窗口的达标场景：

```powershell
python -m settlement_rollout scenario `
  --plan configs/quick/change-plan.json `
  --slo configs/quick/slo.json `
  --injections configs/injections/release-success.json `
  --workdir artifacts/run-success `
  --report artifacts/report-success.json
```

运行 P99/5xx 不达标并全量回滚的场景：

```powershell
python -m settlement_rollout scenario `
  --plan configs/quick/change-plan.json `
  --slo configs/quick/slo.json `
  --injections configs/injections/release-rollback.json `
  --workdir artifacts/run-rollback `
  --report artifacts/report-rollback.json
```

运行包含健康预检失败与回滚重启拒绝的部分回滚场景：

```powershell
python -m settlement_rollout scenario `
  --plan configs/quick/change-plan.json `
  --slo configs/quick/slo.json `
  --injections configs/injections/release-failure.json `
  --workdir artifacts/run-refusal `
  --report artifacts/report-refusal.json
```

两份模板：

- `configs/change-plan.json` 与 `configs/slo.json`：正式 120 秒观察窗口。
- `configs/quick/`：3 秒观察窗口，保持 6 实例、两组批次和相同 SLO 结构，供本地验收。

## 交付物

- `settlement_rollout/config.py`：JSON 配置加载与全量启动前校验。
- `settlement_rollout/supervisor.py`：实例稳定入口、预检、蓝绿切换和请求排空。
- `settlement_rollout/backend.py`：本地版本后端与确定性故障注入。
- `settlement_rollout/orchestrator.py`：批次状态机、观察、判定、推进与回滚。
- `settlement_rollout/sli.py`：可用率、P99、错误预算和触发条件计算。
- `settlement_rollout/store.py`：SQLite 事务、审计、恢复所需状态。
- `settlement_rollout/api.py`：JSON 状态、Prometheus 指标、审计和手工中止 API。
- `settlement_rollout/loadgen.py`：持续回环 HTTP 负载。
- `settlement_rollout/scenario.py`：场景启动、报告生成和期望结果核对。
- `configs/`：变更计划、SLO、正常/回滚/拒绝重启/中止注入模板。
- `tests/`：配置、SLI、事务约束和端到端场景测试。
- `artifacts/report-success.json`、`artifacts/report-rollback.json`、
  `artifacts/report-refusal.json`、`artifacts/report-abort.json`：已执行场景报告。
- `artifacts/acceptance-summary.json`：三类场景的关键 SLI、判定、回滚和传输结果汇总。

## 配置

变更计划主要字段：

```json
{
  "release_id": "settlement-2026.11.19-r1",
  "target_version": "v2",
  "rollback_version": "v1",
  "observation_window_seconds": 120,
  "sampling": {
    "interval_seconds": 2,
    "quota_per_instance": 30
  },
  "instances": [
    {
      "id": "site-a-1",
      "group": "site-a",
      "port": 28701,
      "initial_version": "v1"
    }
  ],
  "batches": [
    {
      "id": "canary-site-a",
      "order": 1,
      "instances": ["site-a-1"],
      "rollback_version": "v1"
    }
  ],
  "network": {
    "host": "127.0.0.1",
    "port_range": [28700, 28799],
    "backend_port_range": [49000, 49999],
    "orchestrator_port": 28799,
    "precheck_timeout_seconds": 1.5,
    "drain_timeout_seconds": 5,
    "request_timeout_seconds": 10
  },
  "storage": {
    "database_path": "state/rollout.sqlite3",
    "report_path": "state/report.json"
  }
}
```

SLO 字段：

```json
{
  "availability": {
    "target_percent": 99.0
  },
  "latency": {
    "percentile": 99.0,
    "target_ms": 80.0,
    "allowed_bad_percent": 1.0
  },
  "minimum_samples": 24,
  "error_budget": {
    "max_consumed_percent": 25.0
  }
}
```

配置会一次性完整校验。任何批次包含未声明实例、批次为空、批次顺序非法、
百分比超出 0..100、观察窗口非正或采样参数非法时，都会在任何实例和数据库
状态创建之前退出。

## SLI 与判定

- 可用率：HTTP 状态 `< 500` 的样本数 / 总样本数。
- P99：固定样本集上的 nearest-rank 99 分位耗时。
- 延迟坏样本：耗时严格大于 `latency.target_ms` 的请求。
- 可用率错误预算消耗：`(1 - 可用率) / (1 - 可用率目标)`。
- 延迟错误预算消耗：`延迟坏样本比例 / allowed_bad_percent`。
- 综合消耗取两者最大值；剩余比例为 `max(0, 1 - 综合消耗)`。
- 样本数小于 `minimum_samples` 直接判定失败，绝不判为达标。
- 即使综合错误预算未超过上限，只要 P99 大于 `target_ms` 也会失败。

为消除墙钟抖动对报告的影响，每个已切换实例在窗口内取前
`sampling.quota_per_instance` 个已完成的目标版本请求；并发延迟由后端按配置
模拟并通过响应头传递，因此同一配置和注入序列的报告除耗时字段外完全一致。

## 状态与审计

`scenario` 生成的数据库位于工作目录的 `state.sqlite3`。以下操作都在同一
SQLite 事务中提交：

- 批次状态迁移与对应事件。
- 判定结果、指标快照、阈值和触发项。
- 逐实例回滚动作、实例版本变更、事件和当时的 SLI 快照。
- 手工中止、中止原因、事件和指标快照。

已进入 `PASSED`、`ROLLED_BACK`、`PARTIAL_ROLLBACK` 等终态的批次不能被回退。
恢复时跳过所有 `PASSED` 批次；已回滚批次不会自动再次推进。

运行期 API 默认监听计划中的 `network.orchestrator_port`：

| 方法 | 路径 | 用途 |
|---|---|---|
| `GET` | `/status` | 批次进度、观察剩余时间、最近判定原因 |
| `GET` | `/metrics` | Prometheus 文本指标 |
| `GET` | `/audit?batch_id=...` | 指定批次的完整决策、事件、快照和回滚序列 |
| `GET` | `/audit` | 所有批次审计 |
| `POST` | `/abort` | 手工中止并回滚；请求体 `{"reason":"..."}` |
| `GET` | `/health` | API 健康检查 |

稳定 Prometheus 指标：

- `settlement_rollout_run_info`
- `settlement_rollout_batch_instance_version_info`
- `settlement_rollout_batch_switched_instances`
- `settlement_rollout_batch_sli_availability_percent`
- `settlement_rollout_batch_sli_p99_latency_ms`
- `settlement_rollout_batch_error_budget_remaining_ratio`
- `settlement_rollout_batch_decisions_total`
- `settlement_rollout_batch_rollbacks_total`
- `settlement_rollout_batch_status_info`

离线查询示例：

```powershell
python -m settlement_rollout status `
  --database artifacts/run-rollback/state.sqlite3 `
  --run-id <run_id>

python -m settlement_rollout audit `
  --database artifacts/run-rollback/state.sqlite3 `
  --run-id <run_id> `
  --batch canary-site-a

python -m settlement_rollout metrics `
  --plan configs/quick/change-plan.json `
  --slo configs/quick/slo.json `
  --database artifacts/run-rollback/state.sqlite3 `
  --run-id <run_id>
```

## 手工中止

使用 30 秒观察窗口启动场景，在另一个终端读取实际 API 端口后中止：

```powershell
python -m settlement_rollout scenario `
  --plan configs/quick/change-plan-manual-abort.json `
  --slo configs/quick/slo.json `
  --injections configs/injections/release-abort.json `
  --workdir artifacts/run-abort `
  --report artifacts/report-abort.json
```

```powershell
$resolved = Get-Content artifacts/run-abort/resolved-plan.json | ConvertFrom-Json
Invoke-RestMethod `
  -Method Post `
  -Uri "http://127.0.0.1:$($resolved.orchestrator_port)/abort" `
  -ContentType "application/json" `
  -Body '{"reason":"operator stop"}'
```

编排器会停止推进、回滚当前批次已切换实例，并把运行标记为 `ABORTED`。

## 服务模式与恢复

实例已独立运行时，可用 `serve` 启动编排器：

```powershell
python -m settlement_rollout serve `
  --plan configs/change-plan.json `
  --slo configs/slo.json `
  --database artifacts/state/rollout.sqlite3
```

进程中断后使用同一配置和数据库再次执行 `serve`。它会从已提交批次和实例状态
继续：已推进批次跳过，已回滚批次不再推进，处于切换/观察/回滚中的批次只处理
尚未完成的部分。

## 错误码

配置错误退出码为 `2`，运行错误为 `3`，场景期望结果不符为 `4`。

配置错误：

`CFG_FILE_UNREADABLE`、`CFG_JSON_INVALID`、`CFG_SCHEMA_INVALID`、
`CFG_UNKNOWN_INSTANCE`、`CFG_EMPTY_BATCH`、`CFG_DUPLICATE_INSTANCE`、
`CFG_DUPLICATE_BATCH`、`CFG_BATCH_ORDER`、`CFG_TARGET_RANGE`、
`CFG_WINDOW_INVALID`、`CFG_SAMPLE_INVALID`、`CFG_PORT_INVALID`。

运行错误：

`INSTANCE_PRECHECK_FAILED`、`INSTANCE_SWITCH_FAILED`、
`INSTANCE_RESTART_REFUSED`、`INSTANCE_UNAVAILABLE`、
`INSTANCE_STATS_FAILED`、`ROLLBACK_VERIFICATION_FAILED`、
`STATE_CONFIG_MISMATCH`。

## 故障注入

注入文件只修改本地版本后端行为，不修改编排器：

- `latency_ms`：模拟慢响应，例如 `1200`。
- `error_percent`：确定性 5xx 比例，例如 `6`。
- `error_status`：5xx 状态码。
- `health_fail_ms`：后端启动后健康端点持续失败的毫秒数。
- `rollback_restart_refused`：回滚到初始版本时模拟重启拒绝。

`configs/injections/release-failure.json` 同时注入慢响应、5xx、健康端点失败
和一个实例的回滚重启拒绝。

## 测试

```powershell
python -m unittest discover -v
```

测试覆盖配置业务错误码、样本不足、P99 独立触发、错误预算、批次终态约束、
手工中止事务，以及达标发布、全量回滚、预检跳过和重启拒绝两类端到端场景。

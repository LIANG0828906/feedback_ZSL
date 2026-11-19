# 灰度发布编排服务

本项目实现一个面向交易平台的本地灰度发布编排服务。它从样本目录读取发布计划和逐秒实例指标，按批次执行聚合与逐实例双重判定，持久化可复核的决策记录，并提供回滚版本核对、Prometheus 指标和 JSON 状态接口。

核心实现只依赖 Python 3.11+ 标准库和 `prometheus-client`，状态保存在本地 SQLite。

## 快速开始

```bash
python -m pip install -r requirements.txt

# 固定种子生成 4 批 x 12 实例的样本到 samples/
python generate_samples.py --output samples --scenario mixed --seed 20261119 --force

# 生成并运行一个完整故障场景，输出样本与 report.json
python run_scenario.py --scenario mixed --seed 20261119 --output artifacts/demo --force
```

Windows PowerShell 使用相同命令。服务本身不区分操作系统。

启动 HTTP 服务：

```bash
python -m release_orchestrator serve \
  --host 127.0.0.1 \
  --port 8080 \
  --db orchestrator.db \
  --samples samples
```

启动后：

- `GET http://127.0.0.1:8080/healthz`
- `GET http://127.0.0.1:8080/metrics`
- `GET http://127.0.0.1:8080/api/releases`
- `GET http://127.0.0.1:8080/api/releases/{release_id}`
- `GET http://127.0.0.1:8080/api/releases/{release_id}/audit`
- `POST http://127.0.0.1:8080/api/releases`，请求体 `{"sample_dir":"samples"}`
- `POST http://127.0.0.1:8080/api/releases/{release_id}/run`
- `POST http://127.0.0.1:8080/api/releases/{release_id}/evaluate`
- `POST http://127.0.0.1:8080/api/releases/{release_id}/rollback/verify`
- `POST http://127.0.0.1:8080/api/plans/validate`，请求体 `{"plan_path":"samples/plan.json"}`

示例：

```bash
curl -X POST http://127.0.0.1:8080/api/releases \
  -H "Content-Type: application/json" \
  -d '{"sample_dir":"samples"}'

curl -X POST http://127.0.0.1:8080/api/releases/rel-mixed-20261119/run \
  -H "Content-Type: application/json" \
  -d '{}'

curl -X POST http://127.0.0.1:8080/api/releases/rel-mixed-20261119/rollback/verify \
  -H "Content-Type: application/json" \
  -d '{}'
```

`POST /evaluate` 每次只推进一个待判定批次。`POST /run` 持续推进到完成、回滚、占用冲突或可选中止点 `{"max_batches":1}`。

## 样本与输入格式

固定种子生成器输出：

```text
samples/
  plan.json
  metrics.jsonl
  version_reports.jsonl
  scenario.json
```

`plan.json` 的关键字段：

```json
{
  "release_id": "rel-mixed-20261119",
  "parameter_key": "service.shutdown_timeout_seconds",
  "target_version": "v2-shutdown-15s",
  "previous_stable_version": "v1-shutdown-30s",
  "thresholds": {
    "batch": {
      "error_rate": 0.02,
      "connection_reject_rate": 0.01
    },
    "instance": {
      "error_rate": 0.04,
      "connection_reject_rate": 0.025
    },
    "minimum_samples_per_instance": 5
  },
  "batches": [
    {
      "batch": 1,
      "instances": ["svc-01", "svc-02"],
      "observation_seconds": 10,
      "start_offset_seconds": 0
    }
  ],
  "metrics_file": "metrics.jsonl",
  "version_reports_file": "version_reports.jsonl"
}
```

`metrics.jsonl` 每行一个实例的秒级样本。也可使用 JSON 数组、包含 `metrics` 数组的 JSON 对象或 CSV：

```json
{"release_id":"rel-mixed-20261119","batch":1,"instance_id":"svc-01","timestamp":1800000100,"request_count":97,"error_rate":0.0042,"connection_reject_rate":0.0017,"parameter_version":"v2-shutdown-15s"}
```

聚合值使用 `request_count` 加权；没有请求数时按等权平均。每实例只取该批按时间排序后的前 `observation_seconds` 条有效样本。

`version_reports.jsonl` 是只读的参数版本上报：

```json
{"release_id":"rel-mixed-20261119","instance_id":"svc-01","timestamp":1800010000,"parameter_version":"v1-shutdown-30s"}
```

未提供版本上报文件时，服务会读取指标行中的 `parameter_version`。

## 判定与状态机

一批通过必须同时满足：

1. 每个计划实例至少有 `minimum_samples_per_instance` 条同时包含错误率和连接拒绝率的有效样本；
2. 加权批次错误率和连接拒绝率均不超过批级门槛；
3. 每个实例的错误率和连接拒绝率均不超过实例级门槛。

任意实例越阈即中止该批。批次平均低于门槛不能掩盖单实例异常。样本不足时结论为 `INSUFFICIENT_SAMPLES`，不会默认通过。

稳定业务错误码：

| 错误码 | 含义 |
| --- | --- |
| `PLAN_VERSION_MISSING` | 目标版本或上一稳定版本缺失 |
| `PLAN_BATCH_OVERLAP` | 批次号重复或观察窗口重叠 |
| `PLAN_DUPLICATE_INSTANCE` | 实例重复归属，包含批次与实例 |
| `PLAN_SCHEMA_INVALID` | 计划结构非法 |
| `INSTANCE_OCCUPANCY_CONFLICT` | 实例被另一未完成发布占用 |
| `INSUFFICIENT_SAMPLES` | 实例样本不足 |
| `AGGREGATE_THRESHOLD_EXCEEDED` | 批级聚合越阈 |
| `INSTANCE_THRESHOLD_EXCEEDED` | 实例级指标越阈 |
| `RELEASE_TERMINAL` | 已中止或已完成发布不得再开新批次 |
| `ROLLBACK_NOT_REQUIRED` | 当前发布无需回滚 |

计划校验是整单拒绝：只要存在版本缺失、批次重叠、重复实例等任何问题，就不会创建发布或批次。错误响应中的 `details.issues` 会包含全部问题及 `batch`/`instance_id` 上下文。

指标不达标时：

- 当前批次写为 `aborted`；
- 发布状态写为 `rollback_in_progress`；
- 后续批次保持 `pending`，结果为 `not_released`；
- 回滚目标固定为计划中的 `previous_stable_version`；
- 触发批次、触发实例和指标写入发布记录及不可变审计记录。

并发发布按批次原子获取实例锁。通过批次和已观察但失败的批次都会占用实例，直到发布完成或回滚核对完成。后到发布遇到锁冲突时，该批以 `INSTANCE_OCCUPANCY_CONFLICT` 中止，并返回冲突发布标识。HTTP 接口对此返回 `409`，与指标不达标区分。

## SQLite、重启与事务

默认数据库为 `orchestrator.db`。主要表：

- `releases`：发布状态、当前批次、回滚目标与核对结果；
- `batches`：批次状态、判定 JSON、结果与错误码；
- `decision_records`：只追加的发布审计记录；
- `samples`：已读取的指标样本；
- `version_reports`：参数版本上报；
- `instance_locks`：未完成发布的实例占用。

每批判定的 `decision_json`、越阈实例、批次状态、发布状态、回滚信息和审计记录在同一个 `BEGIN IMMEDIATE` 事务中提交。实例锁也在同一事务中获取。

服务没有内存态业务状态。使用同一个数据库重新创建 `Orchestrator` 或重启 HTTP 进程后：

- 已放行批次不会重新判定；
- 未完成批次从 `pending` 继续；
- `rollback_in_progress` 和 `blocked` 不会被自动恢复为进行中。

## 回滚版本核对

失败时服务记录失败批次的指标时间作为核对基线。`POST /rollback/verify` 只读取失败的已尝试批次之后、时间更新的版本上报和指标版本字段，逐实例核对回滚目标是否生效，不回写任何参数。

响应中的 `rollback.ineffective_instances` 会列出：

- `instance_id`；
- `observed_at`；
- `reported_version`；
- `reason`，如 `VERSION_MISMATCH` 或 `NO_VERSION_REPORT_AFTER_ROLLBACK`。

只有全部计划实例都上报目标版本后，状态才会变为 `rolled_back` 并释放实例锁。核对不完整时保持 `rollback_in_progress`，且不能开启新批次。

## Prometheus 指标

`/metrics` 暴露稳定的发布标签 `release_id`：

```text
release_orchestrator_current_batch{release_id="..."}
release_orchestrator_released_instances{release_id="..."}
release_orchestrator_threshold_instances{release_id="..."}
release_orchestrator_abort_count{release_id="..."}
release_orchestrator_rollback_in_progress{release_id="..."}
release_orchestrator_parameter_version{release_id="...",version="..."}
```

JSON 状态接口按批次号、实例号、版本和审计 ID 稳定排序，包含：

- 每批 `status`、`result`、`reason_code`、判定详情和逐实例样本数；
- `rollback_target_version`；
- `rollback.trigger_batch`、`trigger_instances`、`trigger_metrics`；
- `rollback.ineffective_instances`；
- 参数版本分布与已放行实例清单。

## 故障注入与场景报告

所有故障都通过生成器参数注入，不修改服务代码：

```bash
# 单实例连接拒绝率越阈，批次平均值仍低于批级门槛
python run_scenario.py --scenario instance_breach --seed 20261119 \
  --output artifacts/breach --force

# 指定实例样本缺失
python run_scenario.py --scenario missing_samples --seed 20261119 \
  --missing-instance svc-01 --output artifacts/missing --force

# 并发发布占用冲突
python run_scenario.py --scenario concurrent --seed 20261119 \
  --shared-instance-count 5 --output artifacts/concurrent --force

# 回滚后有实例仍上报目标版本
python run_scenario.py --scenario version_not_effective --seed 20261119 \
  --unapplied-instance svc-39 --output artifacts/version --force

# 组合单实例越阈与回滚版本未生效
python run_scenario.py --scenario mixed --seed 20261119 \
  --output artifacts/demo --force
```

可用场景：`clean`、`instance_breach`、`missing_samples`、`concurrent`、`version_not_effective`、`mixed`。

生成器还可直接指定 `--breach-batch`、`--breach-instance`、`--breach-metric`、`--missing-batch`、`--missing-instance`、`--unapplied-instance`、`--shared-instance-count`、`--observation-seconds` 和 `--minimum-samples`。

场景运行结束后写入 `report.json`，包含每批判定、触发实例与指标、回滚目标、未生效实例清单及数量、`reproduce` 复现命令。

## CLI

```bash
python -m release_orchestrator validate --plan samples/plan.json
python -m release_orchestrator run --samples samples --db local.db \
  --report artifacts/run-report.json
python -m release_orchestrator run --samples samples --db local.db \
  --verify-rollback
python -m release_orchestrator status --db local.db \
  --release-id rel-mixed-20261119
python -m release_orchestrator status --db local.db \
  --release-id rel-mixed-20261119 --audit
```

## 测试

```bash
pytest -q
```

测试覆盖：计划整单拒绝、批次重叠与重复实例、聚合正常但单实例越阈、样本不足、分批放行、重启续判、事务审计、回滚目标、版本未生效、并发占用冲突、Prometheus 与 JSON 接口、固定种子可复现性。

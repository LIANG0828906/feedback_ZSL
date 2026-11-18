# 值班交接决策服务

纯 Python 3.11 标准库实现，无第三方依赖、无外部服务、无网络访问。输入是本地分钟级多服务 SLI 样本与交接单，输出可复核的错误预算、双窗口燃尽、事故分级、静默建议、SQLite 交接快照和只读回放。

## 快速开始（Windows 10/11）

在项目根目录执行：

```powershell
py -3.11 scripts\generate_data.py --out data --seed 304
py -3.11 run_service.py decide --data-dir data --db var\handover.db
py -3.11 run_service.py submit --data-dir data --db var\handover.db
py -3.11 run_service.py status --data-dir data --db var\handover.db
py -3.11 run_service.py replay HO-2026-1118-1500 --data-dir data --db var\handover.db
py -3.11 run_service.py serve --data-dir data --db var\handover.db --host 127.0.0.1 --port 8765
```

也可用 `python -m duty_handover <command>`。服务只监听本地地址时不需要联网。

生成器固定种子 `304`，生成 3 个服务、每个 1440 分钟、共 4320 行样本，并生成主交接单、同交接时刻副本、同交接标识副本和临近日末副本。

需要并行实例时复用同一份只读样本目录，为每个实例指定独立 `--db` 和 `--port`。SQLite 使用 WAL 与 10 秒 busy timeout；不要把多个实例指向同一个业务库路径。

## 项目结构

```text
duty_handover/
  core.py       预算、燃尽、分级、静默判定
  data.py       样本/交接单解析与校验
  generator.py  确定性样本生成
  store.py      SQLite 快照、拒绝审计、不可变保护
  replay.py     只读分级回放
  metrics.py    Prometheus 文本指标
  service.py    HTTP/CLI 共用业务门面
  http_api.py   HTTP 路由
  cli.py        命令行入口
scripts/generate_data.py
tests/
data/           工作区内生成的样本与交接单
var/            SQLite 默认目录
```

## 输入契约

### SLI 样本

默认读取 `data/samples.csv`。也支持 JSON 数组、`{"samples": [...]}` 对象和 JSONL；若传入目录，优先读取 `samples.csv`。

必需字段：

| 字段 | 含义 |
| --- | --- |
| `timestamp` | ISO-8601 分钟时间，可带 `Z` 或时区 |
| `service` | 服务名 |
| `total_requests` | 该分钟总请求数 |
| `success_requests` | 该分钟成功请求数 |
| `p99_latency_ms` | 该分钟 P99 延迟 |
| `availability_target` | 可用率 SLO，支持 `0.999` 或 `99.9` |
| `latency_threshold_ms` | 延迟门限 |

同一服务同一时间点重复会返回 `DUPLICATE_SAMPLE`。读取后按 `(service, timestamp)` 排序，读取顺序不会影响结果。

### 交接单

主文件为 `data/handover.json`：

```json
{
  "handover_id": "HO-2026-1118-1500",
  "from_operator": "oncall-a",
  "to_operator": "oncall-b",
  "handover_at": "2026-11-18T15:00:00Z",
  "actual_alert_level": "P2",
  "notes": "上一班备注：支付链路 SLI 有点抖；未附预算快照。",
  "open_incidents": [
    {
      "incident_id": "INC-2026-1118-01",
      "services": ["payment-gateway"],
      "severity": "P2",
      "status": "open",
      "started_at": "2026-11-18T14:30:00Z"
    }
  ],
  "recent_changes": [
    {
      "change_id": "CHG-2026-1118-01",
      "services": ["payment-gateway"],
      "risk": "high",
      "changed_at": "2026-11-18T14:42:00Z"
    }
  ],
  "silences": [
    {
      "silence_id": "SIL-2026-1118-PAY-01",
      "services": ["payment-gateway"],
      "severity_scope": ["P2", "P3"],
      "starts_at": "2026-11-18T14:30:00Z",
      "ends_at": "2026-11-18T15:30:00Z"
    }
  ]
}
```

`open_incidents`、`recent_changes`、`silences` 可为空数组。静默服务范围可用 `["*"]` 表示全部服务；静默 `severity_scope` 省略表示 P1-P4 全级别。未提供 `actual_alert_level` 时，实际告警级别取未关闭事故中的最高级别。

## 计算口径

### 预算与延迟

对窗口内每分钟：

```text
可用率坏请求 = total_requests - success_requests
预算总量     = sum(total_requests * (1 - availability_target))
延迟坏请求   = 当 p99_latency_ms > latency_threshold_ms 时，计入该分钟所有成功请求
预算消耗     = 可用率坏请求 + 延迟坏请求（按分钟并集，不重复计算失败请求）
预算余量     = 预算总量 - 预算消耗
燃尽速率     = 预算消耗 / 预算总量
```

因此 P99 超阈的分钟即使 `success_requests == total_requests`，该分钟也会消耗完整一分钟请求量。余量和百分比允许为负，表示预算超支。

### 双窗口

| 窗口 | 默认长度 | 用途 |
| --- | --- | --- |
| 短窗口 | 30 分钟 | 发现突发恶化 |
| 长窗口 | 360 分钟 | 判断是否持续劣化 |

窗口以样本的参考时刻结束并包含首尾分钟。快照同时保留 `short_window`、`long_window` 的起止时间、样本数、预算消耗明细和燃尽率。状态口径：

服务记录中的主预算字段 `error_budget_remaining_*`、`budget_remaining_pct` 和分级所用的预算余量取长窗口；短窗口余量保留在 `budget_remaining.short_window`。这样突发读数与持续/预算读数不会混用。

- `burst`：短窗燃尽达到短窗触发值，且至少为长窗燃尽的两倍。
- `sustained`：长窗燃尽达到长窗触发值，且短窗没有不成比例的尖峰。
- `normal`：两个触发值都未达到。

默认短窗触发值为 `2.0`，长窗触发值为 `1.0`，突增倍数为 `2.0`。这些值以及完整公式也会写入快照的 `window_semantics`。

### 事故分级

分级综合五个维度，每个维度记录实际值、阈值、是否命中、映射级别：

| 维度 | P1 | P2 | P3 |
| --- | --- | --- | --- |
| 错误预算余量 | `<=10%` | `<=25%` | `<=50%` |
| 短窗燃尽 | `>=14.4` | `>=6.0` | `>=2.0` |
| 长窗燃尽 | `>=6.0` | `>=3.0` | `>=1.0` |
| 受影响服务数 | `>=3` | `>=2` | `>=1` |
| 近期变更风险 | `critical` | `high` | `medium` |

`low`/`none` 映射 P4。另有组合规则：多服务且高风险至少 P2；低预算且突发至少 P2；三个以上服务且短窗燃尽 `>=6` 时 P1。最终级别取所有维度与组合规则中最高者，`decision.grading.dimensions` 和 `decision.grading.combination_rules` 可逐项复核。可用率仍达标但 P99 导致燃尽越阈时，短窗燃尽维度会支撑 P3 或更高，不会落到 P4。

受影响服务定义为短窗/长窗燃尽越过触发值，或长窗预算余量低于 50%，或燃尽状态不是 `normal`。

### 静默判定

静默只产生告警抑制建议，不写回或改变样品、预算、燃尽和分级结果。每条静默检查：

- 是否在交接时刻有效。
- 是否覆盖当前全部受影响服务（支持 `"*"`）。
- 是否覆盖当前事故级别。
- 剩余分钟是否不少于当前级别的政策要求（默认 P1 180、P2 120、P3 60、P4 30）。
- 是否范围过宽：通配服务，或覆盖至少 3 个服务且包含无关服务。

结果状态为 `effective`、`risk`、`ineffective`。`ineffective` 会给出稳定原因码，例如 `SILENCE_REMAINING_TOO_SHORT`、`SILENCE_SCOPE_MISMATCH`、`SILENCE_SEVERITY_MISMATCH`、`SILENCE_EXPIRED`；`risk` 会给出 `SILENCE_WILDCARD_SCOPE` 或 `SILENCE_SCOPE_TOO_BROAD`。整体建议为 `keep`、`review`、`replace_or_remove` 或 `no_silence`。

## SQLite 快照

默认数据库为 `var/handover.db`。`snapshots` 表对 `handover_id` 和 `handover_at` 均设唯一约束，并有 `UPDATE`/`DELETE` 保护触发器。快照 JSON 包含：

- 交班人、接班人、交接时刻、交接单标识。
- 每个服务的长短窗口预算余量、燃尽率、燃尽状态。
- 每个服务的当前建议分级、实际级别、维度门槛明细。
- 静默状态、失效原因和服务级失效计数。
- 决策时的完整策略，供回放使用同一口径。

重复提交：

| 情况 | HTTP | 错误码 |
| --- | --- | --- |
| 相同交接时刻 | 409 | `DUPLICATE_HANDOVER_TIMESTAMP` |
| 相同交接标识、不同时刻 | 409 | `DUPLICATE_HANDOVER_ID` |

每次拒绝会写入 `rejected_submissions`，服务重启后仍可查询最近一次拒绝原因。已提交快照不会被后续同名交接覆盖。

## 只读回放

回放从已提交快照读取交接上下文和策略，只从样本目录读取交接时刻之后的数据，重新计算每分钟分级，与快照中实际留下的告警级别比较。输出：

- `actual_series`：实际级别序列。
- `recommended_series`：建议级别、触发指标、静默建议序列。
- `first_divergence_at`：首次级别分歧时刻。
- `first_divergence_trigger_metric`：触发该分歧的指标。
- `silence_recommendation_at_handover`：当时建议的静默处置。

回放只执行 `SELECT` 和内存计算，不修改快照。请求结束时刻晚于所有服务共同覆盖的样本范围时返回 `422 INSUFFICIENT_REPLAY_SAMPLES`。

## HTTP API

```text
GET  /healthz
GET  /metrics
GET  /api/policy
GET  /api/windows?service=...&start=...&end=...
GET  /api/handovers?limit=100
GET  /api/handovers/{handover_id}
GET  /api/status?handover_id=...
GET  /api/handovers/{handover_id}/replay?until=...&step_minutes=1
POST /api/decision
POST /api/handovers
POST /api/replay
POST /api/handovers/{handover_id}/replay
```

`POST /api/decision` 与 `POST /api/handovers` 可传 `{"handover": {...}}`，不传 body 时使用启动参数中的默认交接单。提交成功返回 `201`，重复返回 `409`。所有 JSON 使用稳定键排序。

示例：

```powershell
$h = Get-Content data\handover.json -Raw
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8765/api/handovers `
  -ContentType application/json -Body "{`"handover`":$h}"

Invoke-RestMethod http://127.0.0.1:8765/api/status?handover_id=HO-2026-1118-1500
Invoke-WebRequest http://127.0.0.1:8765/metrics
```

## Prometheus 指标

所有服务级指标使用稳定标签 `service`：

```text
handover_error_budget_remaining_ratio
handover_error_budget_remaining_pct
handover_error_budget_remaining_requests
handover_burn_rate_short
handover_burn_rate_long
handover_current_severity          # 1=P1, 2=P2, 3=P3, 4=P4, 0=unknown
handover_current_grade             # 额外标签 grade
handover_silence_invalid_count     # 最近快照
handover_silence_invalid_total     # 历史累计
handover_snapshot_count            # 含该服务的快照数
handover_snapshot_total            # 全部快照数
```

## 稳定错误码

| 错误码 | 场景 |
| --- | --- |
| `DUPLICATE_HANDOVER_TIMESTAMP` | 交接时刻已提交 |
| `DUPLICATE_HANDOVER_ID` | 交接标识已提交 |
| `SNAPSHOT_NOT_FOUND` | 查询不存在的交接 |
| `INSUFFICIENT_REPLAY_SAMPLES` | 回放区间缺少样本 |
| `REPLAY_RANGE_INVALID` | 回放结束早于交接时刻或步长非法 |
| `INVALID_HANDOVER` | 交接单字段非法 |
| `INVALID_SAMPLE` | SLI 样本字段非法 |
| `DUPLICATE_SAMPLE` | 同服务同时间重复样本 |
| `SERVICE_NOT_FOUND` | 查询不存在的服务 |

HTTP 错误体固定为 `{"error":{"code":"...","message":"...","details":{...}}}`。

## 本地验收

```powershell
py -3.11 -m unittest discover -s tests -v
```

固定场景预期：

- 交接快照建议级别 `P2`，实际级别 `P2`。
- 短窗燃尽约 `2.931574`，长窗燃尽约 `0.480908`，状态 `burst`。
- 支付静默剩余 30 分钟，低于 P2 要求的 120 分钟，判定 `ineffective`。
- 全局通配静默被标为 `risk`，整体静默建议为 `review`。
- 回放首次分歧时刻 `2026-11-18T15:30:00Z`，建议升级为 `P1`。
- `order-api` 的 P99 超阈区间可用率仍达标，但 20 个超阈分钟全部消耗预算，回放路径为 `P1`。

测试覆盖预算复算、延迟联合消耗、读取顺序稳定性、分级门槛支撑、静默解耦、重复提交、重启查询、只读回放、样本不足错误码、Prometheus 指标和 HTTP 接口。

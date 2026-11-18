# 推荐排序 v3/v4 兼容推理网关

本项目从零实现一个可在 Windows 10/11 或 Windows Server 2022、2 核 CPU、1.25 GiB 内存下运行的本地推理网关。网关通过 ONNX Runtime 1.18 托管 v3、v4 两个模型，按租户和客户端标识做无粘性灰度分流，并在模型加载、请求输入、响应输出、SLO 回退和状态恢复各层执行契约校验。

## 关键行为

- v3 契约：48 个 `float32` 特征，列顺序为 `profile_00..39, behavior_00..07`，模型输出 `score`，HTTP 响应中的顶层 `score` 是 JSON 浮点数。
- v4 契约：56 个 `float32` 特征，列顺序为 `behavior_00..07, profile_00..39, context_00..07`，模型输出 `score` 与 `calibration_offset`，HTTP 响应中的顶层 `score` 是带校准字段的对象。
- 分流桶为 `SHA-256(tenant_id + NUL + client_id) mod 10000`，每次请求都按当前阈值重新判断，不保存客户端粘性版本。权重从 0 调到 1 或从 1 调到 0 后，同一客户端的后续新请求立即服从新权重。
- 未携带 `client_id` 的请求按租户权重对每个请求独立抽样。
- 请求先选中目标模型，再校验其声明的 `protocol_version`、特征数量、列顺序与类型。声明协议与目标模型不一致时拒绝，不会把错位特征送入模型。
- 每个模型版本独立加载。清单与 ONNX 实际输入/输出签名不一致时，该版本进入 `failed`，另一个已就绪版本继续服务。
- 每次请求的端到端时延进入最近窗口。灰度版本 P99 或错误率超过该版本清单声明的 SLO 时，自动把全部租户的灰度权重收敛到 0，并持久化回退事件。
- 路由审计、推理窗口、权重策略、回退事件、请求幂等结果都持久化到本地 SQLite。重启后恢复权重、指标窗口、累计指标和幂等响应。
- Prometheus 指标同时按 `version` 与 `tenant` 打标签，所有已配置租户和版本即使计数为零也会暴露稳定序列。

## 快速开始（Windows PowerShell）

```powershell
cd <工作区根目录>
.\scripts\setup.bat
.\scripts\generate_assets.bat
.\scripts\run_tests.bat
.\scripts\run_gateway.bat
```

手动等价命令：

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.txt
.\.venv\Scripts\python.exe scripts\generate_assets.py --output-dir generated --samples 10000
.\.venv\Scripts\python.exe -m rec_gateway --config config\gateway.json
```

默认绑定 `127.0.0.1:8080`，可用 `--host`、`--port`、`--model-dir`、`--database` 覆盖。服务使用单进程、HTTP/1.1、asyncio 事件循环；不要增加 uvicorn worker。

## 模型与样本生成

```powershell
.\.venv\Scripts\python.exe scripts\generate_assets.py --output-dir generated --samples 10000
```

固定种子为 `294`，产出：

- `generated/models/v3/model.onnx` 与 `manifest.json`
- `generated/models/v4/model.onnx` 与 `manifest.json`
- `generated/samples/requests.jsonl`，10000 条请求，四种租户均覆盖 v3/v4
- `generated/assets.json`，包含模型 SHA-256

生成器直接写标准 ONNX protobuf，不依赖 `onnx` Python 包，只依赖 ONNX Runtime。重复执行产物字节稳定。清单内的 `model_sha256` 在加载时再次校验。

## 请求接口

### 单条推理

`POST /v1/infer`，别名 `POST /infer`。

```json
{
  "request_id": "req-001",
  "tenant_id": "tenant-d",
  "client_id": "client-42",
  "protocol_version": "v4",
  "feature_names": ["behavior_00", "behavior_01"],
  "features": [0.25, -0.5]
}
```

上例仅展示字段结构；实际 v4 请求必须按目标清单提供完整 56 列。也支持 `feature_protocol_version` 或 `protocol` 作为 `protocol_version` 的别名，但三者只能出现一个。`features` 也可传以列名为键的对象，此时网关按清单列顺序重排。

v3 成功响应：

```json
{
  "ok": true,
  "protocol_version": "v3",
  "request_id": "req-001",
  "score": 1.1454464,
  "served_version": "v3"
}
```

v4 成功响应：

```json
{
  "ok": true,
  "protocol_version": "v4",
  "request_id": "req-002",
  "score": {
    "calibrated": true,
    "calibration_offset": 0.0625,
    "calibration_version": "v4-platt-local-1",
    "raw_score": 0.375,
    "value": 0.4375
  },
  "served_version": "v4"
}
```

成功响应还带 `X-Request-Id`、`X-Served-Version`、`X-Protocol-Version` 响应头。幂等重放带 `X-Idempotent-Replay: true`。

### 批量推理

`POST /v1/infer/batch`，别名 `POST /batch`。

```json
{
  "items": [
    {"request_id": "b1", "tenant_id": "tenant-a", "protocol_version": "v3", "features": ["...48 values..."]},
    {"request_id": "b2", "tenant_id": "tenant-d", "protocol_version": "v4", "features": ["...56 values..."]}
  ]
}
```

也接受顶层键 `requests`。批量中的每项独立分流、校验和成契约，单项失败不会阻断其他项。批量上限默认为 64。

### 状态、健康与指标

- `GET /status`，别名 `GET /state`：返回版本加载状态、协议版本、特征维度、输出结构、租户权重和最近回退记录，JSON 键固定排序且不含绝对路径。
- `GET /healthz`：任一版本就绪返回 200；全部不可用返回 503。
- `GET /metrics`：Prometheus 文本格式。

核心指标：

```text
gateway_requests_total{version,tenant}
gateway_errors_total{version,tenant}
gateway_timeouts_total{version,tenant}
gateway_latency_ms{version,tenant,quantile="0.5|0.95|0.99"}
gateway_latency_p99_ms{version,tenant}
gateway_error_rate{version,tenant}
gateway_current_weight{version,tenant}
gateway_protocol_mismatch_rejections_total{version,tenant}
gateway_tenant_requests_total{tenant}
gateway_rollbacks_total{version,reason}
```

### 管理接口

```text
PUT  /admin/tenants/{tenant_id}/weights      {"canary_weight": 0.25}
GET  /admin/tenants
GET  /admin/route/preview?tenant_id=&client_id=
GET  /admin/route-audit?tenant_id=&client_id=&limit=50
POST /admin/models/{version}/reload
POST /admin/models/reload                    {"version": "v4"}
```

权重写入同时更新内存路由与 SQLite，不重启服务。`route-audit` 返回每次请求的客户端桶、当时权重、候选版本、实际选择版本、落库版本和错误码，可用于追溯调整前后的客户端分流。

## 业务错误码

成功响应按版本输出不同结构；失败响应统一为 `{"ok": false, "error": {"code", "message", "details"}}`。

| 错误码 | HTTP | 说明 |
| --- | ---: | --- |
| `FEATURE_PROTOCOL_MISMATCH` | 400 | 请求声明协议与选中模型协议不一致 |
| `FEATURE_COUNT_MISMATCH` | 400 | 特征值数量不符 |
| `FEATURE_COLUMN_COUNT_MISMATCH` | 400 | `feature_names` 数量不符 |
| `FEATURE_COLUMN_ORDER_MISMATCH` | 400 | 列名在具体索引处不符 |
| `FEATURE_COLUMN_MISSING` | 400 | 对象式特征缺少列 |
| `FEATURE_COLUMN_UNEXPECTED` | 400 | 对象式特征包含未知列 |
| `FEATURE_TYPE_MISMATCH` | 400 | 某列为非数字、布尔值或非有限数 |
| `UNKNOWN_TENANT` | 404 | 租户未配置 |
| `BATCH_LIMIT_EXCEEDED` | 413 | 批量过大 |
| `IDEMPOTENCY_CONFLICT` | 409 | 同一 `request_id` 对应不同请求体 |
| `IDEMPOTENCY_IN_PROGRESS` | 409 | 同一 `request_id` 的首次请求仍在处理 |
| `VERSION_UNAVAILABLE` | 503 | 无可用模型 |
| `INFERENCE_TIMEOUT` | 504 | 端到端超时 |
| `INFERENCE_ERROR` | 500 | ONNX Runtime 推理失败 |

协议、维度和类型错误都在 `details.column_index` 给出零基列位置；列顺序错误同时给出 `expected_column` 与 `actual_column`。

## 加载状态与契约校验

每个版本的状态是 `loading`、`ready`、`failed` 之一。加载流程：

1. 解析并校验 `manifest.json` 的 schema、协议版本、列索引连续性、列名唯一性、输入输出 rank 和 `[batch, 1]` 输出形状。
2. 校验模型文件在版本目录内且 SHA-256 匹配。
3. 用 ONNX Runtime 打开模型，比较实际输入数量、名称、dtype、维度，以及实际输出名称、dtype、维度。
4. 仅当全部通过才替换为 `ready` 会话；失败时旧会话被移除，该版本不可选，错误码保留在 `/status`。

加载失败不会使进程退出。若灰度版本失败，启动阶段会把灰度权重置零；稳定版本保持可服务。

## 回退与恢复

清单 `slo` 声明 `latency_p99_ms`、`error_rate`、`window_size`、`min_samples` 和 `request_timeout_ms`。每个 `(version, tenant)` 保留最近 `window_size` 条端到端记录：

- 样本数达到 `min_samples` 后，P99 严格大于阈值或错误率严格大于阈值即触发回退。
- 回退在一个策略事务内把全部租户的灰度权重置零，记录指标名、观测值、阈值、窗口样本数、触发租户、回退前权重和回退时刻。
- 回退只改变灰度策略，不触碰 v3 会话；稳定版本请求继续可用。
- 管理员重新设置非零权重后可启动新一轮灰度。

SQLite 使用 WAL。请求的输入指纹与成功响应按 `(tenant_id, request_id)` 持久保存；同一请求体重放返回原响应，不重复推理，不同请求体复用同一 ID 返回 409。并发相同幂等键由单进程 in-flight 集合串行化。

## 故障注入与场景报告

场景脚本可自行拉起隔离的临时网关，也可连接 `--base-url`。它严格串行发送请求，不额外占用推理并发；延迟注入在网关进入 ORT 前执行，列错位通过交换请求中的 `feature_names` 实现，加载失败通过临时生成一份维度不匹配的 v4 清单实现，不修改网关源码。

```powershell
# 正常 v4 灰度
.\.venv\Scripts\python.exe scripts\run_scenario.py --tenant tenant-d --protocol-version v4 --count 5000 --rate 0 --canary-weight 1.0 --output reports\workload_v4_5000.json

# 全部注入 150ms 延迟，触发 P99 SLO 回退
.\.venv\Scripts\python.exe scripts\run_scenario.py --tenant tenant-d --protocol-version v4 --count 80 --rate 100 --canary-weight 1.0 --delay-rate 1.0 --delay-ms 150 --output reports\scenario_rollback.json

# 全部注入列错位
.\.venv\Scripts\python.exe scripts\run_scenario.py --tenant tenant-d --protocol-version v4 --count 20 --rate 200 --canary-weight 1.0 --misalign-rate 1.0 --output reports\scenario_misalign.json

# 注入 v4 加载失败，v3 继续服务
.\.venv\Scripts\python.exe scripts\run_scenario.py --tenant tenant-c --protocol-version v3 --count 50 --rate 200 --canary-weight 1.0 --inject-load-failure --output reports\scenario_load_failure.json
```

报告包含：声明协议分布、目标版本分布、实际服务版本分布、各版本时延 P50/P95/P99、错误总数与分类、超时数、协议不匹配拒绝数、列错位拒绝数、回退事件序列、最终权重与版本状态。

## 资源边界

- 请求并发上限默认 2，由全局信号量控制。
- 两个 ORT 会话均设置 `intra_op_num_threads=1`、`inter_op_num_threads=1`、顺序执行、关闭 CPU arena。
- FastAPI 计算端点使用同步函数，由线程池承载；真正的模型推理仍由双并发信号量限制。
- 每版本每租户统计窗口最多保留 2000 条样本，磁盘只保留清单、SQLite 与固定种子样本。
- `scripts/run_scenario.py` 每次只启动一个网关进程，场景请求串行发送。
- 生成样本约 11 MiB；SQLite 和报告远低于 2 GB 临时磁盘边界。

## 验证

完整验证：

```powershell
.\scripts\run_tests.bat
.\scripts\run_scenarios.bat
```

Linux/WSL 等价入口为 `bash scripts/validate.sh`。

当前工作区实测（Python 3.11.18、ONNX Runtime 1.18.1、Linux x86_64）：

- `pytest`：14 项通过。
- 10,000 条样本分两轮串行执行：v3 5000 条 0 错误，P99 1.675 ms；v4 5000 条 0 错误，P99 2.117 ms。
- 独立计量的网关进程处理 5000 条 v4 请求耗时 7.21 s，P99 1.868 ms，峰值 RSS 84.4 MiB，平均 CPU 占用为单核的 88%。
- 列错位场景：20/20 返回 `FEATURE_COLUMN_ORDER_MISMATCH`，均定位到具体列，无模型计分。
- SLO 回退场景：40 条灰度样本后 P99 达到 150.526 ms，超过清单阈值 120 ms；v4 全租户权重收敛为 0，后续 v4 请求不再进入 v4，回退事件包含 40 条窗口样本和触发时刻。
- 超时场景：40 次注入 700 ms 延迟的请求均返回 504 并计入 `gateway_timeouts_total`；稳定版本仍可服务。
- 加载失败场景：v4 状态为 `failed`、错误码 `MANIFEST_DIMENSION_MISMATCH`，50/50 条 v3 请求由 v3 正常服务。
- 幂等与重启：相同请求 ID 重放命中 SQLite 缓存；权重变更在重启后保持；并发相同幂等键只产生一次新推理。

对应报告在 `reports/`，例如 `workload_v3_5000.json`、`workload_v4_5000.json`、`workload_gateway_rss.json`、`scenario_misalign.json`、`scenario_rollback.json`、`scenario_timeout.json` 和 `scenario_load_failure.json`。

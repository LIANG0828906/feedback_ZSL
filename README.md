# 推荐排序模型双版本灰度网关

本工作区实现一个只在 `127.0.0.1` 提供服务的 ranking ONNX 推理网关，同时托管 `v3` 稳定版本和 `v4` 灰度版本，支持租户/客户端分流、热更新比例、协议字段校验、自动回退、Prometheus 指标，以及确定性故障注入与 JSON 报告。

## 目录

- `gateway/`：网关配置、模型状态机、分流、监控、指标与 FastAPI 接口。
- `run_gateway.py`：本地网关入口。
- `export_models.py`：固定随机种子的确定性 ONNX 导出脚本；v3 为 8 维，v4 为 10 维。
- `traffic_gen.py`：确定性样本生成、故障注入、回退观测和报告生成。
- `traffic_scenario.json`：默认流量与故障场景。
- `tests/test_gateway.py`：真实 ONNX Runtime 集成测试。
- `artifacts/models/`：任务自带模型；`artifacts/traffic_report.json` 为运行报告。

## Windows 运行

要求 Windows 10/11 或 Windows Server 2022、Python 3.11、2 核 CPU、1.25 GiB 内存。

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
python -m pip install -r requirements.txt
python export_models.py --check
python run_gateway.py --config config.json
```

`export_models.py --check` 只计算并打印模型哈希。需要重新生成模型时执行 `python export_models.py`，输出使用固定种子，可重复得到相同文件哈希。

网关默认监听 `http://127.0.0.1:8080`，只启动一个 Uvicorn worker。每个 ONNX session 的 `intra_op_num_threads` 和 `inter_op_num_threads` 均为 1，满足 2 个逻辑处理器的约束。

## 模型与协议

| 版本 | 角色 | 协议 | 特征数 | 输入 |
| --- | --- | --- | --- | --- |
| `v3` | stable | `ranking-features-v1` | 8 | `features: [batch, 8]` |
| `v4` | canary | `ranking-features-v2` | 10 | `features: [batch, 10]` |

v1 顺序：

```text
user_age, item_age, ctr_7d, cvr_7d, price, category_affinity,
freshness, query_match
```

v2 顺序：

```text
user_age, item_age, price, category_affinity, ctr_7d, cvr_7d,
discount, delivery_eta, freshness, query_match
```

请求必须同时提供有序的 `feature_names` 与 `features`。网关先按客户端声明的协议校验字段数量、名称和顺序，再检查所选模型是否接受该协议。校验失败时不会调用模型，也不会返回分数。

```json
{
  "request_id": "req-001",
  "tenant_id": "tenant-canary",
  "client_id": "client-a",
  "protocol_version": "ranking-features-v2",
  "feature_names": [
    "user_age", "item_age", "price", "category_affinity", "ctr_7d",
    "cvr_7d", "discount", "delivery_eta", "freshness", "query_match"
  ],
  "features": [0.3, 0.2, 0.8, 0.6, 0.4, 0.5, 0.1, 0.7, 0.9, 0.2]
}
```

成功请求返回实际模型版本、分数、端到端时延及命中的分流桶。客户端标识也可放在 `X-Client-ID`，租户和协议分别支持 `X-Tenant-ID`、`X-Feature-Protocol-Version` 请求头。

## 分流与管理接口

- `GET /state`、`GET /status`：JSON 状态摘要，包括每版本模型标识、加载状态、是否 ready、协议、当前权重、状态迁移和最近回退记录。
- `GET /metrics`、`GET /prometheus`：Prometheus 文本指标。
- `POST /predict`、`POST /v1/predict`：推理请求。
- `POST /admin/weights`：热更新权重；`tenant_id`/`client_id` 可限定作用域。
- `POST /admin/reload/{version}?wait=true`：重新加载指定模型。
- `POST /admin/faults`：注入推理延迟或下一次加载失败。
- `POST /admin/rollback/reset`：解除指定版本的自动回退禁用状态，便于重新灰度。
- `POST /admin/evaluate`：立即执行一次回退判定。

默认权重为 v3=0.8、v4=0.2。更新示例：

```json
{"weights": {"v3": 0.7, "v4": 0.3}}
```

租户或客户端权重覆盖示例：

```json
{
  "tenant_id": "tenant-canary",
  "client_id": "client-a",
  "weights": {"v3": 0.0, "v4": 1.0}
}
```

每次请求都重新计算权重和稳定哈希桶，不保存客户端版本粘滞状态。未提供 `client_id` 时使用租户权重；租户没有覆盖时使用默认权重。显式设置的客户端级覆盖优先于租户和默认权重。

## 错误码与就绪语义

业务错误统一为 `{"error": {"code": "...", "message": "...", "field": "...", ...}}`，并带 `X-Error-Code` 响应头。

| 错误码 | HTTP | 含义 |
| --- | --- | --- |
| `FEATURE_COUNT_MISMATCH` | 422 | 字段数量与声明协议不一致，`field=features` |
| `FEATURE_NAMES_COUNT_MISMATCH` | 422 | 字段名数量不一致 |
| `FEATURE_ORDER_MISMATCH` | 422 | 顺序不一致，`field=feature_names[index]` |
| `UNSUPPORTED_PROTOCOL` | 422 | 未注册的协议版本 |
| `MODEL_PROTOCOL_MISMATCH` | 422 | 声明协议与选中的模型不兼容 |
| `MODEL_NOT_READY` | 409 | 等待就绪超时且没有兼容的稳定版本 |
| `INFERENCE_TIMEOUT` | 504 | 推理超过配置上限 |
| `MODEL_LOAD_FAILED` | 409 | 管理面重载失败 |

模型加载在后台状态机中执行：`PENDING → LOADING → READY/FAILED`。重载已有模型时状态为 `LOADING`，但旧的已就绪 session 继续服务；新会话加载成功后原子替换，失败则保留旧会话并记录 `MODEL_LOAD_FAILED`。首次加载没有旧会话时才会进入 `FAILED`。整个路径不会用 HTTP 503 表示模型未就绪。

请求如果选中一个正在加载的版本，会等待 `loading.ready_timeout_ms`；期间该版本变为 ready 即继续推理，否则尝试稳定版本。稳定版本协议不兼容时返回 `MODEL_PROTOCOL_MISMATCH`，完全没有可服务版本时返回 `MODEL_NOT_READY`。

## 监控与自动回退

`monitoring` 默认参数：

- 滚动窗口 10 秒，最少 8 个样本。
- v4 端到端 P99 阈值 40 ms。
- 错误率阈值 10%。
- 单请求推理超时 500 ms。

监控同时维护版本级和 `(version, tenant)` 级统计。v4 的版本级或任一租户窗口一旦超过 P99/错误率阈值，立即把该版本权重收缩为 0，回退到 `v3`。回退记录包含触发指标、观测值、阈值、窗口请求/错误/超时数、回退时刻和 UTC 时间。

Prometheus 指标名与标签：

```text
ranking_gateway_version_requests_total{version,tenant}
ranking_gateway_version_errors_total{version,tenant}
ranking_gateway_version_timeouts_total{version,tenant}
ranking_gateway_tenant_requests_total{tenant}
ranking_gateway_version_latency_ms{version,tenant,quantile}
ranking_gateway_version_latency_p99_ms{version,tenant}
ranking_gateway_version_error_rate{version,tenant}
ranking_gateway_version_timeout_rate{version,tenant}
ranking_gateway_version_weight{version}
ranking_gateway_model_ready{version}
ranking_gateway_rollback_active{version}
ranking_gateway_rollbacks_total
ranking_gateway_version_request_summary{version,kind}
```

## 确定性故障注入

故障通过管理接口注入，不修改网关源码。默认配置允许故障注入：

```json
{"inference_delay_ms": {"v4": 50}}
```

```json
{"fail_next_load": ["v4"]}
```

```json
{"clear": true}
```

首次启动即失败的场景可配置 `fault_injection.startup_fail_versions`，或使用环境变量：

```powershell
$env:RANKING_FAULT_FAIL_LOAD = "v4"
python run_gateway.py
```

## 端到端验证

网关已启动时：

```powershell
python traffic_gen.py --report artifacts/traffic_report.json
```

由脚本自动启动并在结束后关闭网关：

```powershell
python traffic_gen.py --autostart --report artifacts/traffic_report.json
```

默认场景以固定种子生成样本，覆盖稳定基线、v4 健康流量、同一客户端热切换、加载失败隔离、50 ms 推理延迟、自动回退、回退期间稳定探测、协议不兼容和字段顺序错位。所有请求只发往回环地址。

报告包含：

- 各版本/租户/阶段的请求数、错误数、超时数和 P50/P95/P99。
- 每次回退的触发指标、阈值、窗口统计和回退时刻。
- 回退耗时、回退期间探测成功率和加载失败隔离检查。
- HTTP 状态码、业务错误码分布、故障注入记录与验收结果。

时间戳与耗时字段每次运行会变化；使用报告中的确定性投影比较其余字段：

```python
import json
from traffic_gen import deterministic_projection

first = deterministic_projection(json.load(open("report-1.json", encoding="utf-8")))
second = deterministic_projection(json.load(open("report-2.json", encoding="utf-8")))
assert first == second
```

当前环境实测（同一配置连续运行两次）：

| 指标 | 结果 |
| --- | --- |
| 稳定版本 P95 | 7.99–15.83 ms（多次运行） |
| v4 注入 50 ms 后 P99 | 54.20–58.93 ms |
| 自动回退耗时 | 109.98–249.80 ms |
| 回退期间稳定探测 | 40/40 成功，0 超时 |
| 确定性投影 | 两次完全一致 |
| 网关进程常驻内存 | 约 87 MiB |

## 测试

```powershell
python -m pytest -q
```

测试覆盖双版本加载、热更新与同客户端重分配、协议版本/字段数量/字段顺序拒绝、50 ms 延迟触发回退、回退期间稳定流量、加载失败隔离，以及加载期间请求等待而非返回 503。

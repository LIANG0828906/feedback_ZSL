# 电源链路冗余审计与下电编排服务

该仓库提供一个可在 Windows 10/11、2 核 CPU、1.25 GiB 内存上运行的轻量服务，用于：

- 从本地资产 JSON 和 21 个只读 SNMP v2c 模拟设备采集 96 台服务器的真实供电链路。
- 判定双路电源是否在同一 PDU、UPS 或上联支路上重合。
- 计算每个 PDU/UPS 的全失电与单路失电暴露面。
- 生成可执行/不可执行的计划内下电计划。
- 在 UPS 电池低于阈值时生成两级紧急下电顺序。
- 将审计、路径、判定和计划原子写入 SQLite，并提供历史查询、Prometheus 指标和 JSON 状态接口。

所有设备均为本机 UDP 模拟端点，不需要真实机房网络、容器、消息队列或额外数据库服务。

## 快速开始

要求 Python 3.11。标准库已足够运行内置 SNMP 后端；安装依赖后会优先使用 `pysnmp 4.4.12` 命令响应器后端。

```powershell
py -3.11 -m venv .venv
.\.venv\Scripts\Activate.ps1
pip install -r requirements.txt
```

启动 21 个模拟设备（4 UPS + 17 PDU）：

```powershell
python -m poweraudit simulate --config config/local.json
```

另开终端启动服务：

```powershell
python -m poweraudit serve --config config/local.json
```

也可以在单进程内同时启动模拟器与 HTTP 服务：

```powershell
python -m poweraudit serve --config config/local.json --with-simulator
```

默认监听 `http://127.0.0.1:8080`，模拟设备监听 `127.0.0.1:12701-12721`。按 `Ctrl+C` 会关闭 HTTP、SQLite 使用中的连接和全部 UDP 监听。

### 端到端示例

```powershell
# 1. 执行并提交审计
curl.exe -X POST http://127.0.0.1:8080/api/v1/audits `
  -H "Content-Type: application/json" `
  -d "{\"batch_id\":\"OPS-20261119-001\"}"

# 2. 查询指定批次的未知设备、最高风险和最近计划
curl.exe "http://127.0.0.1:8080/api/v1/status?batch_id=OPS-20261119-001"

# 3. 生成 PDU-AUX-17 检修计划
curl.exe -X POST http://127.0.0.1:8080/api/v1/plans `
  -H "Content-Type: application/json" `
  -d "{\"batch_id\":\"OPS-20261119-001\",\"target_type\":\"pdu\",\"target_id\":\"PDU-AUX-17\"}"

# 4. 根据低电量 UPS 生成紧急顺序
curl.exe -X POST http://127.0.0.1:8080/api/v1/emergency-plans `
  -H "Content-Type: application/json" `
  -d "{\"batch_id\":\"OPS-20261119-001\"}"

# 5. Prometheus 文本指标
curl.exe http://127.0.0.1:8080/metrics
```

重复提交同一个 `batch_id` 返回 HTTP `409` 和稳定错误码：

```json
{"error":{"code":"AUDIT_BATCH_EXISTS","message":"audit batch already exists: OPS-20261119-001"}}
```

## 目录

```text
poweraudit/
  audit.py         真实拓扑重建、冗余判定、暴露面与风险排序
  collector.py     有界并发 SNMP 采集和总截止时间
  planner.py       检修下电计划、紧急下电顺序
  database.py      SQLite 原子提交和历史查询
  service.py       HTTP API
  metrics.py       Prometheus 文本输出
  simulator.py     只读 SNMP v2c 内置模拟器
  pysnmp_backend.py pysnmp 4.4.12 单引擎 21 端口后端
data/
  asset_inventory.json    96 台服务器和 21 台设备的资产声明
  simulator_scenario.json 模拟设备实际状态和端口连接
config/local.json         本机配置
tests/                    协议、审计、计划、持久化和超时测试
```

## 配置

`config/local.json` 中的相对路径以配置文件所在目录为基准，因此可从任意工作目录启动。

```json
{
  "inventory_path": "../data/asset_inventory.json",
  "scenario_path": "../data/simulator_scenario.json",
  "database": {"path": "../runtime/power_audit.db"},
  "http": {"host": "127.0.0.1", "port": 8080},
  "snmp": {
    "request_timeout_seconds": 0.25,
    "device_timeout_seconds": 1.2,
    "total_timeout_seconds": 8.0,
    "retries": 0,
    "max_workers": 8,
    "max_varbinds_per_request": 24
  },
  "emergency": {
    "battery_threshold_percent": 25,
    "runtime_threshold_minutes": 20,
    "failover_reserve_percent": 10,
    "min_transfer_load_percent": 1.0
  },
  "simulator": {"backend": "auto"}
}
```

- `simulator.backend` 可取 `auto`、`pysnmp`、`builtin`。`auto` 优先 pysnmp，导入失败时使用内置只读 v2c 后端。
- 设备 IP、UDP 端口、社区字符串都在 `data/asset_inventory.json` 的 `devices` 中修改，不需要改 Python 源码。
- 采集采用每设备独立上限加全局硬截止时间。某台设备超时只把该设备及其下游路径标为未知。

## 资产与模拟场景

设备声明：

```json
{
  "device_id": "UPS-01",
  "device_type": "ups",
  "host": "127.0.0.1",
  "port": 12701,
  "community": "public",
  "enabled": true
}
```

PDU 额外包含 `port_count`。服务器 A/B 声明：

```json
{
  "server_id": "SRV-001",
  "rack": "R01",
  "business_cost": 38,
  "acceptable_outage": false,
  "power_inputs": [
    {
      "path_id": "A",
      "pdu_id": "PDU-R01-A",
      "port": 1,
      "declared_ups_id": "UPS-01",
      "declared_upstream_branch_id": "FEED-R01-A"
    },
    {
      "path_id": "B",
      "pdu_id": "PDU-R01-B",
      "port": 1,
      "declared_ups_id": "UPS-03",
      "declared_upstream_branch_id": "FEED-R01-B"
    }
  ]
}
```

`simulator_scenario.json` 是设备实际上报状态，包含 PDU 端口上的 `server_id`、端口状态、实际上联 UPS、上联支路和 UPS 负载/电池。显式 `ports` 表是权威数据，不会由资产声明反推，因此可以模拟“资产声明看似冗余、实际接错”的场景。

演示数据包含：

- 66 台真实独立双路服务器。
- 14 台两路实际接在同一 PDU 的服务器，资产声明已陈旧。
- 6 台两台 PDU 实际共用同一 UPS 的服务器。
- 4 台跨 PDU、跨 UPS 但共用同一上联支路的服务器。
- 6 台仅登记一路电源的服务器。
- UPS-01 处于电池供电、18% 剩余容量、12 分钟剩余续航。

## SNMP OID

企业 OID 根为 `.1.3.6.1.4.1.424242`。模拟器支持标准 SNMP v2c `GET`、`GETNEXT` 和 `GETBULK`，不支持写操作。

| 含义 | OID |
|---|---|
| `sysDescr` | `.1.3.6.1.2.1.1.1.0` |
| 设备 ID | `.1.3.6.1.4.1.424242.1.1.0` |
| 设备类型 | `.1.3.6.1.4.1.424242.1.2.0` |
| 设备状态 | `.1.3.6.1.4.1.424242.1.3.0` |
| PDU 上联 UPS | `.1.3.6.1.4.1.424242.2.1.0` |
| PDU 上联支路 | `.1.3.6.1.4.1.424242.2.2.0` |
| 端口上联 UPS | `.1.3.6.1.4.1.424242.2.3.1.1.<port>` |
| 端口上联支路 | `.1.3.6.1.4.1.424242.2.3.1.2.<port>` |
| 端口状态 | `.1.3.6.1.4.1.424242.2.3.1.3.<port>` |
| 端口服务器归属 | `.1.3.6.1.4.1.424242.2.3.1.4.<port>` |
| UPS 输出负载 % | `.1.3.6.1.4.1.424242.3.1.0` |
| UPS 电池剩余 % | `.1.3.6.1.4.1.424242.3.2.0` |
| UPS 剩余续航分钟 | `.1.3.6.1.4.1.424242.3.3.0` |
| UPS 供电状态 | `.1.3.6.1.4.1.424242.3.4.0` |

## 审计规则

1. PDU 端口上报的服务器归属优先用于重建真实路径；资产声明用于逐项核对和不可达设备的保守补足。
2. 只有 PDU、UPS、上联支路三级都不同，才判定为 `independent`。
3. 任一级重合判定为 `pseudo_redundant`，返回 `overlap_levels` 与主重合层级：
   - `pdu`
   - `ups`
   - `upstream_branch`
4. 只登记并验证到一路为 `single_path`。
5. PDU/UPS 不可达、端口缺失、上联未知或资产端口无法验证时为 `topology_unknown`。未知设备单独放入 `unknown_devices`，不进入正常风险排序。
6. 同一输入的服务器路径、重合结论、暴露列表和风险排序稳定；未知项按设备 ID 排序。
7. 声明和实际上报不一致会在 `declaration_mismatches` 中给出具体服务器和原因。

服务器结果可直接定位 `server_id`、A/B `power_paths`、`pdu_id`、`port`、`ups_id`、`upstream_branch_id`、重合层级和声明差异。

## 下电编排

### 计划内检修

`POST /api/v1/plans` 支持关停 `ups` 或 `pdu`：

```json
{
  "batch_id": "OPS-20261119-001",
  "target_type": "pdu",
  "target_id": "PDU-AUX-17",
  "acceptable_server_ids": []
}
```

结果按以下顺序排列，每台服务器只出现一次：

1. `full_outage`
2. `one_path_loss`
3. 不受影响服务器不进入 `servers`

每项包含 `affected_paths`、`unaffected_paths`、`reason_code`、`reason` 和可中断属性。只要存在未获豁免的全失电服务器，`executable` 为 `false`，并返回：

- `blocking_reasons`
- `blocked_servers`
- `waivable_servers`

`acceptable_server_ids` 会与资产中的 `acceptable_outage: true` 合并。目标设备拓扑未知或存在相关未知路径时也采用保守阻断，不会凭声明宣称“不会全失电”。

### 紧急下电

`POST /api/v1/emergency-plans` 自动选出处于电池供电且低于 `battery_threshold_percent` 的 UPS：

1. 优先级 1：只有该 UPS 一路供电，或全部已知路径都依赖该 UPS 的服务器。
2. 优先级 2：还有另一路 UPS，但其余量不足以承接该服务器负载的服务器。
3. 同优先级按 `business_cost` 升序，再按 `server_id` 升序，保证稳定。

返回的 `sequence` 中每项都有 `position`、`priority`、`affected_paths`、`failing_ups_ids`、`alternate_ups`、容量判断依据和业务代价。可用以下接口回答单台服务器的位次：

```text
GET /api/v1/plans/{plan_id}/servers/{server_id}
```

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/healthz` | 健康检查 |
| `POST` | `/api/v1/audits` | 采集并提交审计，body 可带 `batch_id` |
| `GET` | `/api/v1/audits?limit=50` | 历史审计批次 |
| `GET` | `/api/v1/audits/{batch_id}` | 完整审计结论 |
| `GET` | `/api/v1/audits/{batch_id}/exposure` | 风险排序和未知设备 |
| `GET` | `/api/v1/audits/{batch_id}/servers/{server_id}` | 单台服务器路径和判定 |
| `POST` | `/api/v1/plans` | 创建检修下电计划 |
| `GET` | `/api/v1/plans?batch_id=...` | 历史计划摘要 |
| `GET` | `/api/v1/plans/{plan_id}` | 完整计划 |
| `GET` | `/api/v1/plans/{plan_id}/servers/{server_id}` | 服务器在计划中的位置与原因 |
| `POST` | `/api/v1/emergency-plans` | 创建紧急序列 |
| `GET` | `/api/v1/status?batch_id=...` | 未知设备、最高风险和最近计划 |
| `GET` | `/api/v1/config` | 不含绝对路径的公开配置 |
| `GET` | `/metrics` | Prometheus 文本 |

JSON 响应使用固定键排序；服务器、设备、风险列表均为确定性顺序。状态接口不包含配置文件、数据库或工作区绝对路径。

## 持久化与错误码

SQLite 默认位于 `runtime/power_audit.db`，启用 WAL。一次审计的批头、结论、96 台服务器、每条供电路径和设备暴露面在同一个事务中提交；任一步失败整体回滚。

关键业务错误：

| HTTP | 错误码 | 含义 |
|---|---|---|
| `409` | `AUDIT_BATCH_EXISTS` | 批次已存在，拒绝覆盖 |
| `404` | `AUDIT_NOT_FOUND` | 指定批次不存在 |
| `404` | `PLAN_NOT_FOUND` | 指定计划不存在 |
| `400` | `INVALID_REQUEST` | 请求字段不合法 |
| `400` | `INVALID_TARGET_TYPE` | 下电目标类型不合法 |
| `404` | `UNKNOWN_DEVICE` | 目标设备不存在 |
| `422` | `NO_EMERGENCY_CONDITION` | 没有满足低电量条件的 UPS |
| `500` | `INTERNAL_ERROR` | 未预期内部错误 |

## Prometheus 指标

每台 UPS/PDU 均暴露下列指标，标签固定为 `device_id` 和 `device_type`：

- `poweraudit_device_online`
- `poweraudit_device_server_count`
- `poweraudit_device_all_outage_servers`
- `poweraudit_device_one_path_loss_servers`
- `poweraudit_device_pseudo_redundant_servers`
- `poweraudit_device_single_path_servers`
- `poweraudit_device_topology_known`

批次和计划指标：

- `poweraudit_current_audit_batch_info{batch_id="..."}`
- `poweraudit_audit_servers{redundancy_status="..."}`
- `poweraudit_topology_unknown_devices`
- `poweraudit_infeasible_plans`
- `poweraudit_plans`

## 资源与测试

实现只使用 Python 标准库、SQLite 和单进程 UDP/HTTP；pysnmp 后端为一个引擎承载全部 21 个模拟端点，不启动重复服务。默认并发为 8 个短生命周期 SNMP 工作线程，单次 SNMP UDP 报文最多 24 个 varbind。

运行全部测试：

```powershell
python -m unittest discover -s tests -v
```

测试覆盖：

- SNMP GET 回环和只读 SET 拒绝。
- 96 台服务器的 66/24/6 独立、伪冗余、单路判定。
- 同 PDU、同 UPS、同支路三级重合。
- 风险排序和具体服务器暴露清单。
- 不可达 PDU 的未知分组和下游路径。
- 重复批次不覆盖、重启后历史查询。
- 检修计划阻断/豁免、紧急顺序与单位次查询。
- Prometheus 和 JSON 状态接口。
- 全设备不可达时的总审计硬时限。

演示数据的预期审计结果为：`independent=66`、`pseudo_redundant=24`、`single_path=6`、`topology_unknown=0`；`PDU-AUX-17` 下挂 14 台全失电服务器。

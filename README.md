# 机架带外管理仿真与合规巡检

面向混合固件机架的轻量仿真与巡检服务。6 台 BMC、3 个固件版本、IPMI 2.0
RMCP+、Redfish HTTPS 与 SNMPv2c 都在一个 Python 进程内运行；运行时无第三方
Python 依赖，适合 Windows 10/11 或 Windows Server 2019+。

## 资源与拓扑

- 6 台逻辑 BMC，其中 2 台旧固件、2 台中间固件、2 台新固件。
- 每台 2 个 PSU、4 个风扇、2 个温度传感器；另有 1 个系统电源状态。
- 巡检线程池固定最多 2 个 worker，数据库使用本地 SQLite WAL。
- 不启动数据库、消息队列、容器或额外重服务；全部监听地址均为 `127.0.0.1`。
- 旧固件只打开 IPMI/SNMP 监听；其清单中的 Redfish 端口保留为“不支持”，
  不会绑定或伪造响应。

| 用途 | 端口 |
| --- | --- |
| IPMI RMCP+ | `6230`-`6235` |
| Redfish HTTPS | `8443`-`8448` |
| SNMPv2c UDP | `1610`-`1615` |
| Prometheus/JSON 状态 | `9470` |
| 故障注入控制面 | `9471` |

## 快速启动

PowerShell：

```powershell
py -3.11 -m oob_rack serve
```

Linux/macOS 或 Windows 终端中的 Python：

```bash
python3 -m oob_rack serve
```

首次启动会按 `config/rack.json` 创建 `artifacts/inspections.db`。服务立即执行
一轮巡检，之后按配置周期运行。按 `Ctrl+C` 退出。

常用命令：

```bash
python3 -m oob_rack generate
python3 -m oob_rack serve --once --json
python3 -m oob_rack emulator
python3 -m oob_rack inspect
python3 -m oob_rack status
python3 -m oob_rack faults list
```

`serve --once` 在进程内启动完整仿真器、执行一轮巡检后退出；`emulator` 只启动
BMC 与故障控制面；`inspect` 对已经在运行的仿真器执行一轮巡检。

## 固件行为

| Profile | 协议 | Redfish 电源状态 |
| --- | --- | --- |
| `legacy-1.2` | IPMI、SNMPv2c | 无 Redfish |
| `mid-2.4` | Redfish、IPMI、SNMPv2c | 字符串枚举，如 `"On"`、`"Off"` |
| `new-3.0` | Redfish、IPMI、SNMPv2c | 对象，如 `{"State":"On","Health":"OK"}`，并带 `Actions` |

传感器读数由 `seed + device_id + firmware + sensor_name` 决定；进程重启后不变。
故障注入只覆盖指定设备/传感器，不改变其他设备的确定性基线。

## 归一化与降级

- 电源状态统一为 `on`、`off`、`unknown`。
- 温度统一为摄氏度；风扇统一为 RPM。每个归一化传感器都带 `lower`、`upper`。
- 未知字段形态会记入 `unparsable`，保留截断的原始片段，并置对应值为不可解析；
  不会把不可解析当作离线，也不会当作正常读数。
- 清单中的 `fallback_protocols` 决定采集优先级。某协议不可达时继续尝试后续
  协议；降级成功只增加 `fallback_used` 和“成功来源”，不生成 `unsupported` 标记。
- IPMI 与 Redfish 都成功且字段可解析时，逐项比较电源和同名传感器。只在双方
  都实际存在的值上比较，不补值、不伪造缺失读数。

## 合规状态

每个固件 profile 在配置的 `thresholds` 中声明：

```json
{
  "temperature_c": {"lower": 5.0, "upper": 66.0},
  "fan_rpm": {"lower": 800, "upper": 8000}
}
```

同一设备同一传感器持续越限只维护一条 `active` 违规，并在每轮巡检累加持续
时间；恢复读取后写入 `resolved` 状态、解除事件和 `violation_resolutions`
记录。设备不可达或字段不可解析时不会误解除既有违规。

配置重载：

```bash
curl -X POST http://127.0.0.1:9470/admin/reload
```

阈值非法时返回稳定业务码 `E_THRESHOLD_INVALID`（HTTP 409），当前配置继续生效。
其他配置错误码包括 `E_CONFIG_READ`、`E_CONFIG_SCHEMA`、`E_CONFIG_IMMUTABLE`。

## 数据与查询

SQLite 表包括：

- `inspections`、`inspection_devices`、`protocol_results`
- `sensor_observations`
- `violations`、`violation_events`、`violation_resolutions`
- `inconsistencies`、`unparsable_fields`

每台设备的结果、原始响应、归一化结果、传感器观测和违规判定在同一事务内提交。
巡检中断后重启会复用该 `running` 巡检，只采集尚未提交的设备。

接口：

```text
GET  /status?rack=rack-a
GET  /metrics
GET  /health
GET  /api/config
GET  /api/inspections/{id}
GET  /api/sensors?device_id=bmc-01&sensor=CPU%20Temp&from=...&to=...&limit=100
POST /admin/reload
```

`/status` 与 `/api/inspections/{id}` 使用 `sort_keys=True` 输出，不包含工作区绝对
路径。`/api/inspections/{id}` 同时给出归一化结果与各协议原始响应，便于核对。

Prometheus 指标名称固定：

```text
oob_collection_success_total{device="bmc-01"}
oob_protocol_failures_total{device="bmc-01",protocol="redfish"}
oob_fallback_collections_total{device="bmc-01"}
oob_active_violations{device="bmc-01"}
oob_unparsable_fields_total{device="bmc-01"}
```

## 故障注入

控制面只绑定 `127.0.0.1:9471`。示例：

```bash
curl -X POST http://127.0.0.1:9471/faults \
  -H "Content-Type: application/json" \
  -d '{"device_id":"bmc-02","kind":"offline","protocol":"redfish"}'

curl -X POST http://127.0.0.1:9471/faults \
  -H "Content-Type: application/json" \
  -d '{"device_id":"bmc-05","kind":"redfish_power_unparsable"}'

curl -X POST http://127.0.0.1:9471/faults \
  -H "Content-Type: application/json" \
  -d '{"device_id":"bmc-03","kind":"power_override","protocol":"ipmi","value":"on"}'

curl -X POST http://127.0.0.1:9471/faults \
  -H "Content-Type: application/json" \
  -d '{"device_id":"bmc-01","kind":"sensor_override","sensor":"CPU Temp","value":99}'

curl -X DELETE "http://127.0.0.1:9471/faults?device_id=bmc-01"
```

支持的故障：`offline`、`sensor_override`、`power_override`、
`redfish_power_unparsable`、`redfish_sensor_unparsable`。

## 人工核对

仿真器使用自签证书，Python 客户端和 Redfish 客户端均按
`config/rack.json -> redfish.ca_file` 校验。防火墙或代理不应改写 localhost。

IPMI 使用 RMCP+ HMAC-SHA1（cipher suite 1），人工核对时显式指定 `-C 1`：

```bash
ipmitool -I lanplus -H 127.0.0.1 -p 6230 -U admin -P admin123 -C 1 mc info
ipmitool -I lanplus -H 127.0.0.1 -p 6230 -U admin -P admin123 -C 1 chassis power status
ipmitool -I lanplus -H 127.0.0.1 -p 6230 -U admin -P admin123 -C 1 sensor list
ipmitool -I lanplus -H 127.0.0.1 -p 6230 -U admin -P admin123 -C 1 fru print
```

Redfish：

```bash
curl --cacert certs/bmc-cert.pem -u admin:admin123 \
  https://127.0.0.1:8445/redfish/v1/Chassis/1/Power
```

SNMPv2c 社区名为 `public`；也可用服务自身的标准库客户端通过 `inspect` 验证。

## 验证

```bash
python3 -m unittest discover -s tests -v
python3 scripts/acceptance.py
```

完整验收结果在 `artifacts/acceptance-report.json`，包含基线 6 设备结果、故障
降级、不可解析字段、跨协议不一致、违规持续/解除、指标样本和最终状态快照。
标准 `ipmitool` 的电源、传感器、FRU 人工核对输出见
`artifacts/ipmitool-check.txt`。

最近一次验收：15/15 项通过；基线 6 台设备全部采集成功，旧固件使用
IPMI/SNMP，中间与新固件使用 Redfish/IPMI/SNMP；无伪不一致；故障场景均按
预期降级、报告不可解析或不一致，并在恢复后生成解除记录。

## 实现边界

- 仿真 IPMI 使用 RMCP+ HMAC-SHA1、无会话加密（suite 1），没有实现 AES-CBC
  密文会话；巡检客户端与仿真器均使用标准库 HMAC/RMCP+。
- 旧固件声明 Redfish 端口但不绑定，以表达“真实不存在的接口”。
- 自签证书只用于 localhost 仿真；生产部署必须替换证书和凭据。

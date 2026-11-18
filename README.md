# feedback_ZSL — 轻量 SNMP 采集与告警服务

纯 Python 标准库（`socket` / `sqlite3` / `threading` / `json`）实现，无需第三方
SNMP 库或外部服务，可在 Windows 10/11、Windows Server 2022 或 Linux 上运行
（Python 3.11+）。单进程内以 3 个 UDP 仿真 Agent + 3 个采集线程运行，资源占用
远低于 2 逻辑 CPU / 1.25GiB 内存的限制。

## 组成

- `snmpmon/ber.py` — 最小 BER 编解码（INTEGER/OID/TimeTicks/Counter32 等）
- `snmpmon/snmp.py` — SNMPv2c GetRequest/GetNextRequest/Response 消息编解码
- `snmpmon/simulator.py` — 本地 SNMP Agent 仿真器：3 台交换机各 16 口（共 48 口），
  暴露 `sysUpTime`、`ifDescr`、`ifAdminStatus`、`ifOperStatus`、`ifInOctets`、
  `ifOutOctets`；计数器由固定随机种子按逻辑 tick 确定性生成，tick 持久化到
  `sim_state/`，重启后计数连续；支持配置注入延迟（`latency_ms`）、丢包
  （`loss_percent`）、无响应窗口（`dead_request_windows`）、设备重启
  （`restart_at_ticks`）与接口抖动（`flaps`）
- `snmpmon/collector.py` — 轮询采集器：GETNEXT 发现接口、多变绑 GET 采集；
  超时有限重试、连续失败冷却跳过；32 位计数器回绕按 `(cur-prev) mod 2^32`
  计算增量；sysUpTime 回退时增量标记 `unavailable/device_restart`；
  ifOperStatus up→down 告警、恢复清除、去重窗口内重开而非重复告警
- `snmpmon/storage.py` — SQLite 时序库，`(device, cycle, oid)` 主键去重，
  每周期单事务写入，重复启动不重复写同周期数据
- `run_demo.py` — 一键演示：启动仿真器 + 采集器，输出确定性 JSON 报告
- `tests/restart_continuity.sh` — 仿真器/采集器重启连续性与幂等性验证

## 运行

```bash
python run_demo.py --cycles 200     # 完整演示，输出 report.json
python run_demo.py --cycles 200     # 再次运行，report.json 逐字节一致
```

分进程模式（演示仿真器重启后计数器连续）：

```bash
python -m snmpmon.simulator --config config.json &
python -m snmpmon.collector --config config.json --cycles 50
bash tests/restart_continuity.sh
```

## 报告

`report.json` 含每台设备的采集轮次、成功/失败/冷却跳过次数、超时次数、
告警产生与清除条数、重启导致的不可用增量记录数、各接口最新计数值；
字段稳定排序，不含绝对路径与时间戳，同配置同种子重复运行逐字节一致。

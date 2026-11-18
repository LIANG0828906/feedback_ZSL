# 园区视频网关中继

面向单机 2 核 / 1.25 GiB Windows 主机的 UDP+TCP 网关中继。仓库包含中继、
两个 NAT 行为模拟器、固定种子的 12 个网关客户端、确定性故障注入场景和
JSON 验收报告。

## 实现要点

- **双协议独立映射**：UDP 控制入口只分配 UDP 映射，TCP 控制入口只分配 TCP
  映射。端口分配器在所有网关、两种协议之间做全局去重。相同
  `gateway_id + protocol` 的重复申请返回原映射。
- **NAT 探测**：从控制端口和一个未知临时端口分别向网关 NAT 外部端点发送
  reflection probe。两个来源都收到回复判定为 `full_cone`，仅控制端口收到
  判定为 `port_restricted`。声明类型与实测不符只标注 `type_mismatch` 并保存
  完整证据，不拒绝映射，也不参与转发判定。
- **生命周期**：心跳刷新租约；超时后先进入 draining 窗口，让在途数据处理
  完成，再关闭监听并写入回收记录。释放端口在冷却期内不会被任何网关再次
  分配。重新申请会得到新端口。
- **重启恢复**：映射、绑定、探测证据、令牌、租约和回收计数存入 SQLite
  (WAL)。启动时只恢复未过期映射并重新绑定原端口；过期记录直接回收并记录
  `lease_expired_during_restore`。旧端口心跳按网关身份命中已有映射。
- **来源校验**：UDP 按已绑定 NAT 外部 IP:端口精确校验；TCP 数据连接必须用
  映射令牌绑定，随后按连接的精确 IP:端口校验。其他 UDP 来源或未绑定 TCP
  连接被丢弃并累计 `source_mismatch_drops`。NAT rebinding 只有在探测成功后
  才原子替换绑定，旧来源包不会转发给新绑定。
- **转发落点**：`--forward-udp` / `--forward-tcp` 可指定本地 sink。通过来源
  校验的数据会先写入 sink，再向网关返回 ACK。场景运行会自动启动独立 sink
  并核对实际收包数。
- **状态与指标**：JSON 状态按网关和 `udp,tcp` 稳定排序；Prometheus 文本按
  网关稳定排序。指标标签固定为 `gateway` 以及映射类指标的 `protocol`。

## 运行

要求 Go 1.22+。SQLite 驱动使用 `modernc.org/sqlite`，纯 Go，无需 CGO 或
本机安装 SQLite 动态库，Windows 可直接构建。

```bash
go test ./...

# 固定种子样本已经签入；需要重新生成时：
python3 scripts/generate_gateways.py

go run ./cmd/validate \
  -samples samples/gateways.json \
  -out artifacts/validation-report.json \
  -work-dir artifacts/validation-run \
  -gomaxprocs 2 \
  -memory-limit-mib 1250
```

Windows PowerShell：

```powershell
$env:GOMAXPROCS = "2"
go run ./cmd/validate `
  -samples samples/gateways.json `
  -out artifacts/validation-report.json `
  -work-dir artifacts/validation-run `
  -gomaxprocs 2 `
  -memory-limit-mib 1250
```

也可使用 `scripts/run-validation.ps1`。报告中的 `summary.all_passed` 为
`true` 时进程返回 0。故障注入全部由场景驱动器通过模拟器和客户端完成，不修
改中继源码。

单独启动中继：

```bash
go run ./cmd/relay \
  -control-udp 127.0.0.1:17000 \
  -control-tcp 127.0.0.1:17001 \
  -http 127.0.0.1:17080 \
  -db relay.db \
  -port-min 21000 -port-max 21999 \
  -lease-window 8s -cooldown 3s -drain 250ms
```

## 查询接口

```text
GET /healthz
GET /v1/status?gateway_id=gw-001
GET /v1/status                         # 所有网关，按 gateway_id 排序
GET /v1/recycles?gateway_id=gw-001
GET /metrics
```

`/v1/status` 返回指定网关的活动映射、UDP/TCP 端口、绑定源、探测证据、
租约剩余时间、最近回收原因、来源不符数和回收次数。映射按 `udp` 后 `tcp`
排序；所有 Go struct 字段顺序固定。

Prometheus 指标：

```text
relay_gateway_active_mappings{gateway}
relay_mapping_active{gateway,protocol}
relay_mapping_port{gateway,protocol}
relay_mapping_keepalive_remaining_seconds{gateway,protocol}
relay_mapping_forwarded_total{gateway,protocol}
relay_gateway_source_mismatch_drops_total{gateway}
relay_gateway_nat_type_mismatch_total{gateway}
relay_gateway_recycles_total{gateway}
```

## 本次验收结果

结果文件：`artifacts/validation-report.json`；资源统计：
`artifacts/resource-usage.txt`。

- 18 项检查全部通过，12 个网关同时存在 24 条活动映射，24 个映射端口全局互异。
- 固定种子样本中的 `gw-003` 声明类型错误，被标注 `type_mismatch` 且保留
  双源探测证据；运行中注入的双向类型变化同样被重新测出。
- 断连的 `gw-004`、`gw-011` 各回收 UDP/TCP 两条映射；冷却期内重连均获得
  新端口；随后中继重启，12 个网关均恢复原端口且无重复映射。
- 25 个伪造或 NAT rebinding 后过期来源被丢弃并计数；有效来源数据实际到达
  UDP/TCP sink，收包数为
  49/48。
- 96 次时钟采样（UDP+TCP）的转发 P95 为 `0.789 ms`，低于 20 ms 上限。
- 运行参数 `GOMAXPROCS=2`、Go 内存上限 1250 MiB；实测峰值 RSS
  `21,572 KiB`、墙钟约 13 秒、运行目录约 196 KiB。Linux 验收额外使用
  `taskset -c 0,1` 将进程实际限制在两个逻辑处理器。

端口、探测时间和延迟会随本机调度变化；所有检查语义和固定种子输入保持确定。

## 本地 NAT 模拟说明

NAT 模拟器使用真实 loopback UDP/TCP 套接字作为网关外部端点，内部网关通过
模拟器的私网入口收发，因此中继侧看到的是实际外部 IP:端口。全锥型转发任意
来源；端口限制型只转发网关此前主动联系过的目标。这样中继的双源探测能够
在不依赖真实公网、防火墙或运营商账号的情况下区分两种行为。

这是应用层 NAT 测试床，不尝试用 raw socket/iptables 复刻内核 NAT 实现；它
保留了本任务依赖的可达性、来源限制、端口绑定和 rebinding 语义。Windows 上
无需管理员权限。

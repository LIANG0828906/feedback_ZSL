# DNS 解析链路仿真与归因服务

`dnssim` 在单台 Windows/Linux 主机上启动一组仅监听回环地址的模拟 DNS
服务，并用真实 UDP/TCP DNS 报文逐跳执行查询。它用于复现多办公站点使用不同
递归解析器时的转发、委派、TTL、缓存污染、本地 hosts 和故障注入问题。

## 能力

- 根、TLD、权威、递归、转发器五类逻辑角色，全部通过 `127.0.0.1:high-port`
  通信。
- 委派、CNAME 链、NODATA、NXDOMAIN、SERVFAIL、EDNS(0)。
- 递归/转发器 TTL 缓存；记录 `hit`、`miss`、`preload`、`bypass` 等缓存状态。
- UDP `TC=1` 时保留首次截断记录并自动通过 TCP 重查，记录中同时保留两次尝试。
- 指定跳的超时、SERVFAIL、错误委派注入；支持按传输层和尝试次数匹配。
- 站点静态 hosts 映射、预置负缓存和预置污染缓存，并自动对照权威答案。
- 多站点多域名并发追踪、递归等待时释放全局查询槽位、追踪排队/拒绝业务码。
- 结果中无时间戳和绝对路径；字段与数组顺序稳定，重复运行仅 RTT 可变。
- 关闭时取消处理器、关闭 UDP/TCP 套接字并等待所有服务协程退出。

## 构建与运行

要求 Go 1.22 或更高版本。项目只使用标准库。

```powershell
go build -o dnssim.exe .\cmd\dnssim
.\dnssim.exe sample -out .\configs\sample-topology.json
.\dnssim.exe validate -config .\configs\sample-topology.json -check-ports
.\dnssim.exe trace -config .\configs\sample-topology.json `
  -site shanghai -domain www.example.test -pretty
```

`-check-ports` 会实际绑定并立即关闭每个配置端口。示例使用
`127.0.0.1:15351` 到 `15358`；如端口被占用，修改 JSON 地址或使用
`127.0.0.1:0` 让操作系统分配空闲高位端口。

启动 HTTP 服务：

```powershell
.\dnssim.exe serve -config .\configs\sample-topology.json -http 127.0.0.1:8080
```

程序会在标准输出打印实际 DNS 监听地址。`Ctrl+C` 或
`POST /api/v1/admin/shutdown` 触发优雅关闭。

## 配置模型

完整样本见 `configs/sample-topology.json`。关键字段如下。

```json
{
  "version": 1,
  "seed": 264264,
  "limits": {
    "max_concurrent_queries": 16,
    "max_concurrent_traces": 4,
    "trace_queue_size": 8,
    "default_timeout_ms": 400,
    "trace_queue_timeout_ms": 1500,
    "default_retries": 0
  },
  "zones": [
    {
      "name": "example.test.",
      "authority": "auth",
      "records": [
        {"name": "www.example.test.", "type": "CNAME", "ttl": 30, "value": "edge.example.test."},
        {"name": "edge.example.test.", "type": "A", "ttl": 30, "value": "10.20.0.10"}
      ]
    }
  ],
  "resolvers": [
    {
      "id": "root",
      "role": "root",
      "address": "127.0.0.1:15351",
      "latency_ms": 2,
      "delegations": [{"zone": "test.", "target": "tld", "ttl": 300}]
    },
    {
      "id": "rec-shanghai",
      "role": "recursive",
      "address": "127.0.0.1:15354",
      "upstreams": ["root"],
      "timeout_ms": 400,
      "retries": 0
    }
  ],
  "sites": [
    {
      "id": "nanjing",
      "recursive": "rec-shanghai",
      "static_mappings": {"www.example.test.": "10.20.0.77"},
      "negative_cache": [
        {"name": "api.example.test.", "type": "A", "rcode": "NXDOMAIN", "ttl": 90}
      ]
    }
  ],
  "faults": [
    {
      "id": "tld-servfail",
      "resolver": "tld",
      "domain": "servfail.example.test.",
      "type": "servfail"
    }
  ]
}
```

### 解析器

| 字段 | 说明 |
| --- | --- |
| `role` | `root`、`tld`、`authority`、`recursive` 或 `forwarder` |
| `address` | 必须为回环 `host:port`；端口 `0` 表示自动分配 |
| `upstreams` | 递归/转发器使用；当前按列表首项建立确定性转发路径 |
| `delegations` | 根/TLD 的委派关系，最长后缀匹配 |
| `latency_ms` | 进入该解析器时的固定模拟延迟 |
| `drop_rate` | `[0,1]`，按 seed、域名、类型与尝试序号确定性丢弃 |
| `timeout_ms` | 调用方等待该解析器的单次 UDP/TCP 超时 |
| `retries` | 无响应或 SERVFAIL 后的重试次数 |
| `truncate_udp` / `udp_max_bytes` | 强制截断或设置 UDP 响应上限 |
| `cache.preload` | 预置正/负缓存；用于复现旧地址或污染答案 |

`faults` 可放在顶层，也可放在解析器内。支持：

- `timeout`：不返回响应，调用方在超时后记录异常。
- `servfail`：立即返回 RCODE 2 并标记 `servfail`。
- `wrong_delegation`：返回委派和 glue 到 `wrong_target` 或
  `wrong_address`，并在委派服务器记录 `wrong_delegation`。

`attempts: [1]` 可只影响第一次线缆交互；配合 `retries` 可验证
“首包超时、重试恢复”。`transport: "udp"` 或 `"tcp"` 可限定注入传输层。

### 校验错误码

配置在绑定任何端口前完成校验。常见稳定业务码：

- `DNS_CFG_FORWARD_CYCLE`
- `DNS_CFG_DELEGATION_CYCLE`
- `DNS_CFG_MISSING_REF`
- `DNS_CFG_DUPLICATE_ID`
- `DNS_CFG_INVALID_ROLE`
- `DNS_CFG_INVALID_ADDRESS`
- `DNS_CFG_INVALID_FAULT`

`validate` 失败时输出 `{"ok":false,"error":{"code":...}}`。

## HTTP API

### 提交追踪

```http
POST /api/v1/traces
Content-Type: application/json

{"site":"shanghai","domain":"www.example.test","type":"A"}
```

`type` 支持 `A`、`AAAA`、`CNAME`、`NS`、`TXT`。`bypass_cache:true`
可强制本次查询跳过递归缓存。

### 静态映射

```http
GET    /api/v1/sites/{site}/static-mappings
POST   /api/v1/sites/{site}/static-mappings
PUT    /api/v1/sites/{site}/static-mappings
DELETE /api/v1/sites/{site}/static-mappings?name=www.example.test
DELETE /api/v1/sites/{site}/static-mappings?all=true
```

POST/PUT 请求体：

```json
{"name":"www.example.test","value":"10.20.0.77"}
```

删除不存在条目是幂等成功，`removed` 为 `false` 或 `0`。

### 其他端点

- `GET /healthz`
- `GET /api/v1/stats`
- `GET /api/v1/sites`
- `POST /api/v1/admin/shutdown`

追踪记录不会因队列拥塞静默丢失。追踪槽位耗尽时按
`trace_queue_timeout_ms` 排队；队列满返回 HTTP 429 `DNS_BUSY`，排队超时
返回 HTTP 429 `DNS_QUEUE_TIMEOUT`。

## 结果与归因

每次追踪输出一个 `TraceResult`：

- `hops`：按请求发起顺序排列的线缆交互。每项包含 `resolver`、`role`、
  `server`、`transport`、`attempt`、`message_type`、`rcode`、`ttl`、
  `answers`、`cache_status`、`truncated`、`timed_out`、`rtt_ms`。
- `resolution`：最终应用可见答案，`source` 为 `dns`、`hosts` 或
  `negative_cache`。
- `authoritative`：绕过缓存与注入、直接查询权威服务器得到的对照答案。
- `conclusion`：`SUCCESS`、`NXDOMAIN`、`TIMEOUT`、`SERVFAIL`、
  `ERROR` 或 `INCONSISTENT`。
- `mismatch_cause`：`none`、`static_mapping`、`negative_cache`、
  `cache_poisoning`、`link_fault`、`answer_mismatch` 或
  `authority_unavailable`。
- `attribution`：链路顺序最早的异常跳。字段包括服务器角色、地址、异常类型
  和说明。

归因以请求路径顺序为准，不以 RTT 最大者为准。父递归因下游超时而自身
超时的记录会被识别为传播性超时，真正故障保留在更深的首次异常跳；如果
递归解析器本身被注入超时，则不会被误过滤。

静态映射和负缓存只有在与权威答案不一致时才标记为异常；故障注入未匹配
任何跳时不会产生虚构的 `attribution`。

## TTL 与恢复验证

`configs/sample-topology.json` 包含：

- `shanghai`：正常逐跳解析。
- `shenzhen`：递归 → 转发器 → 根 → TLD → 权威。
- `hangzhou`：预置污染缓存，解析到旧地址 `10.20.0.99`。
- `nanjing`：hosts 静态映射，结论 `INCONSISTENT/static_mapping`。
- `beijing`：预置 NXDOMAIN 负缓存。
- `wuhan`：TLD 首次超时、自动重试成功，最终成功但仍归因首次超时。
- `servfail.example.test`、`wrong.example.test`：SERVFAIL 和错误委派注入。
- `big.example.test`（`TXT`）：UDP 截断后保留截断记录并 TCP 重查。
- `short.example.test`：TTL 1 秒，用于观察缓存命中及过期回源。

站点预置负缓存同样按 TTL 到期；自动化测试覆盖了过期后重新走完整递归链。

通过 HTTP 删除 `nanjing` 的 hosts 映射后再次追踪，结果会回到权威答案
`10.20.0.10`。

## 验证

```powershell
go test -race ./...
go vet ./...
$env:GOOS="windows"; go build -o dnssim.exe .\cmd\dnssim
```

测试覆盖逐跳委派链、缓存命中与过期、UDP/TCP 双记录、静态映射清除、
负缓存、最早故障归因、瞬态超时恢复、无故障不误报、并发查询上限、
关闭后端口释放和 HTTP 映射接口。

本次验收的机器可读摘要见 `artifacts/verification-report.json`。

同一固定端口拓扑在全新进程中重复运行时，除 `rtt_ms` 外结果逐字节一致。
若配置使用端口 `0`，实际绑定端口由操作系统分配，`server` 字段会随运行
变化，这是唯一例外。

递归链路通过 EDNS 私有选项传播剩余超时预算。上层递归会在自身客户端超时前
给下游故障留出收敛窗口，因此归因不会错误落到“等不到子查询的父递归”。

## 资源边界

CLI 在未显式设置 `GOMAXPROCS` 时最多使用 2 个逻辑处理器。示例拓扑只有
8 个回环监听器，无数据库和外部网络；自动化测试约 5 秒内完成，常驻内存
远低于 1.25 GiB。所有端口绑定失败都会在对外提供服务前整体回滚。

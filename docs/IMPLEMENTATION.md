# 实现说明

本仓库在空 Go 模块上独立实现了确定性的隧道/VPN 转发工程工作台。所有样本、事件、报文和真值均由固定种子 `42042` 的生成器在本地产生；SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，运行和测试不依赖容器、真实设备或云服务。

## 代码结构

- `internal/forward/model.go`：规范内部模型、动作、事件和三版本常量。
- `internal/forward/generator.go`：固定种子工程、18 个逻辑事件、8 个报文和生成器真值。
- `internal/forward/render.go`：把同一规范样本渲染成三个 schema 的 JSON 文档。
- `internal/forward/loader*.go`：三版本识别、字段路径错误、单位换算、地址/网段归一和嵌套树扁平化。
- `internal/forward/validate.go`：引用完整性、MTU/保活/端口校验和严格重叠规则检查。
- `internal/forward/engine.go`：四态状态机、逐事件报告、报文裁决与分片串联。
- `internal/forward/matcher.go`：网段、域名后缀、端口区间、显式优先级、同优先级最长前缀和声明次序裁决。
- `internal/forward/fragment.go`：外层/加密开销、Path MTU、8 字节对齐、位宽检查溢出与饱和。
- `internal/forward/scheduler.go`：worker 预算、每隧道有界队列、确定性背压和按隧道保序的并行执行。
- `internal/forward/incremental.go`：依赖图、失效闭包、缓存复用、编辑重放和一次性全量对照。
- `internal/checkpoint/store.go`：SQLite schema、截断/版本检查、状态历史和分流缓存。
- `internal/checkpoint/runner.go`：逐事件两阶段提交、信号可中断入口和恢复后缀重放。
- `internal/workbench/`：HTTP API、嵌入的单页诊断台和 12 类异常注入探针。
- `internal/verify/verify.go`：43 项本地验收检查，`go test` 与 `tunnelbench verify` 共用。
- `cmd/tunnelbench/`：`verify`、`gen`、`serve`、`repl`、`inject`、`run-checkpoint` 命令。

## 三版本归一

| 维度 | v1 | v2 | v3 | 内部模型 |
| --- | --- | --- | --- | --- |
| 版本值 | `1.0` / `v1` | `2.0` / `v2` | `3.0` / `v3` | `v1|v2|v3` |
| 组织 | `tunnel_table` + `split_rules` 扁平表 | `forward_tunnels` + `shunt_rules` 扁平表 | `peer_tree[].tunnels/policy_set` 嵌套树 | 排序后的 `Tunnels/Rules` |
| 网段 | `{address,netmask}` 点分掩码 | CIDR 字符串 | `{address,prefix_length}` 整型前缀 | `net.IPNet` 规范 CIDR |
| 保活 | `keepalive_seconds` | `keepalive_ms` | `keepalive_interval_ms` | 毫秒 |
| 速率 | `rate_bytes_per_second` | `rate_kib_s` | `rate_limit_kibps` | byte/s，KiB 按 1024 |
| 水位 | 无字段 | `watermark_bytes` | `watermark_kib` | `*int64` 字节；缺失即禁用 |

v1 按声明降级规则把所有隧道水位标为未启用，并在状态迁移和报告中输出
`W_WATERMARK_SCHEMA_FIELD_ABSENT:degrade_to_disabled`。v2/v3 中单条隧道仍可缺省水位，
该隧道会得到 `W_WATERMARK_FIELD_ABSENT:disabled_for_tunnel`。

装载后隧道按 `decl_order` 排序，对端按声明顺序排序，规则按
`(priority, decl_order)` 排序。验收器逐项比较三个 schema 的隧道 ID、对端成员和规则优先序，
并比较裁决路径与分片签名。

## 状态机

状态为：

- `unestablished`：未建立/初始；
- `handshaking`：握手中；
- `established`：已建立；
- `disconnected`：断链。

合法迁移：

- `connect`：`unestablished|disconnected -> handshaking`；
- `handshake_complete`：`handshaking -> established`；
- `disconnect`：`handshaking|established -> disconnected`；
- `keepalive_timeout`：`established|handshaking -> disconnected`。

在 `unestablished` 上直接完成握手会返回 `E_STATE_SKIP_INTERMEDIATE`，不会跳到
`established`。事件时间统一为逻辑毫秒；v1 秒字段按 `round(seconds * 1000)` 归一。

## 分流裁决

每条规则的网段、域名后缀、端口区间三类条件是 AND 关系；同一类别中的多个候选项是 OR 关系。

裁决按显式 `priority` 从小到大访问优先级组。同一优先级组内：

1. 选择命中 CIDR 中前缀最长的规则；
2. 前缀相同则选择 `decl_order` 更小的声明规则。

找到命中优先级组后停止，低优先级规则不再影响结论。报告输出所有已评估步骤、命中规则、
选中规则（`★`）、依据编号（含前缀和声明次序）以及完整依赖。未命中时使用
`project.fallback`，依据为 `fallback:declared_action`，来源为 `project.fallback`。

## 分片模型

本实现把归一后的 `mtu_bytes` 定义为外层封包预算：

```text
outer_overhead = protocol_overhead + cipher_overhead
effective_budget = min(mtu_bytes, path_mtu_bytes)
inner_cap = effective_budget - outer_overhead
payload_cap = inner_cap - 20
aligned_cap = floor(payload_cap / 8) * 8
```

非末片按 `aligned_cap` 切分，末片承载剩余字节。每片输出载荷字节、偏移、8 字节偏移单位、
内层字节和外层总字节；报文输出片数和总外层字节。

- `mtu_bytes < protocol_overhead`：`E_MTU_TOO_SMALL`；
- 片数超过 `project.max_fragments`：`E_FRAG_TOO_MANY`；
- 声明 16/32/64 位整数无法容纳运算值：`E_FRAG_OVERFLOW`；
- 单调计数器饱和：`SaturatingAdd` 返回最大值并标记 `saturated=true`，不回绕。

## 增量失效闭包

每个缓存项记录依赖节点，例如：

- 状态：`tunnel:T1.peer_id`、`tunnel:T1.keepalive_ms`、`tunnel:T1.watermark`；
- 裁决：`rules-index`、所有已评估规则 `rule:R...`、`fallback:declared_action`；
- 分片：`result:verdict:<packet-id>`、`tunnel:T2.mtu`、`.protocol`、`.cipher`、`.path_mtu`、`.peer_id`。

编辑产生源节点（如 `tunnel:T2.mtu`）。失效 BFS 同时沿两类边传播：

1. 源节点 -> 直接依赖它的缓存项；
2. 被失效的结果节点 `result:<key>` -> 依赖该结果的下游缓存项。

因此规则编辑会失效裁决，再通过 `result:verdict:P...` 传播到对应分片。
`effective_from_seq` 之前的条目不进入失效集合，形成可复用的历史区间。响应中的
`traces[].status` 为 `hit|rebuilt`，重建项带 `invalidation_sources`，并输出闭包、
重建/复用序号和连续区间。每次编辑都另做一次从零全量时间线重放，用事件签名证明两者一致。

## 并行与背压

`Budget{Workers, PerTunnelBuffer}` 限制实际执行槽和每个隧道/分区的待处理 channel 容量。
同一隧道的状态事件和归属该隧道的报文进入同一有序队列，因此同隧道状态不会乱序；不同隧道可
并行执行。

- worker 槽全部占用时，非阻塞租约返回 `E_BACKPRESSURE_WORKER_BUDGET_EXHAUSTED`；
- 每隧道队列满时，`TrySubmit` 返回 `E_BACKPRESSURE_TUNNEL_BUFFER_FULL` 和当前 pending/limit；
- 普通 `Submit` 在队列满时阻塞，形成调用侧背压，不扩容、不丢事件。

验收器用 workers=1/2/4、buffer=1/2/8 多次运行，并比较顺序报告与并行报告的状态、裁决和分片签名；
`go test -race ./...` 通过。

## SQLite 检查点

数据库包含：

- `checkpoint_meta`：工程 ID、schema、规范 hash、`last_event_seq`；
- `tunnel_state_history`：`(tunnel_id,event_seq)` 状态历史；
- `route_cache`：裁决/分片 JSON、依赖和失效来源。

提交顺序刻意分为两个提交点：

1. 事务写隧道状态历史和分流缓存并 commit；
2. `CommitSequence` 更新 `checkpoint_meta.last_event_seq`。

如果进程在两步之间被杀死，全局序号仍停在上一完整事件；新进程只读取
`event_seq <= last_event_seq` 的状态历史，看不到“已写状态但未提交序号”的事件。恢复时用
已提交状态回填状态机，再重放序号之后的事件。schema 或 hash 不匹配时返回
`E_CHECKPOINT_VERSION_MISMATCH`，调用方丢弃该检查点并整段重放。截断/损坏库在打开时通过
SQLite 头和 `PRAGMA integrity_check` 映射为 `E_CHECKPOINT_TRUNCATED`。

`serve` 和 `run-checkpoint` 使用 `signal.NotifyContext(SIGINT, SIGTERM)`；事件循环在每个事件前
检查 context，因此信号打断后停在最后一个已完成的两阶段提交点。

## 工作台与 API

启动：

```bash
go run ./cmd/tunnelbench serve --addr 127.0.0.1:8088
```

浏览器打开 `http://127.0.0.1:8088/`。页面支持装载三版本、顺序/并行模拟、查看逐事件状态、
裁决路径、分片、施加编辑、查看增量闭包、检查点对照和 12 类异常注入。

常用 API：

- `GET /api/overview`
- `GET /api/equivalence`
- `GET /api/document?schema=v1|v2|v3`
- `GET /api/simulate?schema=v2&parallel=1&workers=4&buffer=8`
- `POST /api/edit`
- `GET /api/checkpoint?schema=v2&stop_after=7`
- `POST /api/inject`，body：`{"kind":"missing_peer"}`

终端 REPL：

```bash
go run ./cmd/tunnelbench repl
```

## 稳定异常码

| 注入 kind | 稳定码 |
| --- | --- |
| `overlap_rule` | `E_RULE_OVERLAP_CONFLICT` |
| `missing_peer` | `E_PEER_NOT_FOUND` |
| `mtu_too_small` | `E_MTU_TOO_SMALL` |
| `negative_keepalive` | `E_KEEPALIVE_NEGATIVE` |
| `field_conflict` | `E_FIELD_CONFLICT` |
| `unknown_schema` | `E_SCHEMA_UNKNOWN` |
| `budget_workers` | `E_BUDGET_WORKERS_OUT_OF_RANGE` |
| `budget_buffer` | `E_BUDGET_BUFFER_OUT_OF_RANGE` |
| `backpressure_workers` | `E_BACKPRESSURE_WORKER_BUDGET_EXHAUSTED` |
| `backpressure_buffer` | `E_BACKPRESSURE_TUNNEL_BUFFER_FULL` |
| `checkpoint_truncated` | `E_CHECKPOINT_TRUNCATED` |
| `checkpoint_version_mismatch` | `E_CHECKPOINT_VERSION_MISMATCH` |

所有 `BizError` 都带 `code`、可定位的 `path` 和可选 `context`。异常注入只操作克隆工程或
`artifacts/workbench/anomaly-*.db` 副本，不修改固定种子源样本，也不破坏主检查点。

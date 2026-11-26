# Warmcache 冷启动预热内核

这是一个只依赖 Go 标准库与系统 SQLite 运行库的本地可验收项目。它把“源站提交分片 → 边缘领取租约 → 共享带宽/并发错峰预热 → 写回闸门 → 提交可见”做成独立的确定性领域内核，并提供 `net/http` 诊断页面。

项目不访问外部服务，不使用 Docker。样本、请求脚本、运行时刻表均由固定种子的仓库内生成器产生。

## 快速开始

环境：

- Linux / WSL Ubuntu
- Go 1.22+
- `CGO_ENABLED=1`
- 系统 `libsqlite3.so.0`（Ubuntu 的 `libsqlite3-0` 包）；不依赖 `sqlite3` CLI，也不依赖开发头文件

```bash
go run ./cmd/warmcache -verify
go run ./cmd/warmcache
# 打开 http://127.0.0.1:8080
```

重新生成三版样本：

```bash
go run ./cmd/warmcache -write-samples -samples samples
```

执行全部测试：

```bash
go test ./... -count=1
go test -race ./...
go vet ./...
```

## 目录

```text
cmd/warmcache/              本地服务入口与验收命令
internal/domain/            归一化模型、业务码、运行期结构
internal/gen/               固定种子三版工程与请求生成器
internal/loader/            v1/v2/v3 装载与语义校验
internal/engine/            tick 调度、租约、预算、闸门、增量缓存
internal/sqlite/            仓库内 database/sql SQLite 驱动
internal/store/             原子检查点与恢复读取
internal/httpui/            API、诊断页面、异常注入
samples/v1.json             flat + tick + 千分比 + hex 摘要
samples/v2.json             flat + ms + 万分比 + hex 摘要
samples/v3.json             按副本组嵌套 + 秒/毫秒 + 万分比 + 整型指纹
```

## 三版 schema

三份样本描述完全相同的领域内容：

| 维度 | v1 | v2 | v3 |
|---|---|---|---|
| 分片清单 | `shards` 扁平表 | `replicaTable` 扁平表 | `replica_groups[].replicas` 嵌套树 |
| 时钟 | tick | 毫秒 | 秒 + 毫秒 |
| 租约 | `lease_ticks` | `leaseMs` | `lease_seconds` |
| 预热刻度 | tick | `preheatMs` | `preheat_ms` |
| 带宽刻度 | permille | basis points | basis points |
| 摘要 | 十六进制字面量 | 十六进制字面量 | 64 位整型指纹 |
| 预热水位 | 省略 | 省略 | `preheat_watermark` |

装载器统一到：

- 时间：整 tick；
- 带宽：milli-unit（千分比基准）；
- 摘要：`uint64`；
- 节点：ID、副本组、加入/退出 tick、领取间隔、租约时长、并发上限、缓冲上限；
- 分片：ID、归属节点、副本组、摘要、目标/上一版本、带宽消耗、预热步数。

固定种子 `4242` 的归一化语义哈希为：

```text
b3359053e528110caaace6798b219e9b14867181a0593e8349557e8285e2a1d4
```

当同一字段的多个别名给出不一致且工程没有 `preheat_watermark` 时，返回：

```text
E_SCHEMA_CONFLICT
```

水位字段存在时视为支持的消歧上下文字段，装载按版本主字段继续并保留稳定结果。

## tick 语义

每个逻辑 tick 固定按以下顺序推进：

1. 施加编辑（源站版本推进、节点加入/退出、预算编辑）；
2. 处理自然退出与中断；中断丢弃当前未提交步，但保留最近完整预热步；
3. 到期租约释放；
4. 各节点按 `(join_tick + n * claim_interval)` 产生领取意图；
5. 以确定性顺序分配唯一租约；
6. 按 `(入队 tick, 分片 ID)` 进行共享源站带宽与节点并发结算；
7. 预算或并发不足的分片进入错峰队列，不预占预算；
8. 完成预热的分片提交可见版本并释放租约；
9. 执行该 tick 的读写请求，读取和写入看到的是本 tick 提交后的状态。

租约有效区间为 `[acquired_tick, expires_tick)`。到期未完成时：

- 释放租约；
- 分片仍在待预热集合；
- 保留 `committed_progress`；
- 优先由上一持有者重新领取，否则其他节点可领取；
- `generation` 增加。

预热进度每 tick 原子推进一个完整步，`progress == committed_progress`。退出信号只停止在完整 tick 提交之后。

## 预算与排队约束

共享预算和节点并发使用以下约束编号：

- `C_BW_BUDGET`：共享源站带宽不足；
- `C_NODE_CONCURRENCY`：节点并发预热上限不足；
- `C_NODE_BUFFER`：节点待预热缓冲上限不足。

缓冲不足和线程不足属于可恢复背压，分别输出：

- `E_BACKPRESSURE_THREADS`
- `E_BACKPRESSURE_BUFFER`

它们不会越界分配或静默丢弃。非法线程/缓冲配置则返回：

- `E_THREAD_BUDGET_INVALID`
- `E_BUFFER_BUDGET_INVALID`

每节点实际结算耗时记录在 `result.diagnostics.nodes[].observed_ms`。

## 写回闸门

预热提交前：

- 普通读：`source=origin`，返回上一已提交版本；
- `fresh=true` 读：拒绝，`E_READ_FRESH_UNAVAILABLE`；
- 写目标版本：进入 `write-gate` 待回放队列，边缘不可见；
- 旧版本或未来版本写：`E_WRITE_VERSION_CONFLICT`；
- 已退役分片写：`E_WRITE_GATE_CLOSED`；
- 未知分片：`E_NOT_FOUND`。

提交后：

- 读切换到 `source=edge`；
- 返回目标版本并标记边缘可见；
- 待回放队列清空，后续写直接可见。

## 增量失效

支持编辑：

- `bump_shard`：源站分片版本推进；
- `retire_node`：节点退出；
- `join_node`：节点加入；
- `set_budget`：编辑共享带宽；
- `retire_shard`：分片退出；
- `set_node_limits`：编辑节点租约/并发/缓冲。

编辑前的 tick 直接复用基线快照。编辑 tick 起重算：

- 租约序列；
- 预算结算；
- 错峰队列；
- 写回闸门；
- 跨时刻依赖传播闭包。

每个受影响 tick 输出 `invalidation`，包含编辑来源、发生 tick、复用边界和传播闭包。`/api/edit` 同时执行一次全量基线，逐 tick 对照并返回 `matches_full=true`。

## SQLite 检查点

仓库内 `internal/sqlite` 通过 `database/sql/driver` 接口实现了一个极薄 SQLite 驱动。它不使用第三方 Go 包，也不需要 `sqlite3.h`，只连接系统 `libsqlite3.so.0`。

每个 tick 在单个事务中按固定顺序写入：

1. 分片租约、预热进度、可用版本、待回放；
2. 节点持租、预算占用、并发与缓冲；
3. 待回放队列、租约历史、闸门结果；
4. `tick_commit` 全局提交水位与 tick payload；
5. `runs.last_tick`。

因此进程在任意写入点终止，重开时都只接受已提交的水位。读取会校验：

- SQLite 文件头；
- `PRAGMA integrity_check`；
- 快照格式版本；
- 分片行不得越过全局水位；
- 全局 tick payload 哈希；
- 分片行数与提交 payload 一致。

相关业务码：

- `E_CHECKPOINT_TRUNCATED`
- `E_CHECKPOINT_CORRUPT`
- `E_CHECKPOINT_ORDER`
- `E_SNAPSHOT_VERSION`

## HTTP API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 本地诊断页面 |
| GET | `/api/health` | 健康与快照版本 |
| GET | `/api/projects` | 三版归一化工程与语义一致性 |
| GET | `/api/sample?version=v1` | 原始样本，响应头带源哈希与语义哈希 |
| GET/POST | `/api/run` | 运行租约/预算/闸门时间线 |
| POST | `/api/edit` | 增量编辑并与全量逐 tick 对照 |
| POST | `/api/checkpoint/compare` | 落盘、重开、继续与状态对照 |
| POST | `/api/inject` | 九类异常注入 |

运行示例：

```bash
curl -s 'http://127.0.0.1:8080/api/run?version=v1&ticks=30&workers=3'
```

异常注入示例：

```bash
curl -s -X POST http://127.0.0.1:8080/api/inject \
  -H 'Content-Type: application/json' \
  -d '{"version":"v2","kind":"truncated_checkpoint"}'
```

## 异常业务码

| 注入/场景 | 业务码 |
|---|---|
| 重复分片编号 | `E_DUP_SHARD` |
| 租约时长为零 | `E_ZERO_LEASE` |
| 带宽刻度越界 | `E_BAD_BANDWIDTH_SCALE` |
| 租约指向已退役节点 | `E_RETIRED_NODE` |
| 越界线程预算 | `E_THREAD_BUDGET_INVALID` |
| 越界缓冲预算 | `E_BUFFER_BUDGET_INVALID` |
| 截断检查点 | `E_CHECKPOINT_TRUNCATED` |
| 快照版本不匹配 | `E_SNAPSHOT_VERSION` |
| 无水位字段的语义冲突 | `E_SCHEMA_CONFLICT` |

所有错误响应格式：

```json
{
  "ok": false,
  "error": {
    "code": "E_XXX",
    "op": "locatable_operation",
    "message": "stable explanation",
    "context": {"path": "field/path", "value": "..."}
  }
}
```

异常注入只修改内存中的原始 JSON 副本或临时目录，不写 `samples/`。

## 已验证结果

在本机 Linux / Go 1.22.12 环境执行：

```text
$ go run ./cmd/warmcache -verify
PASS schema normalization: v1=v2=v3 b3359053e528110c
PASS lease/stagger timeline: 30 ticks, 17 leases, 9 commits
PASS write gate: fallback, defer, reject causes=map[E_NOT_FOUND:true E_READ_FRESH_UNAVAILABLE:true E_WRITE_VERSION_CONFLICT:true]
PASS interruption resume: shard-03 committed after re-lease
PASS incremental invalidation: incremental equals full recompute
PASS SQLite recovery: tick=30 snapshot=warmcache-checkpoint.v1
```

固定样本的诊断数字：

- 9 个分片、3 个节点、30 个逻辑 tick；
- 17 次租约，其中 9 次完成提交，8 次租约过期；
- 6 个分片发生过重复领取/重生；
- 峰值错峰队列 5，峰值共享带宽 190 / 200 milli-unit；
- 默认运行暴露 `E_BACKPRESSURE_BUFFER`，线程预算为 1 时暴露 `E_BACKPRESSURE_THREADS`；
- 写回闸门覆盖源站旧版本回退、强制新读拒绝、延迟回放、版本冲突与未知分片。

## 设计边界

- 这是单进程确定性内核；SQLite 持久化用于崩溃一致性与诊断，不用于分布式共识。
- 预算和并发以 tick 为单位重新结算，不在队列阶段预占。
- 增量重算对编辑 tick 之后采用保守传播：租约重生、共享预算和闸门都有跨时刻依赖，因此不把编辑后的单个 tick 当作独立可缓存单元。
- SQLite 驱动面向本项目的本地文件场景，支持 `database/sql` 所需的打开、查询、执行、事务与行读取；不实现 SQLite 扩展、备份或网络文件语义。

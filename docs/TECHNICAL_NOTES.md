# 灰度路由与流量镜像验收内核

本目录交付一个无外部服务依赖的 Go 内核。SQLite 使用纯 Go 驱动 `modernc.org/sqlite`，样本由固定种子 `4208691` 在本地生成。

## 1. 统一模型与三版 Schema

统一模型定义在 `internal/model/model.go`，核心字段如下：

- 工程版本：`engineering_version`；这是快照版本，不是 schema 版本。
- 规范比例刻度：万分点 `bps`，范围 `0..10000`。
- 逻辑时刻：整数 `tick`。V1 直接读取 `at_tick`；V2/V3 的毫秒时间除以 `tick_interval_ms` 归一。
- 规则顺序：服务内按 `priority DESC, order ASC, id ASC` 裁决。

三版样本分别是：

| Schema | 风格 | 比例 | 时间 | 规则组织 | 镜像采样字段 |
| --- | --- | --- | --- | --- | --- |
| `gray/v1` | snake_case / `upstreams` | `weight_permille` 千分比 | `at_tick` | `gray_rules` 扁平常量表 | 无该字段，内部为 `nil` |
| `gray/v2` | camelCase / `ruleTree` | `weightBps` 万分比 | `timestampMs` | 按服务分组树 | 可选 `sampleModulus` |
| `gray/v3` | 嵌套 `spec` / `ruleGroups` | `basisPoints` 万分比 | `occurredAt.timeMs` | 按服务分组规则树 | 可选 `sampling.modulus` |

千分比装载时乘以 10 转为万分点。若同时给出两种刻度且换算结果不一致，返回 `E_FIELD_AMBIGUOUS`；越界返回 `E_SCALE_RANGE`。

## 2. 稳定分流

规则命中条件是请求标签包含规则中的全部标签。多规则命中时记录：

- `candidate_rule_ids`：按裁决顺序排列的全部候选规则。
- `selected_rule_id` / `selected_rule_no`：最终规则与业务编号。
- `adjudication_basis`：优先级、声明次序、哈希桶和阈值。

稳定桶公式：

```text
bucket = FNV-1a32("grayrule:" + ruleID + NUL + stableKey) mod 10000
canary = bucket < weightBPS
```

因此 `500‰` 与 `5000‱` 使用同一个阈值和同一个桶。权重不变时，同一 `ruleID + key` 永远落在同一侧。权重从 `W0` 调整到 `W1` 时，只有桶位于新旧窗口对称差中的键会迁移；`internal/engine/router.go` 的 `EnumerateMigrations` 输出逐键迁移集合，生成器 `samples/truth.json` 记录对应真值。

`weightBPS=0` 时全部走 baseline；`weightBPS=10000` 时全部走 canary。

## 3. 镜像与资源隔离

镜像策略先做可选采样，再做比例判定：

```text
samplePass = sample == nil OR FNV-1a32("mirror-sample:" + policy + NUL + key) mod modulus == remainder
mirror     = samplePass AND StableBucket("mirror:" + policy, key) < ratioBPS
```

主链路结论在调用影子代理之前已经形成。影子的 `fulfilled`、`rejected`、`timeout` 只写入镜像结果，不回写 `side` 或 `main_allowed`。

资源边界为两套独立预算：

- 主链路：`WorkerBudget` 控制并行路由 worker；`RouteBufferPerService` 是每个逻辑时刻、每个服务的待路由准入上限。
- 影子链路：`MirrorConcurrencyPerService` 是每服务活跃影子调用上限；`ShadowBufferPerService` 是每服务等待队列上限。

影子队列满时：

- `shadow_on_full=drop`：请求得到 `shadow_status=dropped` 和 `E_SHADOW_BUFFER_FULL`。
- `shadow_on_full=defer`：请求得到 `shadow_status=deferred` 和同一约束码，供调用方稍后重试。

`TickReport` 逐时刻输出 `queue_occupancy`、`active_occupancy`、`dropped_set`、`deferred_set` 与 `main_allowed`。

并行准入以逻辑时刻为屏障：同一时刻内按输入顺序做确定性计数，再由 worker 池并行计算。这样 goroutine 调度顺序不会改变哪些请求占用缓冲，也不会改变分流、镜像集合和主链路结论。

## 4. 增量失效闭包

`internal/incremental` 以 `service#tick` 为缓存条目，保存：

- 该时刻的决策；
- 该服务的 `ServiceTickReport`；
- 时刻结束时可序列化的影子队列状态。

编辑入口是 `incremental.Edit`，支持 `rule_weight`、`rule_match`、`mirror_ratio`、`mirror_sample` 和 `topology_add_dependency`。

失效闭包由以下部分显式组成：

1. 字段依赖：例如 `field:mirror_sample:inventory:R-INV-CN`。
2. 拓扑依赖子树：从目标服务沿声明的下游依赖展开。
3. 稳定键传导：权重编辑列出实际迁移键及 `cross-time:mapping-persists-until-next-edit`。
4. 影子队列传导：`cross-time:shadow-queue-occupancy-propagates`。
5. 检查点传导：`checkpoint:route_mapping` 或 `checkpoint:mirror_queue_snapshot`。

重算从 `at_tick` 开始，并用 `at_tick-1` 的队列状态构造 suffix runtime；之前的 `service#tick` 标记为 `reused`。每个决策和报告都带 `cache_state` 与 `invalidation_sources`。`Report.EquivalentToFull` 会把增量结果与“旧配置跑到编辑点、新配置重算后缀”的一次性全量时间线结果逐请求比较。

整份工程快照的 `engineering_version` 改变时，`SnapshotCache.Replace` 显式丢弃全部内存缓存并重建。

## 5. SQLite 检查点与崩溃恢复

数据库包含：

- `meta`：工程版本、checkpoint schema、`committed_tick`。
- `route_mapping`：每个已提交时刻、请求/键/服务/规则/侧/桶和完整决策 JSON。
- `mirror_queue_snapshot`：每个时刻、每服务的等待/活跃影子调用状态。

每个逻辑时刻的写入顺序固定为：

1. 提交 `route_mapping`；
2. 提交 `mirror_queue_snapshot`；
3. 更新 `meta.committed_tick`。

如果进程在第 3 步前被 `SIGKILL`，全局时刻仍指向上一时刻。重启时 `Recover` 删除所有 `tick > committed_tick` 的孤儿分流和队列状态，再从 `committed_tick+1` 续跑。若第 3 步已完成，重启直接接受该时刻。

打开检查点时先做 SQLite header 与 `PRAGMA integrity_check`。截断/损坏返回 `E_CKPT_TRUNCATED`；工程版本不匹配返回 `E_VERSION_MISMATCH`。恢复不会把版本不匹配的检查点局部并入新工程。

真实杀进程脚本：`scripts/recovery_check.sh`。它分别在 `after-routes`、`after-queues`、`before-commit`、`after-commit` 点发送 `SIGKILL`，再重启到 tick 7。

## 6. 稳定业务码

| 业务码 | 含义 |
| --- | --- |
| `E_FIELD_AMBIGUOUS` | 无法消歧的字段组合或双刻度不一致 |
| `E_FIELD_MISSING` | 必填字段缺失 |
| `E_SCALE_RANGE` | 比例/采样模数越界或为非法零值 |
| `E_SCHEMA_UNSUPPORTED` | 未知 schema |
| `E_SERVICE_MISSING` | 规则/镜像/拓扑指向缺失服务 |
| `E_RULE_OVERLAP` | 同优先级同声明次序且匹配条件重叠，或严格模式发现重叠 |
| `E_WORKER_BUDGET` | worker 预算参数非法 |
| `E_WORKER_BUDGET_EXHAUSTED` | 显式非阻塞获取 worker 许可失败 |
| `E_ROUTE_BUFFER_FULL` | 每服务待路由缓冲耗尽 |
| `E_SHADOW_BUFFER_FULL` | 每服务影子缓冲耗尽 |
| `E_CKPT_TRUNCATED` | 检查点截断或完整性检查失败 |
| `E_VERSION_MISMATCH` | 检查点工程版本与当前快照不一致 |

普通的多规则重叠不是错误：只要优先级或声明次序不同，系统会固定裁决并记录候选链。`E_RULE_OVERLAP` 针对的是同优先级、同声明次序且可同时命中的不可裁决组合，或严格注入模式下的重叠。

## 7. 常用命令

```bash
go run ./cmd/grayrelease gen --dir samples
go run ./cmd/grayrelease demo --samples samples --out artifacts/demo-report.json
go run ./cmd/grayrelease simulate \
  --project samples/project-v2.json \
  --requests samples/requests-v2.json \
  --out artifacts/simulation-v2.json
go run ./cmd/grayrelease parallel-check --samples samples
go run ./cmd/grayrelease backpressure-demo --samples samples --route-buffer 2 --workers 4
go run ./cmd/grayrelease incremental-demo --samples samples
bash scripts/recovery_check.sh
bash scripts/verify.sh
```

异常注入：

```bash
go run ./cmd/grayrelease inject --kind overlap --workdir artifacts/inject
go run ./cmd/grayrelease inject --kind bad-scale --workdir artifacts/inject
go run ./cmd/grayrelease inject --kind missing-service --workdir artifacts/inject
go run ./cmd/grayrelease inject --kind truncated-checkpoint --workdir artifacts/inject
go run ./cmd/grayrelease inject --kind version-mismatch --workdir artifacts/inject
```

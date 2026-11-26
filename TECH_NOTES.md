# DNS 缓存与转发工作台技术说明

本仓库从空目录建立，模块名为 `dnslab`。它同时提供：

- 可集成的 Go 领域包；
- 固定种子的三版解析工程、查询样本与真值生成器；
- 纯 Go SQLite 检查点（`modernc.org/sqlite`，已写入 `vendor/`）；
- 基于标准库 `net/http` 的本地诊断工作台；
- CLI 自检与确定性样本落盘命令。

不依赖真实 DNS、外部站点、外部账号或 Docker。

## 快速开始

```bash
# 完整验证：vet + 全部测试 + CLI 自检
make verify

# 仅运行测试（vendor 模式）
go test -mod=vendor -count=1 ./...

# 生成 samples/projects/*.json、queries.json、truth.json
make samples

# 启动工作台，默认 http://127.0.0.1:8080
make serve

# 无浏览器端到端自检
go run -mod=vendor ./cmd/dns-workbench --selfcheck
```

也可以构建本地二进制：

```bash
make build
./bin/dns-workbench -addr 127.0.0.1:8080 -db .runtime/dns-checkpoint.sqlite
./bin/gen-samples -seed 42 -out samples
```

仓库包含 `vendor/modules.txt`，所有命令均显式使用 `-mod=vendor`；`Makefile` 也导出 `GOFLAGS=-mod=vendor`。

## 目录结构

- `spec/`：schema 无关的工程、区域、规则、上游、缓存种子、查询与真值模型。
- `fixedint/`：固定位宽（声明为 64 位毫秒时间戳）检查算术、秒/毫秒换算、溢出与饱和。
- `codes/`：跨领域、HTTP、检查点和工作台稳定错误码。
- `generator/`：固定种子生成三版 JSON 文档、13 条查询和独立真值。
- `loader/`：三版 schema 检测、字段映射、单位归一、引用与范围校验。
- `engine/`：最长后缀路由、优先级/声明次序裁决、缓存 TTL、模拟上游、并发预算、增量闭包。
- `editor/`：深拷贝编辑、参数校验和异常注入定义。
- `checkpoint/`：`database/sql` + `modernc.org/sqlite` 检查点。
- `service/`：工作台/CLI 共用应用门面。
- `workbench/`：HTTP API 与 `go:embed` 单页诊断台。
- `cmd/dns-workbench/`：服务与 `--selfcheck` 入口。
- `cmd/gen-samples/`：样本落盘工具。
- `samples/`：已生成的三版工程、查询、真值和规范化工程。

## 三版 Schema 映射

同一个种子 `42` 生成同一规范工程，并序列化为：

1. `dns-project/v1-flat-literal-seconds`
   - snake_case 旧字段；
   - 扁平 `flat_rules`；
   - 规则使用单个 `authority` 字面量；
   - TTL/超时使用整数秒；
   - 负缓存使用 `negative_ttl_seconds`。
2. `dns-project/v2-flat-list-millis`
   - camelCase 字段；
   - 有序 `upstreams` 列表，规则使用 `upstreamIndex`；
   - 扁平 `rules`；
   - TTL/超时使用剩余毫秒。
3. `dns-project/v3-tree-absolute-negative`
   - `metadata/settings` 嵌套命名；
   - `regionTree` 按区域嵌套，节点内规则后递归子节点做 DFS 扁平化；
   - `upstreamPool` 有序上游；
   - 缓存使用绝对 `expiresAtMs` / `negativeExpiresAtMs`；
   - 显式负缓存配置。

v1 的上游序号不能依赖 JSON 对象顺序。装载器按“规则声明顺序中首次出现 authority 字面量，并立即递归登记其 `next_authority`”重建有序上游，因此与 v2/v3 列表序号一致。

`TestThreeSchemaEquivalenceAndTruth` 使用 `reflect.DeepEqual` 断言三版装载后的 `spec.Project` 完全相同，并逐查询比对真值。

## 路由裁决

候选规则必须满足查询名等于后缀或是后缀的子域。排序键固定为：

1. 后缀标签数降序（最长后缀）；
2. 优先级降序；
3. 声明序号升序。

输出包含：

- `rule_number`、`rule_ordinal`；
- `decision_basis`：`longest_suffix`、`priority_desc` 或 `declared_order_asc`；
- 排序后的全部 `candidate_rules`；
- 选定上游序号。

生成样本包含普通最长匹配、同后缀优先级覆盖、同优先级声明次序三组对照。正常重叠规则可裁决；若注入同后缀、同优先级且声明位置冲突的规则，返回 `E_RULE_OVERLAP`。

## 缓存、TTL 与模拟上游

所有过期判断使用查询自带的 `AtMS` 逻辑时钟，不使用真实挂钟判断 TTL 或超时。

- 正缓存新鲜：`positive_cache_hit`，输出排序记录和每条 `remaining_ttl_ms`。
- 负缓存新鲜：`negative_cache_hit`，`code=OK_NEGATIVE`，不会作为 A/正向记录返回。
- 条目过期：删除过期段并访问模拟上游，来源标记为 `positive_cache_expired_refresh`。
- 未命中：访问模拟上游，来源为 `upstream_response`。
- 秒 → 毫秒使用检查乘法；绝对时刻 = 参考时刻 + 剩余 TTL 使用检查加法；溢出返回 `E_INT_OVERFLOW` / `E_TTL_OVERFLOW`，饱和结果带 `saturated:true`。

模拟上游的延迟是声明的整数 `latency_ms`。当 `latency_ms > timeout_ms` 时直接返回 `E_UPSTREAM_TIMEOUT`，不会 sleep 等待。转发路径记录 `hop_path` 与虚拟耗时；重复访问上游返回 `E_UPSTREAM_FORWARD_CYCLE`，达到 `max_hops` 后仍需转发返回 `E_UPSTREAM_HOPS_EXCEEDED`。

应答记录按 `Name/Type/Value/TTL` 确定性排序。每个样本的缓存键和逻辑时间固定；并发测试以 worker 1/2/4/8 各重复多轮，逐查询语义字段仍与真值一致。

## 并发预算与背压

`engine.Budget` 使用容量为 worker 数的信号量：

- `TryAcquire()` 有空位立即占用；
- 预算耗尽立即返回 `E_BACKPRESSURE_WORKER_BUDGET`，上下文带 `worker_budget`、`in_flight`、`retryable=true`；
- 不会增加隐式 goroutine 或静默丢弃任务。

`RunSamples` 使用固定 worker 池，结果按样本序号回填，因此完成顺序不影响输出顺序。报告记录 `wall_elapsed_ns` 和 `wall_elapsed_ms` 作为真实耗时观测；TTL/超时仍不受墙钟影响。

## 增量失效与传播闭包

`engine.Reconcile` 从旧工程缓存状态构建依赖图：

- 对每个缓存段中的键重新路由，候选链上的每个区域都有边 `候选区域 -> 缓存段区域`；
- 同时为每条规则的后缀构造合成路由边，使尚未生成缓存条目的规则区域也在闭包内；
- 编辑规则直接定位规则区域；
- 编辑上游定位所有引用该上游的规则区域；
- 负缓存开关定位含负缓存条目的区域；
- 从直接区域做 BFS，得到传播闭包。

闭包内段从新工程声明种子重建，闭包外段通过快照深拷贝复用，修订号保持不变。`InvalidationReport` 输出：

- `sources`：编辑来源、字段、前后值、直接区域；
- `direct_regions`、`propagation_closure`；
- `rebuilt_regions`、`reused_regions`；
- 每查询 `QueryImpact`：是否失效、依据来源、闭包、修订前后值。

增量结果会与 `NewEngine(next)` 的一次性全量重算逐查询比较。整份快照替换先由 `loader.Load` 识别 schema；若规范化指纹相同，则标记 `schema_only=true`、语义变更为 false、闭包为空。

## SQLite 检查点

检查点表：

- `meta`：`committed_seq`、`project_version`、`schema_version`；
- `cache_segments`：区域缓存段 JSON、revision、built time；
- `pending_fetches`：待回源队列；
- `committed_queries`：已提交序号和结果 JSON。

`Store.Save` 在同一事务内按以下顺序写入：

1. `cache_segments`；
2. `pending_fetches`；
3. `committed_queries`；
4. 最后更新 `meta.committed_seq/project_version/schema_version`。

事务提交前强杀进程只会回滚整笔事务；提交后恢复到完整提交点，不会出现全局序号领先于缓存段的已提交状态。

恢复时先执行 `PRAGMA integrity_check`，再比较 `project_version`：

- 版本匹配：恢复区域缓存段；
- 版本不匹配：`E_CHECKPOINT_VERSION_MISMATCH`，工作台可调用 recover 从起点整体重算；
- 短文件/截断：`E_CHECKPOINT_TRUNCATED`；
- 其他损坏：`E_CHECKPOINT_CORRUPT`；
- 查询最大序号超过全局序号：`E_CHECKPOINT_SEQUENCE_INVALID`。

SIGINT/SIGTERM 下，HTTP 服务先停止接收新请求，再把最近一个完整报告保存为检查点；被取消的并行批次返回 `E_INTERRUPTED`，不会把半成品写成新的提交点。

## HTTP API

- `GET /`：嵌入式工作台。
- `GET /api/state`：当前工程、样本、真值、最近失效报告。
- `GET /api/source?schema=...`：查看生成的原始 schema JSON。
- `POST /api/load`：重新装载某一版生成工程。
- `POST /api/run`：重置声明缓存并并行运行全部样本。
- `GET /api/query?sequence=N`：对当前引擎执行单条样本。
- `POST /api/edit`：应用规则、上游或负缓存编辑，返回闭包与全量对照。
- `POST /api/replace`：用 `{schema}` 或原始 `{document:{...}}` 整体替换快照。
- `POST /api/inject`：异常注入，始终返回稳定 `error.code` 与上下文。
- `POST /api/checkpoint/save`：保存检查点。
- `POST /api/checkpoint/restore`：仅恢复版本匹配检查点。
- `POST /api/checkpoint/recover`：匹配则恢复；版本不符则整体重算并返回对照。

## 稳定业务码

| 场景 | 业务码 |
| --- | --- |
| 缺少必填字段 | `E_FIELD_MISSING` |
| 字段类型/值不合法 | `E_FIELD_TYPE` / `E_FIELD_INVALID` |
| 不支持的 schema | `E_SCHEMA_UNSUPPORTED` |
| 多版本标记冲突 | `E_VERSION_CONFLICT` |
| 重复区域 | `E_REGION_DUPLICATE` |
| 未知区域 | `E_REGION_NOT_FOUND` |
| 冲突重叠规则注入 | `E_RULE_OVERLAP` |
| 失效上游引用 | `E_UPSTREAM_REFERENCE_INVALID` |
| TTL/超时越界 | `E_TTL_OUT_OF_RANGE` / `E_TIMEOUT_OUT_OF_RANGE` |
| 整数溢出 | `E_INT_OVERFLOW` / `E_TTL_OVERFLOW` |
| 线程/跳数预算越界 | `E_BUDGET_OUT_OF_RANGE` / `E_HOPS_OUT_OF_RANGE` |
| 无路由 | `E_ROUTE_NO_MATCH` |
| 未声明本地应答 | `E_ANSWER_NOT_DECLARED` |
| 转发循环/超跳/超时 | `E_UPSTREAM_FORWARD_CYCLE` / `E_UPSTREAM_HOPS_EXCEEDED` / `E_UPSTREAM_TIMEOUT` |
| worker 预算耗尽 | `E_BACKPRESSURE_WORKER_BUDGET` |
| 检查点截断/损坏/版本不符 | `E_CHECKPOINT_TRUNCATED` / `E_CHECKPOINT_CORRUPT` / `E_CHECKPOINT_VERSION_MISMATCH` |

所有业务错误均为 `codes.Error`，JSON 形态包含 `code`、`message`、可选 `field` 和可定位 `context`。

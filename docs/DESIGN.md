# 日志与崩溃恢复链路设计说明

本仓库交付一个可集成的 Go 领域组件和一个基于标准库 `net/http` 的本地诊断工作台。所有工程样本、三版 schema JSON、段文件和恢复真值均由固定种子 `20261127` 的确定性生成器产出；不访问真实存储服务、外部账号或网络资源。

## 1. 包结构

- `internal/model`：统一日志段、记录、检查点、恢复结果、预算、稳定错误码和定点容差。
- `internal/gen`：固定种子工程生成器与独立真值构造。
- `internal/loader`：`wal.v1`、`wal.v2`、`wal.v3` 装载/编码，JSONL 段文件和截断检查点文件解析。
- `internal/recovery`：序号校验、事务可见性、重放区间、依赖图、并行调度、增量缓存与失效闭包。
- `internal/checkpoint`：`database/sql` + `github.com/mattn/go-sqlite3` 的 SQLite 检查点存储和提交顺序协议。
- `internal/workbench`：HTTP API、异常注入、逻辑时刻服务和内嵌诊断页面。
- `internal/verify`：可重复的 19 项命令行验收并生成 `artifacts/verification-report.json`。
- `cmd/enginelab`：`serve` 与 `verify` 入口。

SQLite 驱动已 vendor 到 `vendor/github.com/mattn/go-sqlite3`，使用仓库内 amalgamation 经 CGO 构建；验证命令均带 `-mod=vendor`，可离线运行。

## 2. 三版 Schema 归一

同一份确定性工程被编码为：

| 版本 | 字段风格 | 序号基准 | 时间/大小 | 段元数据 | 检查点/依赖表达 |
| --- | --- | --- | --- | --- | --- |
| `wal.v1.flat-lsn-tick-bytes` | snake_case，如 `segment_id`、`record_type` | 绝对 LSN | tick、字节 | 扁平 `segments` 表 | 检查点作为 `checkpoint` 日志记录；显式 `predecessor_ids` |
| `wal.v2.flat-offset-ms-kib` | camelCase，如 `baseLsn`、`tsMs` | 段内相对 `offset` + `baseLsn` | 毫秒、KiB | 扁平 `segments` 表 | 无检查点日志条目，使用顶层 `checkpoint` 元数据；可选 `dependsOn` |
| `wal.v3.tree-offset-tick-kib` | 短字段，如 `seg_name`、`seq_off` | 段内相对 `seq_off` + `base_lsn` | tick、KiB | 嵌套 `segment_tree` | `checkpoint_marker`；省略显式依赖，由父子树边恢复前驱 |

归一后的核心不变量：

- 6 个段，ID 为 `seg-000` 到 `seg-005`，段索引和全局 LSN 顺序一致。
- 每个段包含 2 个提交事务、1 个回滚事务和 1 个未提交事务。
- `cp-1` 是 `seg-002` 尾部的真正检查点记录。
- 依赖边构成树：`seg-000 -> seg-001/seg-002`，`seg-001 -> seg-003`，`seg-002 -> seg-004 -> seg-005`。

装载器对缺失字段、未知 schema、冲突 schema 鉴别字段、冲突 LSN/offset、重复段和重复前驱引用返回 `*model.Error`，其中包含稳定 `Code`、`Locator` 和可选 `Details`。

## 3. 序号、事务与可见性

- V1 直接读取绝对 LSN；V2/V3 用 `base_lsn + offset` 归一。
- `Record.Offset` 同时保存相对基准，校验 `offset == lsn - base_lsn`。
- `ValidateSequence` 检查：
  - 记录 ID 全局重复：`E_DUP_RECORD`；
  - LSN 重复：`E_DUP_RECORD`；
  - 段内或全局 LSN 倒序：`E_SEQUENCE_ORDER`；
  - offset 与 base/LSN 冲突：`E_SEQ_BASE_CONFLICT`。
- `CompareVersionProjects` 对三版装载结果逐项比较段 ID/顺序、记录 ID、归一 LSN、操作、键值版本、依赖和检查点 LSN；跨版本基准注入会稳定得到 `E_SEQ_BASE_CONFLICT`。
- 重放时按事务缓冲 put/delete：
  - 只有收到 `commit` 才把缓冲写按 commit LSN 应用到数据项；
  - `abort` 丢弃缓冲；
  - 段尾仍 open 的事务按崩溃未提交处理，不进入可见集合；
  - 删除产生 tombstone 并从最终 `Items` 移除键。

真值由 `gen.BuildTruth` 独立按同一声明规则从生成工程构造，包含最终数据项、可见记录、提交/回滚/未提交事务、检查点状态和逐段范围。

## 4. 检查点范围、容差与依赖图

`recovery.BuildRanges` 以最新检查点 LSN 为上界：

- `lsn <= checkpoint_lsn` 的记录不进入物理重放，由检查点状态/缓存基线覆盖；
- 第一条 `lsn > checkpoint_lsn` 的完整记录是该段起点；
- 最后一条完整记录是终点；
- 完全位于检查点之前的段标记为 `skipped_checkpoint`，范围计数为 0，但仍输出边界点用于诊断；
- 样本的 `cp-1` 位于 `seg-002` 尾，因此首轮重放 `seg-003`、`seg-004`、`seg-005`。

`gen.Truth.Tolerance` 声明固定点容差。当前确定性转换全部整数且单位对齐，所以容差为 0；`model.RangeWithinTolerance` 仍提供 LSN、offset、index、tick、毫秒的定点比较入口。

依赖图方向为 predecessor -> dependent。`BuildGraph` 输出 parents/children/edges/topological order，并报告：

- 缺失前驱：`E_DEPENDENCY_MISSING`，locator 精确到 `segment.predecessor_ids[i]`；
- 环：`E_DEPENDENCY_CYCLE`，details 中给出规范化环路径。

## 5. 增量编辑与失效闭包

`recovery.Engine` 为每段保存完整本地重放报告和 FNV cache key。逻辑时刻固定为：

1. `baseline`：cp-1 基线；
2. `m1-truncate`：截断 `seg-004` 到前 5 条，移除 tx-b 的提交点及尾部；
3. `m2-overwrite`：覆盖 `rec-seg-003-a-put` 的值和版本；
4. `m3-checkpoint`：在截断后的 `seg-004` 尾锚定新增 `cp-2`；
5. `m4-dependency`：把 `seg-005` 前驱从 `seg-004` 改写成 `seg-003`。

失效规则：

- 普通段编辑：直接编辑段 + 依赖图中所有传递 downstream；
- 依赖改写：同时合并旧图和新图中该段的 downstream 闭包；
- 检查点新增：沿锚定段依赖闭包失效，并扩展检查点基线；
- 不在闭包中的段必须命中缓存；
- 每个重算报告带 `InvalidationSources`，包含编辑 ID、类型、直接段和传播路径。

测试强制验证每一步的 `CacheRecomputed` 精确集合，并将增量结果与编辑后工程的一次性 `RecoverOneShot` 比较数据项、范围、事务集合、段写集合和最后恢复点。耗时字段不参与比较。

## 6. 部分段损坏

物理段文件使用 JSONL：第一行是 `segment-header`，其后每行一条记录。

- 干净截断到 k 条：header 的 `record_count` 大于实际记录数，输出 `E_SEGMENT_TRUNCATED`；
- 最后一行不是合法 JSON：输出 `E_RECORD_INCOMPLETE`，跳过最后半条；
- 显式 `complete:false` 的记录同样在完整前缀处停止；
- 段状态为 `degraded`，并输出 `CompleteRecords`、`LastRecovered` LSN/offset/index/record ID。

因为提交记录完整时事务已经提交，损坏尾部不会丢弃已提交结果；如果提交记录本身缺失，事务保持 open，其缓冲写不会可见。验收测试构造了末行损坏，并断言最终数据项与真值完全一致。

## 7. SQLite 检查点和崩溃语义

表：

- `checkpoint_global(id=1)`：只有这里的 `committed_checkpoint_id` 和版本是重启入口；
- `checkpoint_batch`：批次状态；
- `segment_state`：段状态、范围、最后恢复点、cache key 和报告 JSON；
- `record_progress`：每段 applied count、last LSN/offset/index/record；
- `store_meta`：优雅退出标记。

提交顺序：

1. upsert `checkpoint_batch(status='writing')`；
2. 对每个段在一个事务内同时写 `segment_state` 和 `record_progress` 并提交；
3. 调用可选 crash hook；
4. 最后在一个全局事务中 upsert `checkpoint_global`、切换 `committed_checkpoint_id` 并把 batch 标记为 committed。

因此在步骤 4 之前发生任何强制终止，新批次的段行即使部分可见，也不会被 `LoadCommitted` 读取；重启只认上一个全局指针。`internal/checkpoint/store_test.go` 会重新执行测试二进制作为子进程，在 `before_global_commit` hook 中对自身发送 `SIGKILL`，随后验证父进程只能得到 `ErrNoCommittedCheckpoint`，完成续算并保存后可读取一致进度。

版本字段为 `recovery-checkpoint-v1`。不匹配时 `LoadCommitted` 返回 `E_VERSION_MISMATCH`；`Service.Resume` 和版本异常注入会清空内存检查点并从声明起点整份重放。

`SIGINT`、`SIGTERM`、`SIGHUP` 是可捕获信号：CLI 会停止接收新连接、drain HTTP、写 `graceful:<signal>` meta 后退出。`SIGKILL`/`SIGSTOP` 无法捕获，按上述全局指针协议做崩溃恢复，不写优雅退出标记。

## 8. 并行和预算

`model.Budget` 声明：

- `Workers`：合法范围 1..32，零值默认 4；
- `QueueDepth`：等待/排队容量，可为 0，最大 256；
- `MaxSegmentMemoryBytes`：单段重放内存上限，零值默认 16 MiB，硬上限 64 MiB。

调度容量为 `Workers + QueueDepth`。超出容量的段不会被放行，而是得到确定性的 `E_BACKPRESSURE` 诊断和 `state=rejected`。非法线程上限返回 `E_BUDGET_THREADS`；非法内存上限或段估算内存超限返回 `E_BUDGET_MEMORY`。

并行实现用 buffered semaphore 限制 worker，互斥锁保护报告 map；结果最后始终按规范段序聚合。测试覆盖 workers=1/2/4/8、正向/反向调度、batch=1/2/64，并使用 `-race` 验证。每段记录真实 `ElapsedNanoseconds`，UI 显示毫秒观测值；比较器和 cache key 均排除耗时。

## 9. HTTP 工作台

启动：

```bash
make serve
# 或 go run -mod=vendor ./cmd/enginelab serve -addr 127.0.0.1:8088
```

主要 API：

- `GET /`：内嵌单页；
- `GET /api/health`；
- `GET /api/sample`：生成工程、三版原始 JSON 和真值；
- `GET /api/versions`：装载并比较三版；
- `GET /api/timeline`、`GET /api/timeline/{n}`：五个逻辑时刻；
- `POST /api/recover`：自定义 worker、队列、内存、调度方向和提交批次；
- `POST|GET /api/inject?kind=...`：异常注入；
- `POST /api/checkpoint/save/{n}`；
- `GET /api/checkpoint/restore`；
- `GET|POST /api/checkpoint/resume`：版本匹配走检查点，不匹配整份重放。

异常 kind 与代码：

| kind | 稳定码 |
| --- | --- |
| `duplicate_segment_ref` | `E_DUP_SEGMENT_REF` |
| `cross_version_base_conflict` | `E_SEQ_BASE_CONFLICT` |
| `duplicate_record` | `E_DUP_RECORD` |
| `sequence_order` | `E_SEQUENCE_ORDER` |
| `dependency_cycle` | `E_DEPENDENCY_CYCLE` |
| `dependency_missing` | `E_DEPENDENCY_MISSING` |
| `truncated_segment_file` | `E_SEGMENT_TRUNCATED`（可恢复） |
| `incomplete_record` | `E_RECORD_INCOMPLETE`（可恢复） |
| `truncated_checkpoint` | `E_CHECKPOINT_TRUNCATED`（可恢复并全起点重放） |
| `budget_threads` | `E_BUDGET_THREADS` |
| `budget_memory` | `E_BUDGET_MEMORY` |
| `backpressure` | `E_BACKPRESSURE` |
| `schema_version_mismatch` / `project_version_mismatch` | `E_SCHEMA_VERSION` |
| `version_mismatch` / `version_mismatch_snapshot` | `E_VERSION_MISMATCH` |

## 10. 实际验证

已执行：

```bash
gofmt -w cmd internal acceptance_test.go
go test -mod=vendor -count=1 ./...
go test -race -mod=vendor -count=1 ./...
go run -mod=vendor ./cmd/enginelab verify -out artifacts
```

结果：

- 普通全量测试：所有包 `ok`；
- race 全量测试：所有包 `ok`，无数据竞争；
- `verify`：`verification ok=true checks=19`；
- 强制 SIGKILL 子进程续算测试通过；
- 实际启动 HTTP 服务并 curl 验证 `/`、`/api/health`、`/api/versions`、`/api/timeline`、检查点保存/恢复和异常注入；
- `SIGTERM` 实测输出优雅退出并返回 exit code 0。

留存文件：

- `artifacts/verification-report.json`：19 项检查和异常码；
- `artifacts/go-test-output.txt`：普通测试输出；
- `artifacts/go-test-race-output.txt`：race 测试输出；
- `artifacts/verify-output.txt`：verify 命令输出；
- `artifacts/sample_truth.json`：固定种子真值；
- `artifacts/demo_summary.json`：样本计数和异常码摘要。

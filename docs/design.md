# LockLab 设计说明

## 1. 规范化模型

装载器的唯一目标模型是 `locklab.Project`：

```text
Project
  Items[]          (ID, Table, InitialValue)
  Transactions[]   (ID, Priority, Isolation, Operations[])
  Operation        (LocalSeq, AtMs, Kind, ItemID, Table,
                    Granularity, Mode, Value, WaitTimeoutMs, NoWait)
```

所有 schema 表达先归一化为该模型，再由 `ValidateProject` 检查：

- 数据项、事务 ID 唯一；
- `local_seq` 从 1 连续，事务内时间不倒退；
- 终态操作必须为最后一步；
- 行锁/读写引用已存在数据项，表锁引用已存在表；
- 锁操作显式声明模式、粒度、超时；
- worker/memory 预算在硬边界内；
- 超时相对值非负，绝对 deadline 不早于请求时刻。

`CanonicalDigest()` 只覆盖规范化模型，不覆盖源 schema 版本，因此三版装载的摘要可直接比较。

## 2. Schema 解析

JSON 先经过递归 token 扫描拒绝重复键，再检查顶层 schema 判别字段唯一性。每个版本使用 `DisallowUnknownFields` 解码，缺失必填字段、命名歧义、未知模式、非精确单位换算均给出 `Error{Code, Path, Message}`。

### v1

- snake_case；
- 顶层 `lock_modes` 是扁平模式/相容名单；
- 毫秒、字节、相对超时；
- 必选 `deadlock_detection`。

### v2

- camelCase；
- 秒与 KiB；
- `waitDeadlineSeconds` 是绝对时刻，装载时计算 `deadline - at` 并对倒序报错；
- 可选 `features.deadlockDetection`，缺失为 false。

### v3

- PascalCase；
- `LockCompatibility` 为递归树。组节点负责分区，模式节点下每个 `{Mode, Compatible}` 叶子表示一行相容关系；装载器展平后与唯一规范矩阵逐对比较；
- 可选顶层 `DeadlockDetection`，缺失为 false。

## 3. 锁仲裁

### 3.1 相容矩阵

|      | IS | IX | S | SIX | X |
|------|----|----|---|-----|---|
| IS   | Y  | Y  | Y | Y   | N |
| IX   | Y  | Y  | N | N   | N |
| S    | Y  | N  | Y | N   | N |
| SIX  | Y  | N  | N | N   | N |
| X    | N  | N  | N | N   | N |

表级与行级共同判定：

- 同表表锁之间使用矩阵；
- 同行行锁之间使用矩阵；
- 表 `X` 与任何子孙行锁冲突；
- 表 `S`/`SIX` 与子孙 `X`/`SIX` 行锁冲突；
- 表 `IS`/`IX` 是意向锁，不直接阻断其他事务的行锁。

### 3.2 调度、阻塞与唤醒

每个声明操作先获得稳定的全局逻辑步号。调度按时间、事务声明顺序、`local_seq` 排序。事务若在锁请求上阻塞：

1. 请求进入资源 FIFO；
2. 记录当前席位、冲突持有者、绝对 deadline；
3. 事务后续已经到达的步骤进入延迟列表；
4. 释放锁后只检查各队列头部，按资源名排序依次唤醒；
5. 被唤醒事务从原锁请求继续，再按本地顺序执行延迟步骤。

该顺序保证 worker 调度、batch 和事务提交批次不影响结果。

## 4. 可见性

每个 item 保存初始版本 0 和按提交追加的版本链。

- `READ_UNCOMMITTED`：读取写序号最大的未提交版本；若没有则读最新已提交版本；未提交他事务版本标记 dirty。
- `READ_COMMITTED`：读取当前全局提交序号之前的最新提交版本；S 锁只在本次读语句期间持有。
- `REPEATABLE_READ` / `SERIALIZABLE`：事务首操作记录快照提交序号，所有读使用该快照；S 锁持有到事务终态。
- 任意隔离级别下，本事务自己的最新 pending 写优先于快照。

提交时先增加全局提交序号，再把该事务全部 pending 写追加为带同一提交序号的新版本，最后释放锁并唤醒等待者。最终写入集合是每个 item 的最新非初始已提交版本。

异常不是查表：

- dirty read 来自实际读到未提交版本；
- non-repeatable read 来自同一事务对同一 item 观察到不同版本；
- nonserializable 来自 read-from、write-write 和 anti-dependency 图的实际环。

## 5. 超时与死锁

等待 deadline 统一为 `request.AtMs + timeoutMs`。推进时钟时，所有 `deadline <= now` 的等待先冻结并全部标记超时，再统一回滚，避免前一个超时释放锁“救活”同一时刻本应超时的后一个等待。

等待图边：

- 等待请求到所有冲突锁持有者；
- 非队首请求到同资源前序等待者。

DFS 找到环后，按 `(priority 升序, TxID 降序)` 选最低优先级受害者。受害者回滚、丢弃 pending 写、释放全部锁；若等待图无环则不回滚。`deadlock_detection=false` 时不执行此过程，等待最终由 timeout 收敛。

## 6. 增量失效

`BaselineTrace` 保存基线结果、调度和每步后状态快照。编辑只允许一个事务变化，项目配置/数据项和事务声明顺序保持稳定；编辑可改变隔离级别、锁模式、操作内容，也允许重排导致全局调度位置变化。

步骤：

1. 比较旧/新调度，找到首个结构性或语义性差异。
2. 从差异前一步的缓存状态恢复。
3. 用新项目调度继续执行；刷新未来逻辑步元数据和事务配置。
4. 编辑事务全部消费后，对每个边界比较运行状态指纹。
5. 若状态语义一致且调度尾部一致，拼接旧后缀；否则继续重算到终点。
6. 生成每步来源 `direct_edit` / `propagation` / `state_chain` / `prefix_cache` / `state_reconverged`。
7. 从实际等待快照提取受影响队列资源、步骤区间和等待事务。

运行状态指纹覆盖时钟、提交序号、事务状态、锁、等待队列、版本、读写事实和提交/回滚事实；排除可重放结果与工程摘要。

## 7. 并行预算

每个操作准备任务估算固定开销加字段长度。worker pool 数量不超过声明预算和硬上限，batch 按 shuffle seed 打乱后分发；准备阶段只验证内存估算和任务可执行性，不改变仲裁顺序。

失败返回：

- `E_BACKPRESSURE_WORKERS`：worker 为 0、超过硬上限或超过工程预算；
- `E_BACKPRESSURE_MEMORY`：每 worker 内存低于最小值，或单个操作估算超过预算。

锁仲裁仍由单一确定性流执行；`StepObservation` 记录真实 wall-clock 微秒值，不进入语义摘要。

## 8. 检查点协议

目录结构：

```text
checkpoint.commit
checkpoint-body-<generation>.json
checkpoint.commit.tmp        (仅写入过程中存在)
```

保存顺序：

1. 复制并冻结运行时状态；body 中的全局 `commit_seq` 置零。
2. 写入唯一 generation body，`fsync`，同步目录。
3. 构造 marker，包含格式版本、工程 ID/摘要、body SHA-256、`commit_seq`、最后提交步。
4. 写 marker 临时文件，`fsync`，原子 rename 为 `checkpoint.commit`，同步目录。
5. 清理旧 body。

恢复先读 marker，再校验版本、工程快照、body 校验和、等待图引用，最后把 marker 的提交序号写回状态。因此 body 已落而 marker 未发布的崩溃不会暴露未完成状态；marker 原子替换保证旧提交点仍可读。

版本/工程快照不匹配时 `ResumeOrRecompute` 从头完整重算；检查点截断、校验和错误或悬空等待图返回硬业务错误。

## 9. SQLite SQL 导出

由于 Go 标准库没有 SQLite 驱动，本地运行时使用上述崩溃安全文件格式。`ExportSQL` 生成：

```sql
BEGIN IMMEDIATE;
INSERT INTO tx_state ...;
INSERT INTO committed_step ...;
INSERT INTO lock_state ...;
INSERT INTO wait_graph ...;
INSERT INTO wait_queue ...;
INSERT INTO checkpoint_meta ...; -- 最后写全局提交序号
COMMIT;
```

这保持“事务与锁状态先落、全局提交序号最后更新”的语义，可在有 SQLite 的环境中执行/检查，但本仓库测试不依赖外部命令。

## 10. 错误码

错误对象统一为：

```json
{"code":"E_...","path":"$.transactions[1].operations[3]","message":"..."}
```

除验收矩阵中的码外，解析/执行还包括：

- `E_JSON_DUPLICATE_KEY`
- `E_SCHEMA_VERSION_MISSING`
- `E_SCHEMA_UNSUPPORTED`
- `E_SCHEMA_AMBIGUOUS`
- `E_SCHEMA_UNKNOWN_FIELD`
- `E_FIELD_REQUIRED`
- `E_UNIT_AMBIGUOUS`
- `E_LOCK_MODE_UNKNOWN`
- `E_LOCK_MODE_COMPAT_MISMATCH`
- `E_GRANULARITY_INVALID`
- `E_ISOLATION_UNKNOWN`
- `E_OPERATION_INVALID`
- `E_OPERATION_ORDER`
- `E_INVALID_EDIT`
- `E_INTERRUPTED`
- `E_TRUTH_MISMATCH`

所有异常注入只对 clone 或检查点副本操作。

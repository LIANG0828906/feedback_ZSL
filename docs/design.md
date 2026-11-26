# 设计说明

## 1. 规范模型

装载后的语义模型为 `model.Project`：

```text
Project
  ID, Name, Seed
  Bases[]  {ID, SizeBytes, Digest}
  Steps[]  {Number, Name, Inputs[], CommandFingerprint, DependsOn[], Output}
  Shares[] {LayerID, Consumers[]}
  Budget   {Latency, MaxRounds, ThreadLimit, MemoryPerThread, BufferPerThread}
```

三版文档中所有字段、层级和单位都映射到该模型。`model.SemanticHash` 对规范化后的模型做 SHA-256，用于证明三版一致；它不包含 schema 名称、描述性别名或单位原文。

## 2. 严格装载

`internal/loader` 先用 `json.Decoder.UseNumber` 解码为对象树，然后：

- 通过顶层 `schema` 或 `schemaVersion` 选版本；
- 每一层对象核对精确字段白名单；
- 必填字段、数值类型、单位逐项校验；
- v2 的 `layerTree` 递归展开，节点不能同时有 `layer` 和 `children`；
- 所有基础层和步骤输出必须恰好被层元数据覆盖一次，尺寸和 producer 必须一致；
- 秒以 `math/big.Rat` 换算为毫秒，避免浮点误差；
- KiB 只允许整数，换算为字节后进入模型。

任何失败都带 `apperr.Error`，包含稳定 `Code`、`Path`，适用时带 `Steps`/`Layers`。

## 3. 层图与共享校验

层 ID 空间同时包含基础层和步骤输出层。步骤依赖一个层时形成“依赖层 producer -> 当前步骤”的边。校验包括：

- 层 ID、步骤号唯一；
- 输入路径和摘要不能重复/冲突；
- 未知依赖返回 `E_GRAPH_MISSING_REF`；
- 共享声明的消费者集合必须等于实际依赖该层的步骤集合；
- 实际消费者超过一个但未声明共享，返回 `E_GRAPH_SHARE_MISSING`；
- Kahn 拓扑排序检测环，失败时 DFS 找出环中步骤号。

拓扑顺序是调度顺序的确定性基准，但缓存键和层摘要不包含拓扑位置。

## 4. 缓存键与层摘要

实现使用长度前缀编码，避免字段拼接歧义。缓存键字段顺序固定：

1. 算法名；
2. 步骤名；
3. 命令指纹；
4. 按路径排序的输入 `(path, digest)`；
5. 按层 ID 排序的依赖 `(layerID, contentDigest)`。

依赖摘要使用递归拓扑得到的最终内容摘要，因此上游编辑会自然改变下游键。层摘要再加入输出层 ID 和尺寸。

生成器真值由同一确定性规则产出，但测试会从三个独立装载结果重新派生并逐步骤、逐层比较。

## 5. 调度

每一轮从尚未构建的步骤中收集 ready 集合。一个 miss 步骤只有在所有 producer 依赖已命中或本轮之前完成时才 ready。

工作线程初始负载为零。每个候选按编号顺序尝试放入“放入后不超过延迟预算”的负载最小线程；找不到线程则延后。容量 `workers * buffer` 满后，其余 ready 层记录延后原因和位次。

并行执行使用有界 worker goroutine，每个 worker 从 jobs channel 消费。所有结果按步骤号排序后写入快照。cache key、digest 和大小均来自派生前置数据，不依赖 goroutine 完成次序。

### 降级

- 内存不足：从触发轮开始采用单线程 `serial-streaming`，报告中 `MemoryBytes <= budget.MemoryPerThread`，并标注所有触发层；
- 单层延迟不可满足：采用 `uninterruptible-single-layer`，只推进一个层并标注延迟溢出；
- 轮数不足：在第 `MaxRounds` 轮后加入 `batch-continue`，后续轮仍受单轮延迟预算约束；
- 缓冲非法为零：立即返回 `E_BACKPRESSURE_QUEUE`；
- 调度中缓冲受限：记录 `BackpressureEvent`，不阻塞调用方。

## 6. 检查点

SQLite 文件由 `internal/sqlitefile` 自写。写步骤：

1. 生成四张表的内存行集；
2. 序列化 `sqlite_master` 和表叶页；
3. 写同目录临时文件并 `fsync`；
4. 原子 rename；
5. 对目录 `fsync`。

`checkpoint.Store` 在物理写入前执行：

- 每轮先更新 layer map；
- 清理该 layer 的旧索引；
- 添加新索引；
- 校验索引与 committed layer 双向一致；
- 校验轮次连续且 `last_round == len(rounds)`。

由于新快照一次性原子替换，索引不会指向半轮 layer；rename 前失败保留旧快照，rename 后失败保留新快照。

## 7. 恢复

启动时：

- 无文件：从空快照 + 当前基础层开始；
- 文件截断/损坏：返回稳定错误，不修改文件；
- 检查点版本不匹配：重置为空快照并全量重建，结果记录 `Recovery`；
- 工程 ID 不匹配：返回 `E_RECOVERY_MISMATCH`；
- 正常：只从持久化 layer/index/round 恢复命中，不推测未提交内存状态。

信号取消只在轮次提交后检查。显式 `InterruptAfterRound` 使用同一路径，便于确定性测试。

## 8. 增量与清理

增量需要调用方提供上一个工程模型。引擎比较字段级差异，并沿当前 DAG 的依赖方向传播失效来源。最终仍用当前派生摘要校验已持久化层。

清理候选必须是终止层。引擎先检查所有候选：

- 基础层、未知层、有消费者的层都算冲突；
- 任一冲突则不提交任何删除；
- 全部合法时先删索引、再删层状态、最后原子提交。

因此共享依赖不会因部分清理而消失，源工程文档也不会被改写。

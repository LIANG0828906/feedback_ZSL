# 架构与验收说明

## 1. 数据流

```text
固定种子
  -> Generator
     -> Level（规则分块、吸收表、反射面、源、听者）
     -> Truth（独立解析传播 + 镜像路径长度）
  -> ValidateLevel
  -> Engine
     -> buildEnergyPlan（波次传播、Transfer/Escape）
     -> 并行 SolveChunk（路径 -> 权重 -> 最大余数能量分配）
     -> CheckConservation
     -> 稳定提交序 -> SQLite 单块事务
  -> VerifySnapshot（独立读取持久化结果）
```

生成器真值的能量部分使用 `generateTruth` 自己的传播循环，路径部分使用 `analyticChunkPaths`；两者都不调用求解器的 `SolveChunk`/`buildEnergyPlan`。求解器与真值共享的只有坐标、距离、线面求交等数学原语。

## 2. 默认样本

`Generate(seed)` 固定生成：

- 12 m × 10 m 平面关卡；
- 6 列 × 5 行，每块 2 m × 2 m；
- 3 个频带：125、500、2000 Hz；
- 每块 4 条边界反射面；
- 每块 1 个声源、1 个听者；
- 吸收系数范围 `[8000, 36000) / 65536`，严格为正且小于 1；
- 声源逐频带能量约 3.0M–4.0M；
- 最多 2 次反射、3 轮块传播。

所有随机数来自自实现 SplitMix64，不依赖 Go 标准库随机实现版本。

## 3. 能量公式

对声源 `s`、频带 `b`、块 `q` 的某个到达包：

```text
captured = floor(input * (1 - absorption[q,b]))
remainder = input - captured
share[4] = 精确整数四分 remainder
```

四向顺序固定为 `left,right,bottom,top`；余数优先给前几项。内部邻居生成传递，关卡边界或轮次上限生成逃逸。

路径分配：

```text
weight_i = (absorption^reflection_count * distance_gain_i) in Q16
alloc_i  = largest_remainder(total_captured, weight_i)
```

这保证 `Σ alloc_i = total_captured`，且分配顺序稳定。

## 4. 状态和提交

分块状态机只接受：

```text
pending_filtering -> path_filtering
path_filtering    -> energy_transfer | failed
energy_transfer   -> committed | failed
failed            -> path_filtering
committed         -> pending_filtering  (仅失效)
```

回合状态机只接受：

```text
pending_rebuild -> rebuilding
rebuilding      -> committed | failed
failed          -> rebuilding
```

任何其他迁移返回 `E_STATE`，状态和已提交计数均不变化。

提交阶段使用边界图 SCC 缩略图拓扑序；网格通常是一个大 SCC，因此块号排序是环内稳定次序。每个 `CommitChunk` 事务要求 `sequence == snapshots.next_sequence`，成功后递增。

## 5. 缓存和失效

分块缓存键：

```text
SHA256(
  chunk_content_hash,
  params_version,
  algorithm_version,
  sorted(dependencies)
)
```

参数版本只包含会影响数值结果的选项。`MaxParallel` 与 `QueueBudget` 属于调度预算，刻意不进入参数版本，因此调整并行度不会让 SQLite 快照失效。

依赖至少包括：

- `chunk_content`；
- `parameters`；
- `algorithm`；
- `source_emission`；
- 每条入边 `boundary_transfer`，其 `CacheKey` 由上游内容哈希、传递版本和波次组成。

编辑后从种子块沿边界图 BFS，最大传播轮次即失效半径。编辑事务删除受影响块持久化行、清空旧提交序列并更新关卡哈希/真值指纹；未受影响块仍可作为恢复输入。

## 6. SQLite 事务边界

表：

- `snapshots`：快照身份、版本、真值指纹、最后回合、下一提交序号；
- `chunk_results`：每块完整 JSON 结果及独立校验列；
- `commit_log`：回合内提交序、块号、是否复用。

连接使用单连接与互斥锁，事务外再串行化调用；每块提交同时写 `commit_log` 和 `chunk_results`，因此不会出现“记录已提交但没有结果”或相反状态。

恢复校验：

1. 快照参数/算法/真值指纹必须一致；
2. 每个持久化块的内容哈希必须与当前关卡一致；
3. 路径 JSON 的摘要必须等于持久化摘要；
4. 任一失败则删除快照全部块和提交日志，回到待重建状态。

## 7. 独立复核协议

复核器不依赖求解器，只做以下重算：

- 从每块 `SourceEnergy` 汇总回 `ArrivalEnergy`；
- 汇总所有 `Outgoing`，并要求每个摘要都在某个 `Incoming` 中出现且成对；
- 从 `TruthSources.Emission`、持久化块能量、边界净流与 `Escapes` 重算守恒；
- 用 `Truth.Paths` 校验路径长度容差；
- 用 `Truth.Listeners` 校验听者总能量；
- 校验最新回合提交序是否从 0 连续。

`VerifySnapshot` 的字段指纹来自持久化的块能量和路径；三路径比较则逐字段比较能量、路径、入边/出边、逃逸、守恒和提交序。

## 8. 实际验收记录

本机执行：

```bash
go version
# go version go1.22.12 linux/amd64

go test ./... -count=1
# ok  	acousticfield
# ?   	acousticfield/cmd/acoustic [no test files]

go run ./cmd/acoustic demo -seed 20261126 -workers 4 -abort-after 25
```

演示样本 30 块、3 频带。三路径结果一致，最大守恒残差为 0，独立复核通过。测试覆盖：

- 固定种子真值稳定性；
- 逐块逐频带能量与真值；
- 路径长度容差与确定性；
- 旋转反射面的任意法线求交；
- 编辑后失效范围与未受影响缓存复用；
- 全量/增量/断点恢复三路径一致；
- 取消后只恢复已提交块；
- 编辑后重启保留未受影响检查点；
- 单块路径缓冲失败隔离和重试；
- 队列预算等待记录；
- SQLite 提交序号、内容不匹配整体作废；
- 独立复核和首个差异 `E_DIFF`；
- HTTP 诊断页与 JSON API。

